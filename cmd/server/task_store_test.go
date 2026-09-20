package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minimax_pro/internal/logx"
)

// newTestLogger 在临时目录里建 logger，避免测试往 cmd/server/logs 写文件。
func newTestLogger(t *testing.T) *logx.Logger {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	logger := logx.New(io.Discard)
	// 必须在 TempDir 清理之前关闭日志文件句柄，否则 Windows 上目录删不掉。
	t.Cleanup(logger.Close)
	return logger
}

// isolateTaskStore 隔离持久化相关的全局状态与任务表。
func isolateTaskStore(t *testing.T) {
	t.Helper()

	oldEnabled, oldDir, oldPath := taskStoreEnabled, taskStoreDir, taskStorePath
	oldArchiveDir, oldBackupDir := taskArchiveDir, taskBackupDir
	oldRetention, oldArchive, oldBackup, oldMax := taskRetentionDays, taskArchiveDays, taskBackupKeep, taskMaxRecords
	oldRecords, oldCancels := taskRecords, taskCancels
	oldCleared := clearedTasks
	oldSig := archiveSignature
	oldLastBackup := lastBackupAt

	taskStoreEnabled = true
	taskStoreDir = t.TempDir()
	taskStorePath = filepath.Join(taskStoreDir, taskStoreFileName)
	taskArchiveDir = filepath.Join(taskStoreDir, "archive")
	taskBackupDir = filepath.Join(taskStoreDir, "backups")
	taskRetentionDays = 30
	taskArchiveDays = 7
	taskBackupKeep = 5
	taskMaxRecords = 0
	archiveSignature = ""
	lastBackupAt = time.Time{}

	taskRecordsMu.Lock()
	taskRecords = make(map[string]*TaskRecord)
	taskCancels = make(map[string]context.CancelFunc)
	clearedTasks = make(map[string]bool)
	taskRecordsMu.Unlock()

	atomic.StoreInt32(&taskStoreDirty, 0)

	t.Cleanup(func() {
		taskStoreEnabled, taskStoreDir, taskStorePath = oldEnabled, oldDir, oldPath
		taskArchiveDir, taskBackupDir = oldArchiveDir, oldBackupDir
		taskRetentionDays, taskArchiveDays, taskBackupKeep, taskMaxRecords = oldRetention, oldArchive, oldBackup, oldMax
		archiveSignature = oldSig
		lastBackupAt = oldLastBackup

		taskRecordsMu.Lock()
		taskRecords, taskCancels, clearedTasks = oldRecords, oldCancels, oldCleared
		taskRecordsMu.Unlock()

		atomic.StoreInt32(&taskStoreDirty, 0)
	})
}

func putRecords(recs ...*TaskRecord) {
	taskRecordsMu.Lock()
	for _, rec := range recs {
		cp := *rec
		taskRecords[rec.TaskID] = &cp
	}
	taskRecordsMu.Unlock()
}

// TestTaskStoreRoundTrip 落盘后重启加载，历史记录应恢复，未完成任务应标记 interrupted。
func TestTaskStoreRoundTrip(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)
	now := time.Now()

	putRecords(
		&TaskRecord{TaskID: "t-done", Type: "facebook_publish", ProfileName: "fb001",
			Ref: "MoveTask:1", Status: taskStatusSuccess, Message: "publish_triggered",
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-110 * time.Minute)},
		&TaskRecord{TaskID: "t-run", Type: "nurture", ProfileName: "fb002",
			Status: taskStatusRunning, Message: "养号中",
			CreatedAt: now.Add(-30 * time.Minute), UpdatedAt: now.Add(-20 * time.Minute)},
		&TaskRecord{TaskID: "t-queue", Type: "fetch", ProfileName: "fb003",
			Ref: "Account:9", Status: taskStatusQueued,
			CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-10 * time.Minute)},
	)

	if err := saveTaskStore(); err != nil {
		t.Fatalf("saveTaskStore 失败: %v", err)
	}
	if _, err := os.Stat(taskStorePath); err != nil {
		t.Fatalf("快照文件未生成: %v", err)
	}

	// 模拟重启：清空内存后重新加载
	taskRecordsMu.Lock()
	taskRecords = make(map[string]*TaskRecord)
	taskRecordsMu.Unlock()

	loadTaskStore(logger)

	if rec := getTaskRecord("t-done"); rec == nil {
		t.Fatal("已完成任务未恢复")
	} else if rec.Status != taskStatusSuccess || rec.Ref != "MoveTask:1" || rec.Message != "publish_triggered" {
		t.Fatalf("已完成任务字段被改动: %+v", rec)
	}

	for _, id := range []string{"t-run", "t-queue"} {
		rec := getTaskRecord(id)
		if rec == nil {
			t.Fatalf("任务 %s 未恢复", id)
		}
		if rec.Status != taskStatusInterrupted {
			t.Fatalf("任务 %s 应标记为 interrupted，实际 %s", id, rec.Status)
		}
		if !contains(rec.Message, "服务重启中断") {
			t.Fatalf("任务 %s 缺少中断说明，实际 message=%q", id, rec.Message)
		}
	}
	// 原有 message 不应被覆盖
	if rec := getTaskRecord("t-run"); !contains(rec.Message, "养号中") {
		t.Fatalf("原有 message 被覆盖: %q", rec.Message)
	}
}

// TestTaskStoreFirstBootWritesSnapshot 首次启动（无历史文件）也应安排落盘，生成空快照。
func TestTaskStoreFirstBootWritesSnapshot(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)

	if _, err := os.Stat(taskStorePath); !os.IsNotExist(err) {
		t.Fatalf("测试前置条件错误，快照文件不应存在: %v", err)
	}

	loadTaskStore(logger)

	if atomic.LoadInt32(&taskStoreDirty) != 1 {
		t.Fatal("首次加载后应置脏标记，等待写出空快照")
	}
	if err := saveTaskStore(); err != nil {
		t.Fatalf("saveTaskStore: %v", err)
	}
	if _, err := os.Stat(taskStorePath); err != nil {
		t.Fatalf("首次启动应生成快照文件: %v", err)
	}

	// 空快照应能再次正常加载，不产生多余记录
	loadTaskStore(logger)
	taskRecordsMu.Lock()
	n := len(taskRecords)
	taskRecordsMu.Unlock()
	if n != 0 {
		t.Fatalf("空快照不应加载出记录，实际 %d 条", n)
	}
}

// TestTaskStoreDropsExpired 超期记录不应被加载。
func TestTaskStoreDropsExpired(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)
	taskRetentionDays = 30

	payload := taskStoreFile{
		Schema:  taskStoreSchema,
		SavedAt: time.Now(),
		Tasks: []*TaskRecord{
			{TaskID: "old", Type: "fetch", Status: taskStatusSuccess,
				CreatedAt: time.Now().AddDate(0, 0, -40), UpdatedAt: time.Now().AddDate(0, 0, -40)},
			{TaskID: "fresh", Type: "fetch", Status: taskStatusSuccess,
				CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now().Add(-2 * time.Hour)},
		},
	}
	data, err := json.Marshal(&payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(taskStorePath, data, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	loadTaskStore(logger)

	if getTaskRecord("old") != nil {
		t.Fatal("超期记录不应被加载")
	}
	if getTaskRecord("fresh") == nil {
		t.Fatal("保留期内记录应被加载")
	}
}

// TestTaskStoreRetentionZeroKeepsAll 保留天数为 0 时永久保留。
func TestTaskStoreRetentionZeroKeepsAll(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)
	taskRetentionDays = 0

	putRecords(&TaskRecord{TaskID: "ancient", Type: "fetch", Status: taskStatusSuccess,
		CreatedAt: time.Now().AddDate(-1, 0, 0), UpdatedAt: time.Now().AddDate(-1, 0, 0)})

	loadTaskStore(logger) // 空文件路径 → 直接返回
	if err := saveTaskStore(); err != nil {
		t.Fatalf("saveTaskStore: %v", err)
	}

	taskRecordsMu.Lock()
	taskRecords = make(map[string]*TaskRecord)
	taskRecordsMu.Unlock()
	loadTaskStore(logger)

	if getTaskRecord("ancient") == nil {
		t.Fatal("保留天数为 0 时应永久保留")
	}
}

// TestTaskStoreCorruptFileIsQuarantined 损坏的快照应被备份而不是让服务起不来。
func TestTaskStoreCorruptFileIsQuarantined(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)

	if err := os.WriteFile(taskStorePath, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	loadTaskStore(logger)

	if _, err := os.Stat(taskStorePath); !os.IsNotExist(err) {
		t.Fatalf("损坏文件应被移走，实际 stat err=%v", err)
	}
	matches, _ := filepath.Glob(taskStorePath + ".corrupt.*")
	if len(matches) == 0 {
		t.Fatal("未生成损坏文件的备份")
	}
}

// TestTrimTaskRecordsKeepsUnfinished 条数超限时只裁最旧的终态记录，未完成的一律保留。
func TestTrimTaskRecordsKeepsUnfinished(t *testing.T) {
	base := time.Now()
	tasks := []*TaskRecord{
		{TaskID: "a", Status: taskStatusSuccess, CreatedAt: base.Add(-3 * time.Hour)},
		{TaskID: "b", Status: taskStatusQueued, CreatedAt: base.Add(-2 * time.Hour)},
		{TaskID: "c", Status: taskStatusFailed, CreatedAt: base.Add(-1 * time.Hour)},
		{TaskID: "d", Status: taskStatusSuccess, CreatedAt: base},
	}

	got := trimTaskRecords(tasks, 2)
	ids := make([]string, 0, len(got))
	for _, rec := range got {
		ids = append(ids, rec.TaskID)
	}
	// 应丢掉最旧的终态 a、再丢 c；b(排队中) 必须保留
	if len(ids) != 2 || ids[0] != "b" || ids[1] != "d" {
		t.Fatalf("裁剪结果不符合预期: %v", ids)
	}

	// 上限大于总数时原样返回
	if out := trimTaskRecords(tasks, 10); len(out) != len(tasks) {
		t.Fatalf("未超限不应裁剪，实际 %d 条", len(out))
	}
}

// TestPruneTaskRecordsClearsMemory 定期清理应同时清掉内存里的超期记录。
func TestPruneTaskRecordsClearsMemory(t *testing.T) {
	isolateTaskStore(t)
	taskRetentionDays = 30
	taskMaxRecords = 0

	putRecords(
		&TaskRecord{TaskID: "old", Type: "fetch", Status: taskStatusSuccess,
			CreatedAt: time.Now().AddDate(0, 0, -45), UpdatedAt: time.Now().AddDate(0, 0, -45)},
		&TaskRecord{TaskID: "keep", Type: "fetch", Status: taskStatusSuccess,
			CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now().Add(-time.Minute)},
	)

	if n := pruneTaskRecords(); n != 1 {
		t.Fatalf("应清理 1 条，实际 %d", n)
	}
	if getTaskRecord("old") != nil {
		t.Fatal("超期记录仍在内存中")
	}
	if getTaskRecord("keep") == nil {
		t.Fatal("保留期内记录被误删")
	}
	if atomic.LoadInt32(&taskStoreDirty) != 1 {
		t.Fatal("清理后应置脏标记等待落盘")
	}
}

// TestTaskStoreArchiveSplitsOldTerminal 老终态记录应进入归档文件、离开主快照，重启后仍能恢复。
func TestTaskStoreArchiveSplitsOldTerminal(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)
	taskArchiveDays = 7
	now := time.Now()

	putRecords(
		&TaskRecord{TaskID: "t-old", Type: "facebook_publish", Status: taskStatusSuccess,
			CreatedAt: now.AddDate(0, 0, -10), UpdatedAt: now.AddDate(0, 0, -9)},
		&TaskRecord{TaskID: "t-new", Type: "facebook_publish", Status: taskStatusSuccess,
			CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-30 * time.Minute)},
	)

	if err := saveTaskStore(); err != nil {
		t.Fatalf("saveTaskStore: %v", err)
	}

	// 主快照应只含 t-new
	raw, err := os.ReadFile(taskStorePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f taskStoreFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(f.Tasks) != 1 || f.Tasks[0].TaskID != "t-new" {
		t.Fatalf("主快照应只含 t-new，实际 %d 条", len(f.Tasks))
	}

	// 归档文件应含 t-old
	month := archiveMonth(now.AddDate(0, 0, -9))
	araw, err := os.ReadFile(archiveFilePath(month))
	if err != nil {
		t.Fatalf("读归档文件失败: %v", err)
	}
	var af taskStoreFile
	if err := json.Unmarshal(araw, &af); err != nil {
		t.Fatalf("unmarshal archive: %v", err)
	}
	found := false
	for _, rec := range af.Tasks {
		if rec.TaskID == "t-old" {
			found = true
		}
	}
	if !found {
		t.Fatal("t-old 未进入归档文件")
	}

	// 重启后两条都应恢复
	taskRecordsMu.Lock()
	taskRecords = make(map[string]*TaskRecord)
	taskRecordsMu.Unlock()
	loadTaskStore(logger)
	if getTaskRecord("t-old") == nil {
		t.Fatal("重启后归档中的 t-old 未恢复")
	}
	if getTaskRecord("t-new") == nil {
		t.Fatal("重启后主快照中的 t-new 未恢复")
	}
}

// TestTaskStoreChecksumDetectsTamper 主快照被篡改后，加载应检测到校验和失配并隔离。
func TestTaskStoreChecksumDetectsTamper(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)

	putRecords(&TaskRecord{TaskID: "t1", Type: "fetch", Status: taskStatusSuccess,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour)})
	if err := saveTaskStore(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 篡改主快照内容（把 task_id 里的 t1 改成 t2，破坏校验和）
	raw, _ := os.ReadFile(taskStorePath)
	tampered := append([]byte{}, raw...)
	if i := bytes.Index(tampered, []byte("t1")); i >= 0 {
		tampered[i+1] = '2'
	}
	if err := os.WriteFile(taskStorePath, tampered, 0o644); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	loadTaskStore(logger)

	// 被篡改的文件应被隔离（rename 成 .corrupt）
	if _, err := os.Stat(taskStorePath); !os.IsNotExist(err) {
		t.Fatalf("被篡改的文件应被隔离，实际 stat err=%v", err)
	}
	matches, _ := filepath.Glob(taskStorePath + ".corrupt.*")
	if len(matches) == 0 {
		t.Fatal("未生成被篡改文件的隔离备份")
	}
}

// TestTaskStoreBackupRestores 主快照损坏但存在滚动备份时，加载应从备份恢复。
func TestTaskStoreBackupRestores(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)
	taskBackupKeep = 3

	putRecords(&TaskRecord{TaskID: "t1", Type: "fetch", Status: taskStatusSuccess,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour)})
	if err := saveTaskStore(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 做一次滚动备份（isolateTaskStore 已把 lastBackupAt 归零，首次调用会执行）
	rotateBackup(logger)

	// 破坏主快照
	if err := os.WriteFile(taskStorePath, []byte("{broken"), 0o644); err != nil {
		t.Fatalf("write broken: %v", err)
	}

	loadTaskStore(logger)

	// 应从备份恢复，t1 还在，主快照文件也应恢复为合法内容
	if getTaskRecord("t1") == nil {
		t.Fatal("损坏后未从备份恢复")
	}
	if _, err := os.Stat(taskStorePath); err != nil {
		t.Fatalf("主快照未恢复: %v", err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
