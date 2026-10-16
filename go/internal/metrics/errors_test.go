package metrics

// 系统错误原因聚合与最近事件环形缓冲的回归测试。

import (
	"fmt"
	"testing"
)

func TestRecordSysErrAggregation(t *testing.T) {
	r := New()
	r.RecordSysErr("g1", "a1", ErrTimeout, "timeout", "c1", "http")
	r.RecordSysErr("g1", "a1", ErrTimeout, "timeout", "c1", "http")
	r.RecordSysErr("g1", "a1", ErrDispatch, "pool queue empty", "", "http")
	r.RecordSysErr("g2", "a", ErrAbort, "", "c2", "invoker")

	snap := r.Snapshot(nil, nil)
	if snap.SysErrReasons["timeout"] != 2 || snap.SysErrReasons["dispatch-failed"] != 1 ||
		snap.SysErrReasons["caller-abort"] != 1 {
		t.Fatalf("global reasons = %v", snap.SysErrReasons)
	}
	var g1 *GroupJSON
	for i := range snap.Groups {
		if snap.Groups[i].Group == "g1" {
			g1 = &snap.Groups[i]
		}
	}
	if g1 == nil {
		t.Fatal("g1 missing from snapshot")
	}
	if g1.Metrics.SysErr != 3 {
		t.Fatalf("g1 sysErr = %d, want 3", g1.Metrics.SysErr)
	}
	if g1.SysErrReasons["timeout"] != 2 || g1.SysErrReasons["dispatch-failed"] != 1 {
		t.Fatalf("g1 reasons = %v", g1.SysErrReasons)
	}

	// Errors()：原因计数与排序（次数降序）、事件内容
	e := r.Errors()
	if e.Total != 4 {
		t.Fatalf("errors total = %d, want 4", e.Total)
	}
	if e.Reasons[0].Reason != ErrTimeout || e.Reasons[0].Count != 2 {
		t.Fatalf("reasons[0] = %+v, want timeout×2", e.Reasons[0])
	}
	if e.Recent[0].Reason != ErrAbort || e.Recent[0].Channel != "invoker" {
		t.Fatalf("recent[0] = %+v, want newest (caller-abort/invoker)", e.Recent[0])
	}
	if e.Recent[1].Message != "pool queue empty" {
		t.Fatalf("recent[1] = %+v", e.Recent[1])
	}
}

func TestSysErrRingCap(t *testing.T) {
	r := New()
	for i := 0; i < 120; i++ {
		r.RecordSysErr("g", "a", ErrTimeout, fmt.Sprintf("m%d", i), "", "http")
	}
	e := r.Errors()
	if len(e.Recent) != errRingCap {
		t.Fatalf("recent len = %d, want %d", len(e.Recent), errRingCap)
	}
	if e.Total != 120 {
		t.Fatalf("total = %d, want 120（聚合不随环形覆盖丢失）", e.Total)
	}
	// 最新在前：第 119 条（m119）应为 Recent[0]，最旧保留的是 m20
	if got := e.Recent[0].Message; got != "m119" {
		t.Fatalf("recent[0] = %q, want m119", got)
	}
	if got := e.Recent[len(e.Recent)-1].Message; got != "m20" {
		t.Fatalf("recent[-1] = %q, want m20", got)
	}
}

func TestSysErrNilReceiverSafe(t *testing.T) {
	var r *Recorder
	r.RecordSysErr("g", "a", ErrTimeout, "timeout", "", "http") // 不应 panic
	e := r.Errors()
	if e == nil || e.Reasons == nil || e.Recent == nil {
		t.Fatal("nil receiver Errors() should return empty slices")
	}
}
