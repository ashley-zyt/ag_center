package main

// 任务记录持久化 —— 把内存里的 taskRecords 落到本地 JSON 快照，
// 避免服务重启/被 kill 之后任务进度全部清零。
//
// 设计取舍：
//   - 用 JSON 快照而不是 SQLite：本项目依赖极简（只有 chromedp），引入数据库驱动会显著
//     增加编译体积与部署复杂度；而任务记录本来就只在内存里查询与聚合，全量加载是既有做法。
//   - 状态变更只置「脏」标记，由后台协程按 taskStoreFlushInterval 合并落盘：一次任务执行
//     会变更 2~3 次状态（queued→running→success/failed），合并写入可避免频繁整文件重写。
//   - 先写临时文件再 rename，保证不会留下写到一半的坏快照。
//   - 重启加载时，快照里仍为 queued/running 的任务必然是「孤儿」——执行它的 goroutine 已随
//     进程消失、不可能再推进，因此统一标记为 interrupted，而不是伪装成还在跑。
//
// 结构（schema v2）：
//   - 主快照 data/tasks.json 只保留「活跃」记录：未完成（queued/running/interrupted）与
//     最近完成（updated_at 在归档窗口内）的终态。高频落盘只写这个小文件。
//   - 更早的终态记录按月归档到 data/archive/tasks-<YYYYMM>.json，历史不丢、主文件瘦身。
//     归档写入由「归档集合签名变化」触发：任何时刻一条记录要么在主快照、要么在归档，
//     且先写归档再写主快照，崩溃最坏只出现「两边都有」（加载按 taskID 去重），不会丢。
//   - 主快照/归档文件都内嵌 SHA256 校验和（覆盖 tasks 数组），加载时校验完整性。
//   - 主快照每隔一段时间滚动备份到 data/backups/，保留最近 N 份，供写坏/误删回滚。
//
// 环境变量：
//
//	TASK_STORE_ENABLED       默认 true；设 false/0 退回纯内存
//	TASK_STORE_DIR           快照目录，默认 data（相对工作目录，与 logs/ 同级）
//	TASK_RETENTION_DAYS      总保留天数，默认 30；设 0 表示永久保留
//	TASK_STORE_ARCHIVE_DAYS  终态归档窗口天数，默认 7；设 0 表示不归档（全部留在主文件）
//	TASK_STORE_BACKUP_KEEP   滚动备份保留份数，默认 5；设 0 表示不做备份
//	TASK_MAX_RECORDS         记录条数上限，默认 0（不限）；超限时优先丢弃最旧的终态记录

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"minimax_pro/internal/logx"
)

const (
	// taskStoreSchema 快照格式版本号，v2 引入 checksum 与归档分层。
	taskStoreSchema = 2
	// taskStoreFileName 主快照文件名。
	taskStoreFileName = "tasks.json"
	// taskStoreFlushInterval 状态变更后的主快照合并落盘窗口。
	taskStoreFlushInterval = 2 * time.Second
	// taskStoreMaintenanceInterval 低频维护周期（内存清理 + 滚动备份）。
	taskStoreMaintenanceInterval = time.Hour
	// taskStoreBackupInterval 两次滚动备份之间的最小间隔。
	taskStoreBackupInterval = time.Hour
)

var (
	taskStoreEnabled  = true
	taskStoreDir      = "data"
	taskRetentionDays = 30
	taskArchiveDays   = 7
	taskBackupKeep    = 5
	taskMaxRecords    = 0
	taskStorePath     string
	taskArchiveDir    string
	taskBackupDir     string

	taskStoreDirty     int32 // 1 表示主快照有未落盘的变更
	taskStoreSaveMu    sync.Mutex
	taskStoreErrMu     sync.Mutex
	taskStoreLastError string

	archiveSignature string // 上次写归档时的归档集合签名，空表示从未写过
	lastBackupAt     time.Time
	lastBackupMu     sync.Mutex
)

func init() {
	if v := strings.TrimSpace(os.Getenv("TASK_STORE_ENABLED")); v == "false" || v == "0" {
		taskStoreEnabled = false
	}
	if v := strings.TrimSpace(os.Getenv("TASK_STORE_DIR")); v != "" {
		taskStoreDir = v
	}
	if v := strings.TrimSpace(os.Getenv("TASK_RETENTION_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			taskRetentionDays = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("TASK_STORE_ARCHIVE_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			taskArchiveDays = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("TASK_STORE_BACKUP_KEEP")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			taskBackupKeep = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("TASK_MAX_RECORDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			taskMaxRecords = n
		}
	}
	taskStorePath = filepath.Join(taskStoreDir, taskStoreFileName)
	taskArchiveDir = filepath.Join(taskStoreDir, "archive")
	taskBackupDir = filepath.Join(taskStoreDir, "backups")
}

// taskStoreFile 快照文件结构。Tasks 按 CreatedAt 升序存放，便于人工查看与增量比较。
type taskStoreFile struct {
	Schema   int           `json:"schema"`
	SavedAt  time.Time     `json:"saved_at"`
	Checksum string        `json:"checksum,omitempty"` // 覆盖 tasks 数组的 SHA256（十六进制）；旧文件无此字段
	Tasks    []*TaskRecord `json:"tasks"`
}

// ===== 校验和 =====

// computeTasksChecksum 计算 tasks 数组的 SHA256（十六进制）。
// tasks 数组由结构体字段顺序固定的 TaskRecord 组成，序列化结果确定，可稳定复算。
func computeTasksChecksum(tasks []*TaskRecord) string {
	data, err := json.Marshal(tasks)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ===== 保留期 / 归档窗口 =====

// taskRetentionCutoff 返回保留窗口的起始时间；零值表示不过期（永久保留）。
func taskRetentionCutoff(now time.Time) time.Time {
	if taskRetentionDays <= 0 {
		return time.Time{}
	}
	return now.AddDate(0, 0, -taskRetentionDays)
}

// archiveCutoff 返回归档窗口的起始时间；零值表示不启用归档。
// 终态记录 updated_at 早于该时间即应移入归档。
func archiveCutoff(now time.Time) time.Time {
	if taskArchiveDays <= 0 {
		return time.Time{}
	}
	return now.AddDate(0, 0, -taskArchiveDays)
}

// filterExpired 过滤掉 created_at 早于保留窗口的记录（供落盘前裁剪）。
func filterExpired(tasks []*TaskRecord, now time.Time) []*TaskRecord {
	cutoff := taskRetentionCutoff(now)
	if cutoff.IsZero() {
		return tasks
	}
	kept := make([]*TaskRecord, 0, len(tasks))
	for _, rec := range tasks {
		if !rec.CreatedAt.Before(cutoff) {
			kept = append(kept, rec)
		}
	}
	return kept
}

// isArchivable 判断一条记录是否应移入归档：仅终态（success/failed）且 updated_at 早于归档窗口。
// 未完成（queued/running/interrupted）永远留在主快照。
func isArchivable(rec *TaskRecord, cutoff time.Time) bool {
	if cutoff.IsZero() {
		return false
	}
	switch rec.Status {
	case taskStatusSuccess, taskStatusFailed:
	default:
		return false
	}
	return rec.UpdatedAt.Before(cutoff)
}

// splitActiveArchive 把全量记录拆成「主快照」与「归档」两组。
func splitActiveArchive(tasks []*TaskRecord, now time.Time) (active, archive []*TaskRecord) {
	cutoff := archiveCutoff(now)
	for _, rec := range tasks {
		if isArchivable(rec, cutoff) {
			archive = append(archive, rec)
		} else {
			active = append(active, rec)
		}
	}
	return active, archive
}

// ===== 归档文件 =====

func archiveMonth(t time.Time) string { return t.Format("200601") }

func archiveFilePath(month string) string {
	return filepath.Join(taskArchiveDir, "tasks-"+month+".json")
}

// archiveSetSignature 计算归档集合的确定性签名：按 taskID 排序后依次喂入哈希。
// 用于判断归档集合是否变化，避免每次落盘都重写归档文件。
func archiveSetSignature(archive []*TaskRecord) string {
	ids := make([]string, 0, len(archive))
	for _, rec := range archive {
		ids = append(ids, rec.TaskID)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeArchiveFiles 把归档记录按月写入 data/archive/tasks-<YYYYMM>.json。
// archive 为空时清空归档目录里的旧月份文件。调用方须保证传入的 archive 已过保留期裁剪。
func writeArchiveFiles(archive []*TaskRecord, now time.Time) error {
	if err := os.MkdirAll(taskArchiveDir, 0o755); err != nil {
		return fmt.Errorf("创建归档目录 %s 失败: %w", taskArchiveDir, err)
	}

	byMonth := make(map[string][]*TaskRecord)
	for _, rec := range archive {
		m := archiveMonth(rec.UpdatedAt)
		byMonth[m] = append(byMonth[m], rec)
	}

	wanted := make(map[string]bool, len(byMonth))
	for month, recs := range byMonth {
		wanted[month] = true
		payload := taskStoreFile{Schema: taskStoreSchema, SavedAt: now, Tasks: recs}
		payload.Checksum = computeTasksChecksum(recs)
		data, err := json.Marshal(&payload)
		if err != nil {
			return fmt.Errorf("序列化归档快照 %s 失败: %w", month, err)
		}
		tmp := archiveFilePath(month) + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return fmt.Errorf("写入归档临时文件 %s 失败: %w", month, err)
		}
		if err := os.Rename(tmp, archiveFilePath(month)); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("替换归档文件 %s 失败: %w", month, err)
		}
	}

	// 清理归档目录里「月份已无记录」的旧文件（该月记录已全部超期删除）。
	if entries, err := os.ReadDir(taskArchiveDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "tasks-") || !strings.HasSuffix(name, ".json") {
				continue
			}
			month := strings.TrimSuffix(strings.TrimPrefix(name, "tasks-"), ".json")
			if !wanted[month] {
				_ = os.Remove(filepath.Join(taskArchiveDir, name))
			}
		}
	}
	return nil
}

// ===== 备份轮转 =====

// rotateBackup 距上次备份超过间隔时，把当前主快照复制一份到 backups/，并保留最近 N 份。
func rotateBackup(logger *logx.Logger) {
	if !taskStoreEnabled || taskBackupKeep <= 0 {
		return
	}
	lastBackupMu.Lock()
	if !lastBackupAt.IsZero() && time.Since(lastBackupAt) < taskStoreBackupInterval {
		lastBackupMu.Unlock()
		return
	}
	lastBackupAt = time.Now()
	lastBackupMu.Unlock()

	if _, err := os.Stat(taskStorePath); err != nil {
		return // 主快照还不存在，无从备份
	}
	if err := os.MkdirAll(taskBackupDir, 0o755); err != nil {
		logger.Print("TASK_STORE", "创建备份目录失败: "+err.Error())
		return
	}
	dst := filepath.Join(taskBackupDir, taskStoreFileName+"."+time.Now().Format("20060102150405"))
	data, err := os.ReadFile(taskStorePath)
	if err != nil {
		logger.Print("TASK_STORE", "读取主快照以备份失败: "+err.Error())
		return
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		logger.Print("TASK_STORE", "写入备份失败: "+err.Error())
		return
	}
	pruneBackups()
}

func pruneBackups() {
	entries, err := os.ReadDir(taskBackupDir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), taskStoreFileName+".") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files) // 时间戳升序，越新越靠后
	excess := len(files) - taskBackupKeep
	for i := 0; i < excess; i++ {
		_ = os.Remove(filepath.Join(taskBackupDir, files[i]))
	}
}

// restoreFromBackup 用最新一份备份覆盖主快照，成功返回 true。
func restoreFromBackup(logger *logx.Logger) bool {
	entries, err := os.ReadDir(taskBackupDir)
	if err != nil {
		return false
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), taskStoreFileName+".") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return false
	}
	sort.Strings(files)
	latest := filepath.Join(taskBackupDir, files[len(files)-1])
	data, err := os.ReadFile(latest)
	if err != nil {
		return false
	}
	if err := os.WriteFile(taskStorePath, data, 0o644); err != nil {
		return false
	}
	logger.Print("TASK_STORE", "已从备份恢复主快照: "+latest)
	return true
}

// ===== 记录快照 =====

// markTaskStoreDirty 标记「内存里有变更待落盘」，由后台协程合并写入。
func markTaskStoreDirty() {
	if !taskStoreEnabled {
		return
	}
	atomic.StoreInt32(&taskStoreDirty, 1)
}

// snapshotTaskRecords 锁内浅拷贝一份记录，锁外序列化，按提交时间升序返回。
func snapshotTaskRecords() []*TaskRecord {
	taskRecordsMu.Lock()
	out := make([]*TaskRecord, 0, len(taskRecords))
	for _, rec := range taskRecords {
		cp := *rec
		out = append(out, &cp)
	}
	taskRecordsMu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].TaskID < out[j].TaskID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// appendTaskMessage 拼接任务说明文字，保留原有信息。
func appendTaskMessage(oldMsg, add string) string {
	if strings.TrimSpace(oldMsg) == "" {
		return add
	}
	return oldMsg + "；" + add
}

// ===== 加载 =====

// readAndVerify 读取并解析快照文件，校验 checksum（若有），返回解析结果。
func readAndVerify(path string) (*taskStoreFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f taskStoreFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	if f.Checksum != "" {
		if got := computeTasksChecksum(f.Tasks); got != f.Checksum {
			return nil, fmt.Errorf("校验和不匹配")
		}
	}
	return &f, nil
}

// quarantine 把无法使用的快照文件改名隔离（保留现场供排查）。
func quarantine(logger *logx.Logger, path string, cause error) {
	bak := path + ".corrupt." + time.Now().Format("20060102150405")
	if rerr := os.Rename(path, bak); rerr != nil {
		logger.Print("TASK_STORE", "任务快照解析失败且隔离失败: "+path+" ("+cause.Error()+") / "+rerr.Error())
	} else {
		logger.Print("TASK_STORE", "任务快照解析失败，已隔离到 "+bak+": "+cause.Error())
	}
}

// loadTaskStore 启动时把历史任务（主快照 + 归档文件）读回内存。
func loadTaskStore(logger *logx.Logger) {
	if !taskStoreEnabled {
		logger.Print("TASK_STORE", "持久化已关闭（TASK_STORE_ENABLED=false），任务记录仅存内存，重启即清空")
		return
	}

	// 无论有没有读到历史都安排一次落盘：首次启动写出空快照，
	// 加载到孤儿任务时则负责把 interrupted 状态写回文件。
	defer markTaskStoreDirty()

	loaded, expired, orphan := 0, 0, 0

	// 主快照
	loaded, expired, orphan = loadTaskFile(logger, taskStorePath, true, loaded, expired, orphan)

	// 归档文件（按月，按文件名字典序=时间序加载，后加载的覆盖先加载的同 taskID）
	if taskArchiveDays > 0 {
		if entries, err := os.ReadDir(taskArchiveDir); err == nil {
			var names []string
			for _, e := range entries {
				if !e.IsDir() && strings.HasPrefix(e.Name(), "tasks-") && strings.HasSuffix(e.Name(), ".json") {
					names = append(names, e.Name())
				}
			}
			sort.Strings(names)
			for _, name := range names {
				loaded, expired, orphan = loadTaskFile(logger, filepath.Join(taskArchiveDir, name), false, loaded, expired, orphan)
			}
		}
	}

	logger.Print("TASK_STORE", fmt.Sprintf(
		"已加载历史任务 %d 条（主快照 %s；超期丢弃 %d 条；标记重启中断 %d 条）",
		loaded, taskStorePath, expired, orphan))
}

// loadTaskFile 读入单个快照/归档文件。isPrimary 表示主快照（损坏时优先尝试备份恢复）。
func loadTaskFile(logger *logx.Logger, path string, isPrimary bool, loaded, expired, orphan int) (int, int, int) {
	f, err := readAndVerify(path)
	if err != nil {
		if os.IsNotExist(err) {
			if isPrimary {
				logger.Print("TASK_STORE", "无历史任务快照（首次启动），记录文件: "+path)
			}
			return loaded, expired, orphan
		}
		// 主快照损坏：先尝试从滚动备份恢复
		if isPrimary && restoreFromBackup(logger) {
			if f2, err2 := readAndVerify(path); err2 == nil {
				f = f2
			} else {
				quarantine(logger, path, err2)
				return loaded, expired, orphan
			}
		} else {
			quarantine(logger, path, err)
			return loaded, expired, orphan
		}
	}

	cutoff := taskRetentionCutoff(time.Now())

	taskRecordsMu.Lock()
	for _, rec := range f.Tasks {
		if rec == nil || rec.TaskID == "" {
			continue
		}
		if !cutoff.IsZero() && rec.CreatedAt.Before(cutoff) {
			expired++
			continue
		}
		// 快照里仍是 queued/running 的，一定是上次进程没跑完就退出的孤儿任务。
		if rec.Status == taskStatusQueued || rec.Status == taskStatusRunning {
			rec.Message = appendTaskMessage(rec.Message, "服务重启中断")
			rec.Status = taskStatusInterrupted
			orphan++
		}
		taskRecords[rec.TaskID] = rec
		loaded++
	}
	taskRecordsMu.Unlock()

	return loaded, expired, orphan
}

// ===== 落盘 =====

// saveTaskStore 把当前内存记录落盘：主快照写活跃记录，归档集合变化时重写归档文件。
// 先写归档、再写主快照，保证崩溃最坏只出现「两边都有」，不会丢记录。
func saveTaskStore() error {
	if !taskStoreEnabled {
		return nil
	}

	taskStoreSaveMu.Lock()
	defer taskStoreSaveMu.Unlock()

	tasks := snapshotTaskRecords()
	now := time.Now()
	tasks = filterExpired(tasks, now)
	tasks = trimTaskRecords(tasks, taskMaxRecords)

	active, archive := splitActiveArchive(tasks, now)

	// 先写归档（仅当归档集合变化）
	if taskArchiveDays > 0 {
		sig := archiveSetSignature(archive)
		if sig != archiveSignature {
			if err := writeArchiveFiles(archive, now); err != nil {
				return err
			}
			archiveSignature = sig
		}
	}

	// 再写主快照（只含活跃记录）
	payload := taskStoreFile{Schema: taskStoreSchema, SavedAt: now, Tasks: active}
	payload.Checksum = computeTasksChecksum(active)
	data, err := json.Marshal(&payload)
	if err != nil {
		return fmt.Errorf("序列化任务快照失败: %w", err)
	}
	if err := os.MkdirAll(taskStoreDir, 0o755); err != nil {
		return fmt.Errorf("创建快照目录 %s 失败: %w", taskStoreDir, err)
	}
	tmpPath := taskStorePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("写入临时快照失败: %w", err)
	}
	if err := os.Rename(tmpPath, taskStorePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("替换任务快照失败: %w", err)
	}
	return nil
}

// reportTaskStoreError 记录失败并打日志，同样的错误只打一次，避免刷屏。
func reportTaskStoreError(logger *logx.Logger, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	taskStoreErrMu.Lock()
	dup := msg == taskStoreLastError
	taskStoreLastError = msg
	taskStoreErrMu.Unlock()
	if dup {
		return
	}
	logger.Print("TASK_STORE", "保存任务快照失败（将自动重试）: "+msg)
}

func clearTaskStoreError() {
	taskStoreErrMu.Lock()
	taskStoreLastError = ""
	taskStoreErrMu.Unlock()
}

// flushTaskStoreNow 立即落盘，不等合并窗口（供进程退出前调用）。
func flushTaskStoreNow(logger *logx.Logger) {
	if !taskStoreEnabled {
		return
	}
	atomic.StoreInt32(&taskStoreDirty, 0)
	if err := saveTaskStore(); err != nil {
		reportTaskStoreError(logger, err)
		return
	}
	clearTaskStoreError()
	logger.Print("TASK_STORE", "退出前任务快照已保存: "+taskStorePath)
}

// trimTaskRecords 按条数上限裁剪（入参按时间升序）：优先丢弃最旧的终态记录，
// 未完成的任务一律保留，避免把排队中的任务裁掉。
func trimTaskRecords(tasks []*TaskRecord, max int) []*TaskRecord {
	if max <= 0 || len(tasks) <= max {
		return tasks
	}
	need := len(tasks) - max
	drop := make(map[string]bool, need)
	for _, rec := range tasks {
		if need == 0 {
			break
		}
		if rec.Status == taskStatusQueued || rec.Status == taskStatusRunning {
			continue
		}
		drop[rec.TaskID] = true
		need--
	}
	if len(drop) == 0 {
		return tasks
	}
	kept := make([]*TaskRecord, 0, len(tasks)-len(drop))
	for _, rec := range tasks {
		if !drop[rec.TaskID] {
			kept = append(kept, rec)
		}
	}
	return kept
}

// pruneTaskRecords 清理内存中的超期记录与超出条数上限的记录，返回删除条数。
func pruneTaskRecords() int {
	cutoff := taskRetentionCutoff(time.Now())
	if cutoff.IsZero() && taskMaxRecords <= 0 {
		return 0
	}

	taskRecordsMu.Lock()
	removed := 0

	if !cutoff.IsZero() {
		for id, rec := range taskRecords {
			if rec.CreatedAt.Before(cutoff) {
				delete(taskRecords, id)
				removed++
			}
		}
	}

	if taskMaxRecords > 0 && len(taskRecords) > taskMaxRecords {
		all := make([]*TaskRecord, 0, len(taskRecords))
		for _, rec := range taskRecords {
			all = append(all, rec)
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].CreatedAt.Equal(all[j].CreatedAt) {
				return all[i].TaskID < all[j].TaskID
			}
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		})
		need := len(all) - taskMaxRecords
		for _, rec := range all {
			if need == 0 {
				break
			}
			if rec.Status == taskStatusQueued || rec.Status == taskStatusRunning {
				continue
			}
			delete(taskRecords, rec.TaskID)
			removed++
			need--
		}
	}

	taskRecordsMu.Unlock()

	if removed > 0 {
		markTaskStoreDirty()
	}
	return removed
}

// startTaskStoreWriter 启动后台协程：主快照合并落盘 + 定期内存清理与滚动备份。
func startTaskStoreWriter(logger *logx.Logger) {
	if !taskStoreEnabled {
		return
	}

	go func() {
		flushTicker := time.NewTicker(taskStoreFlushInterval)
		defer flushTicker.Stop()
		maintTicker := time.NewTicker(taskStoreMaintenanceInterval)
		defer maintTicker.Stop()

		for {
			select {
			case <-flushTicker.C:
				if atomic.LoadInt32(&taskStoreDirty) == 0 {
					continue
				}
				atomic.StoreInt32(&taskStoreDirty, 0)
				if err := saveTaskStore(); err != nil {
					atomic.StoreInt32(&taskStoreDirty, 1) // 保留脏标记，下轮重试
					reportTaskStoreError(logger, err)
				} else {
					clearTaskStoreError()
				}
			case <-maintTicker.C:
				if n := pruneTaskRecords(); n > 0 {
					logger.Print("TASK_STORE", fmt.Sprintf("已清理 %d 条超期/超量任务记录", n))
				}
				rotateBackup(logger)
			}
		}
	}()
}

// taskStoreStatus 供 /tasks 响应附带存储状态，便于排查「为什么重启后没数据」。
func taskStoreStatus() map[string]any {
	st := map[string]any{
		"enabled":        taskStoreEnabled,
		"file":           filepath.ToSlash(taskStorePath),
		"retention_days": taskRetentionDays,
		"archive_days":   taskArchiveDays,
		"backup_keep":    taskBackupKeep,
	}
	if !taskStoreEnabled {
		return st
	}
	if fi, err := os.Stat(taskStorePath); err == nil {
		st["saved_at"] = fi.ModTime().Format(time.RFC3339)
		st["size_bytes"] = fi.Size()
	}
	if taskArchiveDays > 0 {
		if entries, err := os.ReadDir(taskArchiveDir); err == nil {
			n := 0
			var size int64
			for _, e := range entries {
				if !e.IsDir() && strings.HasPrefix(e.Name(), "tasks-") && strings.HasSuffix(e.Name(), ".json") {
					n++
					if fi, err2 := e.Info(); err2 == nil {
						size += fi.Size()
					}
				}
			}
			st["archive_files"] = n
			st["archive_size_bytes"] = size
		}
	}
	return st
}
