package main

import (
	"context"
	"testing"
	"time"
)

// TestBuildPublishExecutorParses 5 个平台的 payload 都应能正确解析出执行闭包（不真正执行）。
func TestBuildPublishExecutorParses(t *testing.T) {
	logger := newTestLogger(t)

	cases := []struct {
		taskType string
		payload  string
	}{
		{"facebook_publish", `{"profile_name":"fb001","title":"hi","video_oss_url":"http://x/v.mp4"}`},
		{"twitter_publish", `{"profile_name":"tw001","text":"hi","video_oss_url":"http://x/v.mp4"}`},
		{"youtube_publish", `{"profile_name":"yt001","title":"hi","description":"d","video_oss_url":"http://x/v.mp4"}`},
		{"tiktok_publish", `{"profile_name":"tt001","text":"hi","video_oss_url":"http://x/v.mp4"}`},
		{"instagram_publish", `{"profile_name":"ig001","text":"hi","video_oss_url":"http://x/v.mp4"}`},
	}
	for _, c := range cases {
		exec, ok := buildPublishExecutor(context.Background(), c.taskType, c.payload, logger)
		if !ok || exec == nil {
			t.Fatalf("%s 应解析成功", c.taskType)
		}
	}

	if _, ok := buildPublishExecutor(context.Background(), "unknown_type", "{}", logger); ok {
		t.Fatal("未知类型不应解析成功")
	}
	if _, ok := buildPublishExecutor(context.Background(), "facebook_publish", "{bad", logger); ok {
		t.Fatal("非法 JSON 不应解析成功")
	}
}

// TestLoadTaskStoreKeepsQueuedPublish 重启加载时：queued 发布任务（有 payload）应保留 queued，
// 其余 queued / running 任务应标记 interrupted。
func TestLoadTaskStoreKeepsQueuedPublish(t *testing.T) {
	isolateTaskStore(t)
	logger := newTestLogger(t)

	db, err := openTaskStoreDB()
	if err != nil {
		t.Fatalf("openTaskStoreDB: %v", err)
	}
	now := time.Now()
	insert := func(id, typ, status, payload string) {
		_, err := db.Exec(`INSERT INTO tasks (task_id,type,profile_name,ref,batch,payload,status,message,created_at,updated_at)
			VALUES (?,?,'p1','','',?,?,'',?,?)`, id, typ, payload, status, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("q-pub", "facebook_publish", "queued", `{"profile_name":"fb001","title":"hi","video_oss_url":"http://x/v.mp4"}`)
	insert("q-fetch", "fetch", "queued", "")
	insert("running", "twitter_publish", "running", `{"profile_name":"tw001","text":"hi","video_oss_url":"http://x/v.mp4"}`)
	_ = db.Close()

	loadTaskStore(logger)

	if rec := getTaskRecord("q-pub"); rec == nil || rec.Status != taskStatusQueued {
		t.Fatalf("queued 发布任务应保留 queued，实际 %+v", rec)
	}
	if rec := getTaskRecord("q-fetch"); rec == nil || rec.Status != taskStatusInterrupted {
		t.Fatalf("queued fetch 应标记 interrupted，实际 %+v", rec)
	}
	if rec := getTaskRecord("running"); rec == nil || rec.Status != taskStatusInterrupted {
		t.Fatalf("running 应标记 interrupted，实际 %+v", rec)
	}
}
