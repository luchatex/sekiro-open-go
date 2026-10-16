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
	r.Finish("g1", "actA", Success, 200)
	r.Finish("g1", "actA", Success, 300)
	r.Finish("g1", "actA", Fail, 10)
	r.Finish("g1", "actB", SysErr, 0)
	r.Finish("g2", "actA", Success, 8)

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
	r.Finish("g", "a", Success, 1)
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
	e.hourLocked(curH-25).Total++
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
