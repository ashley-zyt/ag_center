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

	taskID, _ := registerTask("facebook_publish", "fb1", "MoveTask:1", "b1", "{}", nil)
	finalizeAsyncTask(logger, taskID, "facebook_publish", "fb1", "MoveTask:1", "failed",
		"Undetectable 未启动，任务暂停执行（熔断中，稍后自动恢复）", nil)

	taskRecordsMu.Lock()
	rec := taskRecords[taskID]
	taskRecordsMu.Unlock()
	if rec == nil || rec.Status != taskStatusPaused {
		t.Fatalf("期望 paused，实际 %+v", rec)
	}
}

// TestClaimReplayIdempotent 验证重复重放的幂等性 —— 这是「大量
// 获取并发额度失败: context canceled」的根因防护。
//
// 背景：paused 任务被 /tasks/resume 重放后，若状态仍停留在 paused，
// account_sys 的 MachinePauseMonitor（每 5 分钟一轮）会再次把它当「新暂停」重复重放，
// 同一 taskID 堆出多个 goroutine 抢并发额度；先拿到额度的那条跑完后 finishTask 会
// cancel 掉最后登记的那个 cancel func，其余 goroutine 全部以
// 「获取并发额度失败: context canceled」失败，还会把已成功的记录覆盖成 failed。
func TestClaimReplayIdempotent(t *testing.T) {
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

	// 造一条 paused 的发布任务（模拟 Undetectable 未启动被暂停）
	taskRecordsMu.Lock()
	taskRecords["t1"] = &TaskRecord{
		TaskID: "t1", Type: "tiktok_publish", ProfileName: "tt1",
		Batch: "b1", Payload: "{}", Status: taskStatusPaused,
	}
	taskRecordsMu.Unlock()

	// 第一次重放：应成功，且状态必须立刻离开 paused（否则会被下一轮 resume 重复捞起）
	ctx1, ok := claimReplay("t1")
	if !ok || ctx1 == nil {
		t.Fatal("首次 claimReplay 应成功")
	}
	taskRecordsMu.Lock()
	status := taskRecords["t1"].Status
	taskRecordsMu.Unlock()
	if status != taskStatusQueued {
		t.Fatalf("重放后状态应为 queued（立刻离开 paused），实际 %s", status)
	}

	// 第二次重放（模拟 5 分钟后的自动 resume）：必须被幂等拦下，不能再起 goroutine
	if ctx2, ok2 := claimReplay("t1"); ok2 {
		t.Fatal("已有存活重放时 claimReplay 必须返回 false，否则会重复重放")
	} else if ctx2 != nil {
		t.Fatal("被拦下时不应返回 context")
	}

	// 不存在的任务同样返回 false
	if _, ok3 := claimReplay("not-exist"); ok3 {
		t.Fatal("不存在的 taskID 不应 claim 成功")
	}

	// 任务真正结束（finishTask 释放 cancel）后，允许再次 claim（例如又失败了要恢复）
	finishTask("t1", taskStatusPaused, "again")
	if ctx4, ok4 := claimReplay("t1"); !ok4 || ctx4 == nil {
		t.Fatal("cancel 已释放后应可再次 claim")
	}
}

// TestResumePausedDedup 验证 resumePausedPublishTasks 会「立刻把重放任务改成 queued」
// 且返回真正入队的条数，重复调用不会再次入队。
//
// 用「payload 无法解析」的发布任务做样本：走 buildPublishExecutor 失败分支，
// 既能验证选中/计数语义，又不会真的起执行 goroutine（避免测试打网络）。
func TestResumePausedDedup(t *testing.T) {
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

	// t1：可重放的发布任务（但 payload 非法 → 解析失败，不起 goroutine）
	// t2：无 payload 的非发布任务（采集），resume 不该碰它
	taskRecordsMu.Lock()
	taskRecords["t1"] = &TaskRecord{
		TaskID: "t1", Type: "tiktok_publish", ProfileName: "tt1",
		Payload: "{bad", Status: taskStatusPaused,
	}
	taskRecords["t2"] = &TaskRecord{
		TaskID: "t2", Type: "fetch", ProfileName: "tt1", Status: taskStatusPaused,
	}
	taskRecordsMu.Unlock()

	if n := resumePausedPublishTasks(logger); n != 1 {
		t.Fatalf("首次 resume 应入队 1 条，实际 %d", n)
	}

	taskRecordsMu.Lock()
	t1Status := taskRecords["t1"].Status
	t2Status := taskRecords["t2"].Status
	taskRecordsMu.Unlock()

	// 关键：重放的任务必须立刻离开 paused，否则下一轮 resume 会重复捞它
	if t1Status != taskStatusFailed {
		t.Fatalf("非法 payload 的发布任务应被收尾为 failed，实际 %s", t1Status)
	}
	// 无 payload 的非发布任务必须原样保持 paused（交给 account_sys 重发）
	if t2Status != taskStatusPaused {
		t.Fatalf("无 payload 的非发布任务应保持 paused，实际 %s", t2Status)
	}
	// 没有 paused 的发布任务可捞了 → 第二次应为 0
	if n := resumePausedPublishTasks(logger); n != 0 {
		t.Fatalf("重复 resume 应入队 0 条，实际 %d", n)
	}
}

// TestRegisterTaskDingtalkNotify 验证钉钉通知配置随任务登记正确存储。
func TestRegisterTaskDingtalkNotify(t *testing.T) {
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

	// 带 notify
	taskID, _ := registerTask("tiktok_publish", "tt1", "", "", "{}", &DingtalkNotify{
		Webhook: "https://oapi.dingtalk.com/robot/send?access_token=abc",
		Keyword: "发布结果",
		Owner:   "张三",
	})
	taskRecordsMu.Lock()
	rec := taskRecords[taskID]
	taskRecordsMu.Unlock()
	if rec == nil {
		t.Fatal("任务未登记")
	}
	if rec.DingtalkWebhook != "https://oapi.dingtalk.com/robot/send?access_token=abc" {
		t.Errorf("webhook 未正确存储: %q", rec.DingtalkWebhook)
	}
	if rec.DingtalkKeyword != "发布结果" {
		t.Errorf("keyword 未正确存储: %q", rec.DingtalkKeyword)
	}
	if rec.DingtalkOwner != "张三" {
		t.Errorf("owner 未正确存储: %q", rec.DingtalkOwner)
	}

	// nil notify（account_sys 下发）应为空
	taskID2, _ := registerTask("facebook_publish", "fb1", "", "", "{}", nil)
	taskRecordsMu.Lock()
	rec2 := taskRecords[taskID2]
	taskRecordsMu.Unlock()
	if rec2.DingtalkWebhook != "" || rec2.DingtalkKeyword != "" || rec2.DingtalkOwner != "" {
		t.Errorf("nil notify 应得空字段，实际 %q/%q/%q", rec2.DingtalkWebhook, rec2.DingtalkKeyword, rec2.DingtalkOwner)
	}
}
