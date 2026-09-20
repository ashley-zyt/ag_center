package main

import (
	"net/url"
	"testing"
	"time"
)

func mkTask(id, typ, profile, ref, status string, ago time.Duration) *TaskRecord {
	t := time.Now().Add(-ago)
	return &TaskRecord{
		TaskID: id, Type: typ, ProfileName: profile, Ref: ref,
		Status: status, CreatedAt: t, UpdatedAt: t,
	}
}

func TestTaskFilterAndCounters(t *testing.T) {
	seed := []*TaskRecord{
		mkTask("t1", "fetch", "ig001", "Account:1", taskStatusQueued, 30*time.Minute),
		mkTask("t2", "fetch", "ig001", "Account:2", taskStatusQueued, 10*time.Minute),
		mkTask("t3", "nurture", "tw001", "WarmupTask:9", taskStatusRunning, time.Minute),
		mkTask("t4", "facebook_publish", "fb001", "MoveTask:7", taskStatusSuccess, 2*time.Minute),
		mkTask("t5", "send_message", "ig001", "", taskStatusSuccess, 3*time.Minute), // 外部任务（无 ref）
	}

	// 1) ref_prefix 过滤：排除外部任务
	f := parseTaskFilter(url.Values{"ref_prefix": {"Account:,WarmupTask:"}})
	var matched []*TaskRecord
	for _, r := range seed {
		if f.match(r) {
			matched = append(matched, r)
		}
	}
	if len(matched) != 3 {
		t.Fatalf("ref_prefix 过滤应命中 3 条，实际 %d", len(matched))
	}
	for _, r := range matched {
		if r.TaskID == "t5" {
			t.Fatal("外部任务(t5, 空 ref) 不应命中 ref_prefix 过滤")
		}
	}

	// 2) 多值 type + status 过滤
	f2 := parseTaskFilter(url.Values{"type": {"fetch,nurture"}, "status": {"queued,running"}})
	n := 0
	for _, r := range seed {
		if f2.match(r) {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("type/status 多值过滤应命中 3 条，实际 %d", n)
	}

	// 3) profile_name 多值过滤
	f3 := parseTaskFilter(url.Values{"profile_name": {"fb001, tw001"}})
	n3 := 0
	for _, r := range seed {
		if f3.match(r) {
			n3++
		}
	}
	if n3 != 2 {
		t.Fatalf("profile_name 多值过滤应命中 2 条，实际 %d", n3)
	}

	// 4) 交叉统计 + profile 聚合
	c := newTaskCounters()
	for _, r := range seed {
		c.add(r)
	}
	if c.Total != 5 {
		t.Fatalf("Total 应为 5，实际 %d", c.Total)
	}
	if c.StatusCounts[taskStatusQueued] != 2 || c.StatusCounts[taskStatusSuccess] != 2 || c.StatusCounts[taskStatusRunning] != 1 {
		t.Fatalf("StatusCounts 异常: %v", c.StatusCounts)
	}
	if c.TypeStatusCounts["fetch"][taskStatusQueued] != 2 {
		t.Fatalf("交叉统计异常: %v", c.TypeStatusCounts["fetch"])
	}
	if c.ProfileCounts["ig001"][taskStatusQueued] != 2 {
		t.Fatalf("profile 聚合异常: %v", c.ProfileCounts["ig001"])
	}

	// 5) profile 排序：排队多的在前
	stats := c.profileStats()
	if len(stats) != 3 || stats[0].ProfileName != "ig001" || stats[0].Queued != 2 {
		t.Fatalf("profileStats 排序异常: %+v", stats)
	}

	// 6) 空值不过滤
	f4 := parseTaskFilter(url.Values{})
	for _, r := range seed {
		if !f4.match(r) {
			t.Fatal("空过滤条件应全部命中")
		}
	}
}
