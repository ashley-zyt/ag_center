package main

// 任务统计与过滤 —— 供 GET /tasks（总览 + 明细）与 GET /tasks/summary（纯统计）共用。
//
// 设计背景：
//   account_sys 需要在自己的后台看到「运营机器上各类任务（采集/养号/发私信/查回复/发布）
//   的执行进度与堆积情况」。为了让它一次请求就能拿到聚合结果、不必拉回几百条明细，
//   这里把过滤与计数逻辑抽出来单独实现，并提供只返回统计的 /tasks/summary。
//
// 口径说明（两个接口刻意不同，避免歧义）：
//   GET /tasks          → status_counts / type_counts / type_status_counts 是**全局口径**
//                         （不受任何 query 过滤影响），用于「这台机器整体怎么样」；
//                         tasks 明细受过滤影响；matched 是过滤命中的**总条数**（截断前）。
//   GET /tasks/summary  → 所有计数都是**过滤后口径**，用于「只看我要的那批任务怎么样」；
//                         profiles 给出堆积落在哪个指纹浏览器上。

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"minimax_pro/internal/logx"
)

// ===== 过滤条件 =====

// taskFilter 任务过滤条件。空集合表示该维度不过滤。
type taskFilter struct {
	statuses    map[string]struct{} // status 精确匹配，逗号分隔多值
	types       map[string]struct{} // type 精确匹配，逗号分隔多值
	profiles    map[string]struct{} // profile_name 精确匹配，逗号分隔多值
	batches     map[string]struct{} // batch 精确匹配，逗号分隔多值
	refPrefixes []string            // ref 前缀匹配，逗号分隔多值，任一命中即可
}

// parseCSVSet 解析逗号分隔参数为集合；空串/全空白返回 nil（表示不过滤）。
func parseCSVSet(raw string) map[string]struct{} {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	set := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			set[p] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// parseCSVList 解析逗号分隔参数为切片（保持顺序），空串返回 nil。
func parseCSVList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseTaskFilter 从 query 解析过滤条件。
//
// 支持的参数：
//
//	status        queued / running / success / failed / interrupted，可逗号分隔多值
//	              （interrupted = 服务重启时被中断的历史任务，只在加载快照后出现）
//	type          fetch / nurture / send_message / check_reply / *_publish，可多值
//	profile_name  指纹浏览器名，可多值
//	batch         批次标识，可多值 —— 用于精确回答「这批任务跑完没有」
//	ref_prefix    ref 前缀，如 "Account:,WarmupTask:" —— 用于只看本系统下发的任务
//	              （外部系统直接调用机器端时 ref 为空，会被前缀过滤自然排除）
func parseTaskFilter(q url.Values) taskFilter {
	return taskFilter{
		statuses:    parseCSVSet(q.Get("status")),
		types:       parseCSVSet(q.Get("type")),
		profiles:    parseCSVSet(q.Get("profile_name")),
		batches:     parseCSVSet(q.Get("batch")),
		refPrefixes: parseCSVList(q.Get("ref_prefix")),
	}
}

// match 判断单条任务是否命中过滤条件。
func (f taskFilter) match(rec *TaskRecord) bool {
	if len(f.statuses) > 0 {
		if _, ok := f.statuses[rec.Status]; !ok {
			return false
		}
	}
	if len(f.types) > 0 {
		if _, ok := f.types[rec.Type]; !ok {
			return false
		}
	}
	if len(f.profiles) > 0 {
		if _, ok := f.profiles[rec.ProfileName]; !ok {
			return false
		}
	}
	if len(f.batches) > 0 {
		if _, ok := f.batches[rec.Batch]; !ok {
			return false
		}
	}
	if len(f.refPrefixes) > 0 {
		hit := false
		for _, p := range f.refPrefixes {
			if strings.HasPrefix(rec.Ref, p) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// ===== 计数 =====

// taskCounters 任务计数容器。
type taskCounters struct {
	Total            int
	StatusCounts     map[string]int            // 各状态数量
	TypeCounts       map[string]int            // 各类型数量
	TypeStatusCounts map[string]map[string]int // 类型 × 状态 交叉（进度与堆积的核心视图）
	ProfileCounts    map[string]map[string]int // profile_name × 状态（堆积落在哪个浏览器）

	BatchCounts     map[string]map[string]int // batch × 状态（「哪一批跑完没有」的核心视图）
	BatchFirstSeen  map[string]time.Time      // 该批最早的提交时间
	BatchLastUpdate map[string]time.Time      // 该批最近一次状态变更时间
	Unbatched       int                       // 未带 batch 的任务数（外部调用或调用方没传）
}

func newTaskCounters() *taskCounters {
	return &taskCounters{
		StatusCounts: map[string]int{
			taskStatusQueued:      0,
			taskStatusRunning:     0,
			taskStatusSuccess:     0,
			taskStatusFailed:      0,
			taskStatusInterrupted: 0,
			taskStatusPaused:      0,
		},
		TypeCounts:       make(map[string]int),
		TypeStatusCounts: make(map[string]map[string]int),
		ProfileCounts:    make(map[string]map[string]int),
		BatchCounts:      make(map[string]map[string]int),
		BatchFirstSeen:   make(map[string]time.Time),
		BatchLastUpdate:  make(map[string]time.Time),
	}
}

func (c *taskCounters) add(rec *TaskRecord) {
	c.Total++
	c.StatusCounts[rec.Status]++
	c.TypeCounts[rec.Type]++

	if c.TypeStatusCounts[rec.Type] == nil {
		c.TypeStatusCounts[rec.Type] = make(map[string]int)
	}
	c.TypeStatusCounts[rec.Type][rec.Status]++

	if rec.ProfileName != "" {
		if c.ProfileCounts[rec.ProfileName] == nil {
			c.ProfileCounts[rec.ProfileName] = make(map[string]int)
		}
		c.ProfileCounts[rec.ProfileName][rec.Status]++
	}

	if rec.Batch == "" {
		c.Unbatched++
		return
	}
	if c.BatchCounts[rec.Batch] == nil {
		c.BatchCounts[rec.Batch] = make(map[string]int)
	}
	c.BatchCounts[rec.Batch][rec.Status]++
	if ts, ok := c.BatchFirstSeen[rec.Batch]; !ok || rec.CreatedAt.Before(ts) {
		c.BatchFirstSeen[rec.Batch] = rec.CreatedAt
	}
	if ts, ok := c.BatchLastUpdate[rec.Batch]; !ok || rec.UpdatedAt.After(ts) {
		c.BatchLastUpdate[rec.Batch] = rec.UpdatedAt
	}
}

// ===== 响应结构 =====

// profileTaskStat 单个指纹浏览器的任务分布（数组形式，便于前端直接渲染表格）。
type profileTaskStat struct {
	ProfileName string `json:"profile_name"`
	Queued      int    `json:"queued"`
	Running     int    `json:"running"`
	Success     int    `json:"success"`
	Failed      int    `json:"failed"`
	Interrupted int    `json:"interrupted"`
	Paused      int    `json:"paused"`
	Total       int    `json:"total"`
}

func (c *taskCounters) profileStats() []profileTaskStat {
	out := make([]profileTaskStat, 0, len(c.ProfileCounts))
	for name, counts := range c.ProfileCounts {
		st := profileTaskStat{
			ProfileName: name,
			Queued:      counts[taskStatusQueued],
			Running:     counts[taskStatusRunning],
			Success:     counts[taskStatusSuccess],
			Failed:      counts[taskStatusFailed],
			Interrupted: counts[taskStatusInterrupted],
			Paused:      counts[taskStatusPaused],
		}
		st.Total = st.Queued + st.Running + st.Success + st.Failed + st.Interrupted + st.Paused
		out = append(out, st)
	}
	// 堆积多的排前面（先按排队数，再按执行中，最后按 profile 名保证顺序稳定）
	sort.Slice(out, func(i, j int) bool {
		if out[i].Queued != out[j].Queued {
			return out[i].Queued > out[j].Queued
		}
		if out[i].Running != out[j].Running {
			return out[i].Running > out[j].Running
		}
		return out[i].ProfileName < out[j].ProfileName
	})
	return out
}

// batchTaskStat 单个批次的任务分布（数组形式，便于前端直接渲染表格）。
//
// Pending = queued + running；Done=true（Pending==0）表示该批已无待执行任务，
// 但**不代表全部成功** —— 仍可能含 failed / interrupted，要看各自计数。
type batchTaskStat struct {
	Batch       string `json:"batch"`
	Queued      int    `json:"queued"`
	Running     int    `json:"running"`
	Success     int    `json:"success"`
	Failed      int    `json:"failed"`
	Interrupted int    `json:"interrupted"`
	Paused      int    `json:"paused"`
	Total       int    `json:"total"`
	Pending     int    `json:"pending"`
	Done        bool   `json:"done"`
	FirstSeen   string `json:"first_seen"`
	LastUpdate  string `json:"last_update"`
	// DurationSeconds = 该批最后一次状态变更 - 首次提交；批还在跑时会持续增长。
	DurationSeconds int `json:"duration_seconds"`
}

// batchStats 按批次聚合，按「最近提交的批」倒序返回；limit<=0 表示不限。
func (c *taskCounters) batchStats(limit int) []batchTaskStat {
	out := make([]batchTaskStat, 0, len(c.BatchCounts))
	for batch, counts := range c.BatchCounts {
		st := batchTaskStat{
			Batch:       batch,
			Queued:      counts[taskStatusQueued],
			Running:     counts[taskStatusRunning],
			Success:     counts[taskStatusSuccess],
			Failed:      counts[taskStatusFailed],
			Interrupted: counts[taskStatusInterrupted],
			Paused:      counts[taskStatusPaused],
		}
		st.Total = st.Queued + st.Running + st.Success + st.Failed + st.Interrupted + st.Paused
		st.Pending = st.Queued + st.Running + st.Paused
		st.Done = st.Pending == 0
		if ts, ok := c.BatchFirstSeen[batch]; ok {
			st.FirstSeen = ts.Format(time.RFC3339)
			if last, ok2 := c.BatchLastUpdate[batch]; ok2 && last.After(ts) {
				st.DurationSeconds = int(last.Sub(ts).Seconds())
			}
		}
		if ts, ok := c.BatchLastUpdate[batch]; ok {
			st.LastUpdate = ts.Format(time.RFC3339)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		ti := c.BatchFirstSeen[out[i].Batch]
		tj := c.BatchFirstSeen[out[j].Batch]
		if !ti.Equal(tj) {
			return ti.After(tj) // 新批在前
		}
		return out[i].Batch < out[j].Batch // 时间相同按名称，保证顺序稳定
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// handleTaskSummary GET /tasks/summary —— 只返回统计、不返回明细。
//
// 与 GET /tasks 的差别：这里所有计数都是**过滤后**口径，方便调用方用 type / ref_prefix
// 只看自己关心的那批任务，一次拿到「进度 + 堆积落点 + 最老排队时长」。
//
// query 参数同 GET /tasks（status / type / profile_name / batch / ref_prefix），无 limit；
// 另支持 batch_limit 控制返回多少个批次的聚合（默认 50，0 = 不限）。
func handleTaskSummary(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		q := r.URL.Query()
		filter := parseTaskFilter(q)

		batchLimit := 50
		if v := strings.TrimSpace(q.Get("batch_limit")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				batchLimit = n
			}
		}

		taskRecordsMu.Lock()
		all := newTaskCounters()     // 全量口径（便于对比「本系统任务 / 全部任务」）
		matched := newTaskCounters() // 过滤后口径
		var oldestQueued *time.Time
		for _, rec := range taskRecords {
			all.add(rec)
			if !filter.match(rec) {
				continue
			}
			matched.add(rec)
			if rec.Status == taskStatusQueued {
				if oldestQueued == nil || rec.CreatedAt.Before(*oldestQueued) {
					t := rec.CreatedAt
					oldestQueued = &t
				}
			}
		}
		taskRecordsMu.Unlock()

		resp := map[string]any{
			"total":              all.Total, // 本机累计接收的任务总数（内存口径）
			"matched":            matched.Total,
			"status_counts":      matched.StatusCounts,
			"type_counts":        matched.TypeCounts,
			"type_status_counts": matched.TypeStatusCounts,
			"profiles":           matched.profileStats(),
			// 按批次聚合：一眼看出「哪一批下发、跑到什么程度、还差几个没跑完」
			"batches":     matched.batchStats(batchLimit),
			"batch_total": len(matched.BatchCounts),
			"unbatched":   matched.Unbatched,
		}
		if oldestQueued != nil {
			resp["oldest_queued_at"] = oldestQueued.Format(time.RFC3339)
			resp["oldest_queued_seconds"] = int(time.Since(*oldestQueued).Seconds())
		}

		writeJSON(w, http.StatusOK, resp)
	}
}
