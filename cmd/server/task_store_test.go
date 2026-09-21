package main

import (
	"context"
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
	oldRetention, oldMax := taskRetentionDays, taskMaxRecords
	oldRecords, oldCancels := taskRecords, taskCancels
	oldCleared := clearedTasks
	oldDB := taskStoreDB

	taskStoreEnabled = true
	taskStoreDir = t.TempDir()
	taskStorePath = filepath.Join(taskStoreDir, taskStoreFileName)
	taskRetentionDays = 30
	taskMaxRecords = 0

	taskRecordsMu.Lock()
	taskRecords = make(map[string]*TaskRecord)
	taskCancels = make(map[string]context.CancelFunc)
	clearedTasks = make(map[string]bool)
	taskRecordsMu.Unlock()

	taskStoreDBMu.Lock()
	taskStoreDB = nil
	taskStoreDBMu.Unlock()

	atomic.StoreInt32(&taskStoreDirty, 0)

	t.Cleanup(func() {
		taskStoreDBMu.Lock()
		if taskStoreDB != nil {
			_ = taskStoreDB.Close()
		}
		taskStoreDB = oldDB
		taskStoreDBMu.Unlock()

		taskStoreEnabled, taskStoreDir, taskStorePath = oldEnabled, oldDir, oldPath
		taskRetentionDays, taskMaxRecords = oldRetention, oldMax

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

	loadTaskStore(logger)
	if err := saveTaskStore(); err != nil {
		t.Fatalf("saveTaskStore 失败: %v", err)
	}
	if _, err := os.Stat(taskStorePath); err != nil {
		t.Fatalf("数据库文件未生成: %v", err)
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

// TestTaskStoreFirstBootWritesSnapshot 首次启动（无历史文件）也应安排落盘，生成空数据库。
func TestTaskStoreFirstBootWritesSnapshot(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)

	if _, err := os.Stat(taskStorePath); !os.IsNotExist(err) {
		t.Fatalf("测试前置条件错误，数据库文件不应存在: %v", err)
	}

	loadTaskStore(logger)

	if atomic.LoadInt32(&taskStoreDirty) != 1 {
		t.Fatal("首次加载后应置脏标记，等待写出空库")
	}
	if err := saveTaskStore(); err != nil {
		t.Fatalf("saveTaskStore: %v", err)
	}
	if _, err := os.Stat(taskStorePath); err != nil {
		t.Fatalf("首次启动应生成数据库文件: %v", err)
	}

	// 空库应能再次正常加载，不产生多余记录
	loadTaskStore(logger)
	taskRecordsMu.Lock()
	n := len(taskRecords)
	taskRecordsMu.Unlock()
	if n != 0 {
		t.Fatalf("空库不应加载出记录，实际 %d 条", n)
	}
}

// TestTaskStoreDropsExpired 超期记录不应被加载。
func TestTaskStoreDropsExpired(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)
	taskRetentionDays = 30

	// 直接往数据库里塞一条超期记录和一条新鲜记录，绕过 saveTaskStore 的过滤
	db, err := openTaskStoreDB()
	if err != nil {
		t.Fatalf("openTaskStoreDB: %v", err)
	}
	insert := func(id string, ms int64) {
		_, err := db.Exec(`INSERT INTO tasks (task_id, type, profile_name, ref, status, message, created_at, updated_at)
			VALUES (?, 'fetch', '', '', 'success', '', ?, ?)`, id, ms, ms)
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("old", time.Now().AddDate(0, 0, -40).UnixMilli())
	insert("fresh", time.Now().Add(-2*time.Hour).UnixMilli())
	_ = db.Close()

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

	loadTaskStore(logger)
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

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
