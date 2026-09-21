package main

import (
	"context"
	"io"
	"testing"

	"minimax_pro/internal/logx"
)

func TestIsUndetectableNotStarted(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"Undetectable 未启动，任务暂停执行（熔断中，稍后自动恢复）", true},
		{"Undetectable 未启动（未配置 UNDETECTABLE_EXE 自动拉起），任务暂停执行", true},
		{"启动Undetectable失败: exec: file does not exist", true},
		{"已尝试启动Undetectable，但在超时时间内API仍不可用", true},
		{"publish button not ready within timeout", false},
		{"视频下载失败: context deadline exceeded", false},
		{"获取并发额度失败: context canceled", false},
	}
	for _, c := range cases {
		if got := isUndetectableNotStarted(c.msg); got != c.want {
			t.Errorf("isUndetectableNotStarted(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

// TestFinalizeAsyncTaskPaused 验证「未启动」失败会被标 paused 而非 failed。
func TestFinalizeAsyncTaskPaused(t *testing.T) {
	oldRecords, oldCancels, oldCleared := taskRecords, taskCancels, clearedTasks
	taskRecordsMu.Lock()
	taskRecords = make(map[string]*TaskRecord)
	taskCancels = make(map[string]context.CancelFunc)
	clearedTasks = make(map[string]bool)
	taskRecordsMu.Unlock()
	defer func() {
		taskRecordsMu.Lock()
		taskRecords, taskCancels, clearedTasks = oldRecords, oldCancels, oldCleared
		taskRecordsMu.Unlock()
	}()

	logger := logx.New(io.Discard)
	defer logger.Close()

	taskID, _ := registerTask("facebook_publish", "fb1", "MoveTask:1", "b1", "{}")
	finalizeAsyncTask(logger, taskID, "facebook_publish", "fb1", "MoveTask:1", "failed",
		"Undetectable 未启动，任务暂停执行（熔断中，稍后自动恢复）", nil)

	taskRecordsMu.Lock()
	rec := taskRecords[taskID]
	taskRecordsMu.Unlock()
	if rec == nil || rec.Status != taskStatusPaused {
		t.Fatalf("期望 paused，实际 %+v", rec)
	}
}
