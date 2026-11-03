package metrics

import (
	"testing"
	"time"
)

func TestRecorderCounts(t *testing.T) {
	r := New()
	r.Invoke("g1", "actA", 100)
	r.Invoke("g1", "actA", 50)
	r.Invoke("g1", "actB", 10)
	r.Invoke("g2", "actA", 5)
	r.Finish("g1", "actA", Success, 200, 180)
	r.Finish("g1", "actA", Success, 300, 260)
	r.Finish("g1", "actA", Fail, 10, 9)
	r.Finish("g1", "actB", SysErr, 0, 0)
	r.Finish("g2", "actA", Success, 8, 8)

	snap := r.Snapshot(nil, nil)
	if len(snap.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(snap.Groups))
	}
	var g1 *GroupJSON
	for i := range snap.Groups {
		if snap.Groups[i].Group == "g1" {
			g1 = &snap.Groups[i]
		}
	}
	if g1 == nil {
		t.Fatal("group g1 missing")
	}
	if g1.Metrics.Total != 3 {
		t.Errorf("g1 total = %d, want 3", g1.Metrics.Total)
	}
	if g1.Metrics.Success != 2 {
		t.Errorf("g1 success = %d, want 2", g1.Metrics.Success)
	}
	if g1.Metrics.Fail != 1 {
		t.Errorf("g1 fail = %d, want 1", g1.Metrics.Fail)
	}
	if g1.Metrics.SysErr != 1 {
		t.Errorf("g1 sysErr = %d, want 1", g1.Metrics.SysErr)
	}
	if got := g1.Metrics.SuccessRate; got != 2.0/3.0 {
		t.Errorf("g1 successRate = %v, want %v", got, 2.0/3.0)
	}
	if g1.Metrics.BytesInKB != 160.0/1024 {
		t.Errorf("bytesInKB = %v, want %v", g1.Metrics.BytesInKB, 160.0/1024)
	}
	if g1.Metrics.BytesOutKB != 510.0/1024 {
		t.Errorf("bytesOutKB = %v, want %v", g1.Metrics.BytesOutKB, 510.0/1024)
	}
	if len(g1.Actions) != 2 || g1.Actions[0].Action != "actA" || g1.Actions[1].Action != "actB" {
		t.Errorf("g1 actions unexpected: %+v", g1.Actions)
	}
	// 序列：当前小时桶 total=3
	if got := g1.Series[len(g1.Series)-1]; got != 3 {
		t.Errorf("g1 last hour series = %d, want 3", got)
	}
}

func TestRecorderQPSSecondBuckets(t *testing.T) {
	r := New()
	for i := 0; i < 120; i++ {
		r.Invoke("g", "a", 0)
	}
	qps := r.QPS()
	if qps != 2.0 { // 120 次 / 60s
		t.Errorf("qps = %v, want 2.0", qps)
	}
}

func TestNilRecorderSafe(t *testing.T) {
	var r *Recorder
	r.Invoke("g", "a", 1) // 不应 panic
	r.Finish("g", "a", Success, 1, 1)
	if r.QPS() != 0 {
		t.Error("nil qps != 0")
	}
	if s := r.Snapshot(nil, nil); s == nil || s.Groups == nil {
		t.Error("nil snapshot invalid")
	}
}

func TestHourlyAndDailySeries(t *testing.T) {
	r := New()
	// 直接操作内部结构模拟跨小时数据：当前小时 2 条 + 25 小时前 1 条
	now := time.Now()
	curH := now.Unix() / 3600
	r.Invoke("g", "a", 0)
	r.Invoke("g", "a", 0)
	r.mu.Lock()
	e := r.entityLocked("g", "a")
	e.hourLocked(curH-25).Total++                                    // 小时桶：超出 24 桶窗口
	e.dayLocked(localDayStart(now).AddDate(0, 0, -1).Unix()).Total++ // 天桶：昨天的 1 条
	r.mu.Unlock()

	snap := r.Snapshot(nil, nil)
	if len(snap.Hourly.Buckets) != 24 {
		t.Fatalf("hourly buckets = %d, want 24", len(snap.Hourly.Buckets))
	}
	// 25 小时前超出 24 桶窗口，不出现
	var sum int64
	for _, v := range snap.Hourly.Total {
		sum += v
	}
	if sum != 2 {
		t.Errorf("hourly total sum = %d, want 2（25h 前的桶不在 24h 窗口内）", sum)
	}
	if got := snap.Hourly.Total[23]; got != 2 {
		t.Errorf("current hour bucket = %d, want 2", got)
	}
	// 天序列 7 天窗口：今天 2 条 + 昨天（25h 前）1 条
	if len(snap.Daily.Buckets) != 7 {
		t.Fatalf("daily buckets = %d, want 7", len(snap.Daily.Buckets))
	}
	var dsum int64
	for _, v := range snap.Daily.Total {
		dsum += v
	}
	if dsum != 3 {
		t.Errorf("daily total sum = %d, want 3（含昨天 1 条）", dsum)
	}
}

func TestGroupClientsCallback(t *testing.T) {
	r := New()
	r.Invoke("g", "a", 0)
	snap := r.Snapshot(func(group string) int {
		if group == "g" {
			return 3
		}
		return 0
	}, nil)
	if snap.Groups[0].Clients != 3 {
		t.Errorf("clients = %d, want 3", snap.Groups[0].Clients)
	}
}

func TestEmptyGroupIgnored(t *testing.T) {
	r := New()
	r.Invoke("", "a", 0) // group 为空不记
	snap := r.Snapshot(nil, nil)
	if len(snap.Groups) != 0 {
		t.Errorf("groups = %d, want 0", len(snap.Groups))
	}
}

func TestIdleEntityEvicted(t *testing.T) {
	r := New()
	r.Invoke("stale", "random-action", 0)
	r.Invoke("live", "a", 0)

	r.mu.Lock()
	r.ents[key{"stale", "random-action"}].lastSeen =
		time.Now().Add(-entityIdleTTL - time.Minute).Unix()
	r.writes = gcEvery - 1
	r.mu.Unlock()

	r.Invoke("live", "a", 0) // 触发惰性 GC；活跃维度刷新 lastSeen
	r.mu.Lock()
	_, stale := r.ents[key{"stale", "random-action"}]
	_, live := r.ents[key{"live", "a"}]
	r.mu.Unlock()
	if stale {
		t.Fatal("静默超过保留窗口的 group/action 维度未被淘汰")
	}
	if !live {
		t.Fatal("活跃维度被误删")
	}
}

// 序列桶必须统一是 unix 秒：小时桶为整点，天桶为本地 0 点。
// 回归背景：buildSeries 曾对天桶也乘 3600，天图时间轴被渲染成 2174 年。
func TestSeriesBucketsAreUnixSeconds(t *testing.T) {
	r := New()
	r.Invoke("g", "a", 0)
	now := time.Now()
	snap := r.Snapshot(nil, nil)

	hour := snap.Hourly.Buckets[len(snap.Hourly.Buckets)-1]
	if want := now.Unix() / 3600 * 3600; hour != want {
		t.Fatalf("hourly 末桶 = %d, want %d（整点 unix 秒）", hour, want)
	}
	for _, b := range snap.Hourly.Buckets {
		if b <= 0 || b > now.Unix()+3600 {
			t.Fatalf("hourly 桶 %d 不是合理的 unix 秒", b)
		}
	}

	day := snap.Daily.Buckets[len(snap.Daily.Buckets)-1]
	if want := localDayStart(now).Unix(); day != want {
		t.Fatalf("daily 末桶 = %d, want %d（本地 0 点 unix 秒）", day, want)
	}
	for _, b := range snap.Daily.Buckets {
		if b <= 0 || b > now.Unix()+1 {
			t.Fatalf("daily 桶 %d 不是合理的 unix 秒（疑似被乘了 3600）", b)
		}
		if y := time.Unix(b, 0).Year(); y != now.Year() {
			t.Fatalf("daily 桶 %d 落在 %d 年，want %d 年", b, y, now.Year())
		}
	}
}

// 小时桶与天桶的保留窗口相互独立：小时只覆盖 24h 图（26h 预算），天覆盖 7d 图。
// 不能用「小时桶留满 7 天、天桶现算」的方式实现——那会把单维度桶数从 34 拉到
// 168，内存放大约 4 倍。这条断言同时是内存预算的护栏。
func TestRetentionBudget(t *testing.T) {
	if hourRetentionHours < 24 || hourRetentionHours > 30 {
		t.Fatalf("小时桶保留 %dh：需覆盖 24h 图，且不应超出太多", hourRetentionHours)
	}
	if dayRetentionDays < 7 {
		t.Fatalf("天桶保留 %d 天：需覆盖 7d 图", dayRetentionDays)
	}
	if total := hourRetentionHours + int64(dayRetentionDays); total > 40 {
		t.Fatalf("单维度桶预算 %d 超出 40 个（内存放大的主要来源）", total)
	}
}

func TestHourAndDayBucketsPrunedIndependently(t *testing.T) {
	r := New()
	now := time.Now()
	curH := now.Unix() / 3600
	today := localDayStart(now)
	d6 := today.AddDate(0, 0, -6).Unix() // 6 天前：仍在 7 天窗口
	d9 := today.AddDate(0, 0, -9).Unix() // 9 天前：超出窗口

	r.Invoke("g", "a", 0)
	r.mu.Lock()
	e := r.entityLocked("g", "a")
	e.hourLocked(curH-30).Total++ // 30h 前：超出小时窗口
	e.dayLocked(d6).Total++       // 6 天前的天桶
	e.dayLocked(d9).Total++       // 9 天前的天桶
	r.writes = gcEvery - 1
	r.mu.Unlock()

	r.Invoke("g", "a", 0) // 触发惰性 GC
	r.mu.Lock()
	_, hourKept := e.hours[curH-30]
	_, dayKept := e.days[d6]
	_, dayDropped := e.days[d9]
	r.mu.Unlock()
	if hourKept {
		t.Fatal("30h 前的小时桶未被清理")
	}
	if !dayKept {
		t.Fatal("6 天前的天桶被误删（7d 图会缺数据）")
	}
	if dayDropped {
		t.Fatal("9 天前的天桶未被清理")
	}

	// 天序列求和应覆盖这 6 天前的数据
	var sum int64
	for _, v := range r.Snapshot(nil, nil).Daily.Total {
		sum += v
	}
	if sum != 3 {
		t.Fatalf("daily sum = %d, want 3（当前 2 条 + 6 天前 1 条）", sum)
	}
}

// 全局原因分布必须包含 group 为空的错误，口径与 /op/api/errors 的 total 一致。
func TestGlobalReasonsIncludeGroupless(t *testing.T) {
	r := New()
	r.RecordSysErr("g1", "a1", ErrTimeout, "timeout", "c1", "http", "")
	r.RecordSysErr("", "", ErrAbort, "", "", "http", "") // 调用方断连：无 group
	snap := r.Snapshot(nil, nil)
	if snap.SysErrReasons[ErrAbort] != 1 {
		t.Fatalf("global reasons = %v，无 group 的 caller-abort 被漏计", snap.SysErrReasons)
	}
	if e := r.Errors(); e.Total != 2 {
		t.Fatalf("errors total = %d, want 2", e.Total)
	}
	var sum int64
	for _, n := range snap.SysErrReasons {
		sum += n
	}
	if sum != 2 {
		t.Fatalf("全局原因求和 = %d, want 2（应与 /op/api/errors total 一致）", sum)
	}
}
