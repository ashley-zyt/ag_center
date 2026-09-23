package main

// 任务记录持久化 —— 把内存里的 taskRecords 落到本地 SQLite 单文件数据库，
// 避免服务重启/被 kill 之后任务进度全部清零。
//
// 设计取舍（为什么内存仍是主数据源、SQLite 只做持久化）：
//   - 查询/聚合（GET /tasks、/tasks/summary）始终读内存 map，O(1) 级，永不落 SQL；
//     SQLite 只负责「写」与「启动加载」，两者职责分明。
//   - 状态变更只置「脏」标记，由后台协程按 taskStoreFlushInterval 合并落盘：一次任务执行
//     会变更 2~3 次状态（queued→running→success/failed），合并写入可避免频繁整库重写。
//   - 落盘采用「事务内 DELETE 全表 + 批量 INSERT」全量重建，保证内存与数据库严格一致；
//     被清空/裁剪的记录无需额外做增量删除，重启后不会复活。
//   - 重启加载时，库里仍为 queued/running 的任务必然是「孤儿」——执行它的 goroutine 已随
//     进程消失、不可能再推进，因此统一标记为 interrupted，而不是伪装成还在跑。
//
// 环境变量：
//
//	TASK_STORE_ENABLED     默认 true；设 false/0 退回纯内存
//	TASK_STORE_DIR         数据库文件目录，默认 data（相对工作目录，与 logs/ 同级）
//	TASK_RETENTION_DAYS    总保留天数，默认 30；设 0 表示永久保留
//	TASK_MAX_RECORDS       记录条数上限，默认 0（不限）；超限时优先丢弃最旧的终态记录

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动，无需 CGO/gcc，注册 driver 名 "sqlite"

	"minimax_pro/internal/logx"
)

const (
	// taskStoreFileName 数据库文件名。
	taskStoreFileName = "tasks.db"
	// taskStoreFlushInterval 状态变更后的合并落盘窗口。
	taskStoreFlushInterval = 2 * time.Second
	// taskStoreMaintenanceInterval 低频维护周期（内存清理 + 落盘同步删除）。
	taskStoreMaintenanceInterval = time.Hour
)

var (
	taskStoreEnabled  = true
	taskStoreDir      = "data"
	taskRetentionDays = 30
	taskMaxRecords    = 0
	taskStorePath     string

	taskStoreDirty     int32 // 1 表示内存有未落盘的变更
	taskStoreDB        *sql.DB
	taskStoreDBMu      sync.Mutex // 串行化所有 DB 写（SQLite 单写）
	taskStoreErrMu     sync.Mutex
	taskStoreLastError string
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
	if v := strings.TrimSpace(os.Getenv("TASK_MAX_RECORDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			taskMaxRecords = n
		}
	}
	taskStorePath = filepath.Join(taskStoreDir, taskStoreFileName)
}

// taskStoreSchemaSQL 建表语句。时间统一存 Unix 毫秒（整数），便于 SQL 排序与按窗口删除。
const taskStoreSchemaSQL = `
CREATE TABLE IF NOT EXISTS tasks (
	task_id      TEXT PRIMARY KEY,
	type         TEXT NOT NULL,
	profile_name TEXT NOT NULL DEFAULT '',
	ref          TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL,
	message      TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_type ON tasks(type);
CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at);
`

// openTaskStoreDB 打开（必要时创建）SQLite 数据库并确保表结构存在。
func openTaskStoreDB() (*sql.DB, error) {
	if err := os.MkdirAll(taskStoreDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建目录 %s 失败: %w", taskStoreDir, err)
	}
	db, err := sql.Open("sqlite", taskStorePath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 只支持单写者，固定单连接，避免并发写时出现 "database is locked"。
	db.SetMaxOpenConns(1)

	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			task_id      TEXT PRIMARY KEY,
			type         TEXT NOT NULL,
			profile_name TEXT NOT NULL DEFAULT '',
			ref          TEXT NOT NULL DEFAULT '',
			batch        TEXT NOT NULL DEFAULT '',
			batch_total  INTEGER NOT NULL DEFAULT 0,
			payload      TEXT NOT NULL DEFAULT '',
			status       TEXT NOT NULL,
			message      TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			dingtalk_webhook TEXT NOT NULL DEFAULT '',
			dingtalk_keyword TEXT NOT NULL DEFAULT '',
			dingtalk_owner TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_type ON tasks(type)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("初始化表结构失败: %w", err)
		}
	}
	// 兼容旧库（此前建表漏了 batch、后来又加 payload、再加钉钉列）：缺列则补上。
	for _, col := range []struct{ name, decl string }{
		{"batch", "batch TEXT NOT NULL DEFAULT ''"},
		{"batch_total", "batch_total INTEGER NOT NULL DEFAULT 0"},
		{"payload", "payload TEXT NOT NULL DEFAULT ''"},
		{"dingtalk_webhook", "dingtalk_webhook TEXT NOT NULL DEFAULT ''"},
		{"dingtalk_keyword", "dingtalk_keyword TEXT NOT NULL DEFAULT ''"},
		{"dingtalk_owner", "dingtalk_owner TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(db, "tasks", col.name, col.decl); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// ensureColumn 检查表是否已有指定列，没有则 ALTER TABLE 补上（SQLite 兼容旧库迁移）。
func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("查询表结构失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("解析表结构失败: %w", err)
		}
		if name == column {
			return nil // 已存在
		}
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, decl)); err != nil {
		return fmt.Errorf("补列 %s 失败: %w", column, err)
	}
	return nil
}

// taskRetentionCutoff 返回保留窗口的起始时间；零值表示不过期（永久保留）。
func taskRetentionCutoff(now time.Time) time.Time {
	if taskRetentionDays <= 0 {
		return time.Time{}
	}
	return now.AddDate(0, 0, -taskRetentionDays)
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

// loadTaskStore 启动时把历史任务从 SQLite 读回内存。
func loadTaskStore(logger *logx.Logger) {
	if !taskStoreEnabled {
		logger.Print("TASK_STORE", "持久化已关闭（TASK_STORE_ENABLED=false），任务记录仅存内存，重启即清空")
		return
	}

	db, err := openTaskStoreDB()
	if err != nil {
		logger.Print("TASK_STORE", "打开任务数据库失败: "+err.Error())
		return
	}
	taskStoreDBMu.Lock()
	if taskStoreDB != nil {
		_ = taskStoreDB.Close() // 重复加载（测试/异常恢复）时先关旧连接
	}
	taskStoreDB = db
	taskStoreDBMu.Unlock()

	// 无论有没有读到历史都安排一次落盘：首次启动写出空库，
	// 加载到孤儿任务时则负责把 interrupted 状态与超期裁剪写回库。
	defer markTaskStoreDirty()

	rows, err := db.Query(`SELECT task_id, type, profile_name, ref, batch, batch_total, payload, status, message, created_at, updated_at, dingtalk_webhook, dingtalk_keyword, dingtalk_owner
	                       FROM tasks ORDER BY created_at ASC`)
	if err != nil {
		logger.Print("TASK_STORE", "查询任务记录失败: "+err.Error())
		return
	}
	defer rows.Close()

	cutoff := taskRetentionCutoff(time.Now())
	loaded, expired, orphan := 0, 0, 0

	taskRecordsMu.Lock()
	for rows.Next() {
		var rec TaskRecord
		var createdMS, updatedMS int64
		if err := rows.Scan(&rec.TaskID, &rec.Type, &rec.ProfileName, &rec.Ref,
			&rec.Batch, &rec.BatchTotal, &rec.Payload, &rec.Status, &rec.Message, &createdMS, &updatedMS,
			&rec.DingtalkWebhook, &rec.DingtalkKeyword, &rec.DingtalkOwner); err != nil {
			continue
		}
		rec.CreatedAt = time.UnixMilli(createdMS)
		rec.UpdatedAt = time.UnixMilli(updatedMS)

		if rec.TaskID == "" {
			continue
		}
		if !cutoff.IsZero() && rec.CreatedAt.Before(cutoff) {
			expired++
			continue
		}
		// 库里仍是 queued/running 的，一定是上次进程没跑完就退出的孤儿任务。
		if rec.Status == taskStatusRunning {
			// 执行中被打断：不可幂等，只能标记 interrupted
			rec.Message = appendTaskMessage(rec.Message, "服务重启中断")
			rec.Status = taskStatusInterrupted
			orphan++
		} else if rec.Status == taskStatusQueued {
			// 排队中：发布任务若参数完整（有 payload），保留 queued，由 resumeQueuedPublishTasks 自动重放；
			// 其余类型（fetch/nurture/send_message/check_reply）仍标记 interrupted，靠各自调度器兜底。
			if rec.Payload != "" && strings.HasSuffix(rec.Type, "_publish") {
				// 保留 queued，等待重放
			} else {
				rec.Message = appendTaskMessage(rec.Message, "服务重启中断")
				rec.Status = taskStatusInterrupted
				orphan++
			}
		}
		taskRecords[rec.TaskID] = &rec
		loaded++
	}
	taskRecordsMu.Unlock()

	logger.Print("TASK_STORE", fmt.Sprintf(
		"已加载历史任务 %d 条（数据库 %s；超期丢弃 %d 条；标记重启中断 %d 条）",
		loaded, taskStorePath, expired, orphan))
}

// ===== 落盘 =====

// saveTaskStore 把当前内存记录落盘：事务内 DELETE 全表 + 批量 INSERT（全量重建），
// 保证内存与数据库严格一致。被清空/裁剪的记录无需额外增量删除。
func saveTaskStore() error {
	if !taskStoreEnabled {
		return nil
	}

	taskStoreDBMu.Lock()
	defer taskStoreDBMu.Unlock()

	db := taskStoreDB
	if db == nil {
		return nil // 数据库尚未初始化（打开失败或未加载），跳过落盘
	}

	tasks := snapshotTaskRecords()
	now := time.Now()
	tasks = filterExpired(tasks, now)
	tasks = trimTaskRecords(tasks, taskMaxRecords)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM tasks"); err != nil {
		return fmt.Errorf("清空任务表失败: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO tasks
		(task_id, type, profile_name, ref, batch, batch_total, payload, status, message, created_at, updated_at, dingtalk_webhook, dingtalk_keyword, dingtalk_owner)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("准备插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, rec := range tasks {
		if _, err := stmt.Exec(rec.TaskID, rec.Type, rec.ProfileName, rec.Ref, rec.Batch, rec.BatchTotal, rec.Payload,
			rec.Status, rec.Message, rec.CreatedAt.UnixMilli(), rec.UpdatedAt.UnixMilli(),
			rec.DingtalkWebhook, rec.DingtalkKeyword, rec.DingtalkOwner); err != nil {
			return fmt.Errorf("写入任务 %s 失败: %w", rec.TaskID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
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
	logger.Print("TASK_STORE", "保存任务记录失败（将自动重试）: "+msg)
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
	logger.Print("TASK_STORE", "退出前任务记录已保存: "+taskStorePath)
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
// 数据库侧的同步删除由随后的 saveTaskStore 全量重建完成。
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

// startTaskStoreWriter 启动后台协程：合并落盘 + 定期内存清理（DB 同步靠全量重建）。
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
		"max_records":    taskMaxRecords,
	}
	if !taskStoreEnabled {
		return st
	}
	if fi, err := os.Stat(taskStorePath); err == nil {
		st["saved_at"] = fi.ModTime().Format(time.RFC3339)
		st["size_bytes"] = fi.Size()
	}
	return st
}
