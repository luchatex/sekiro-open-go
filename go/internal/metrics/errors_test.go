package metrics

// 系统错误原因聚合与最近事件环形缓冲的回归测试。

import (
	"fmt"
	"testing"
	"time"
)

func TestRecordSysErrAggregation(t *testing.T) {
	r := New()
	r.RecordSysErr("g1", "a1", ErrTimeout, "timeout", "c1", "http", "")
	r.RecordSysErr("g1", "a1", ErrTimeout, "timeout", "c1", "http", "")
	r.RecordSysErr("g1", "a1", ErrDispatch, "pool queue empty", "", "http", "")
	r.RecordSysErr("g2", "a", ErrAbort, "", "c2", "invoker", "")

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
		r.RecordSysErr("g", "a", ErrTimeout, fmt.Sprintf("m%d", i), "", "http", "")
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
	r.RecordSysErr("g", "a", ErrTimeout, "timeout", "", "http", "") // 不应 panic
	r.RecordLate("g", "a", time.Second)                             // 不应 panic
	e := r.Errors()
	if e == nil || e.Reasons == nil || e.Recent == nil || e.ByAction == nil || e.Late == nil {
		t.Fatal("nil receiver Errors() should return empty slices")
	}
}

// 聚合视图：同 group/action/原因归一行，按次数降序；事件环滚动不影响聚合。
func TestSysErrByActionAggregation(t *testing.T) {
	r := New()
	for i := 0; i < 7; i++ {
		r.RecordSysErr("wenshudetail", "getDetailData", ErrTimeout, "timeout", "c1", "http", "")
	}
	r.RecordSysErr("wenshudetail", "getDetailData", ErrDispatch, "pool queue empty", "", "http", "")
	r.RecordSysErr("wenshuapp", "search", ErrAbort, "", "c2", "http", "")

	e := r.Errors()
	if len(e.ByAction) != 3 {
		t.Fatalf("byAction 行数 = %d, want 3: %+v", len(e.ByAction), e.ByAction)
	}
	top := e.ByAction[0]
	if top.Group != "wenshudetail" || top.Action != "getDetailData" ||
		top.Reason != ErrTimeout || top.Count != 7 {
		t.Fatalf("byAction[0] = %+v, want wenshudetail/getDetailData/timeout×7", top)
	}
	if top.LastTime == 0 {
		t.Fatal("byAction[0].lastTime 未填充")
	}
	// 聚合合计必须等于错误总数（group 为空的错误不进聚合，故此处只做下限校验）
	var sum int64
	for _, a := range e.ByAction {
		sum += a.Count
	}
	if sum != e.Total {
		t.Fatalf("聚合合计 %d != total %d", sum, e.Total)
	}
}

// 迟到响应：按 group/action 聚合次数与最大延迟，独立于系统错误计数。
func TestRecordLateAggregation(t *testing.T) {
	r := New()
	r.RecordLate("g1", "a1", 2*time.Second)
	r.RecordLate("g1", "a1", 8*time.Second)
	r.RecordLate("g2", "a2", 500*time.Millisecond)
	r.RecordLate("", "a2", time.Second) // group 为空：忽略

	e := r.Errors()
	if e.LateTotal != 3 {
		t.Fatalf("lateTotal = %d, want 3", e.LateTotal)
	}
	if e.Total != 0 {
		t.Fatalf("errors total = %d, want 0（迟到不计系统错误）", e.Total)
	}
	if len(e.Late) != 2 {
		t.Fatalf("late 行数 = %d, want 2: %+v", len(e.Late), e.Late)
	}
	if got := e.Late[0]; got.Group != "g1" || got.Count != 2 || got.MaxDelayMs != 8000 {
		t.Fatalf("late[0] = %+v, want g1/a1 ×2 max 8000ms", got)
	}
}

// 参数摘要开关反映在快照里（默认关）。
func TestCaptureParamsFlag(t *testing.T) {
	t.Cleanup(func() { SetCaptureParams(false) })
	SetCaptureParams(false)
	if CaptureParams() || New().Errors().CaptureParams {
		t.Fatal("默认应为关闭")
	}
	SetCaptureParams(true)
	if !CaptureParams() || !New().Errors().CaptureParams {
		t.Fatal("SetCaptureParams(true) 未生效")
	}
}
