package group

// 「迟到响应」与「错误事件参数摘要」的回归测试。
//
// 线上排障实证：某 group 每分钟固定 4 条 getDetailData 超时、持续 30 分钟，
// 仅凭 timeout 计数无法区分「上游慢过阈值」与「上游挂死」。迟到响应统计给出
// 客观依据；参数摘要用于把「反复失败的同一条请求」指认出来。
//
// 迟到统计的入口不是 Record.deliver：超时会把 record 从 pending 摘除，
// 迟到的包根本走不到 deliver，只能由 Client 按序号反查超时表（见
// Client.trackLateTimeout / DropLateResponse）。此处用真实路由路径验证。

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"sekiro-go/internal/metrics"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// 超时终结后 provider 才回包：计一次迟到（多包只计首包），且不写 sink。
func TestLateResponseCountedOnceAfterTimeout(t *testing.T) {
	metrics.Reset()
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("late-group")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	sink := newFakeSink()
	params := orderedjson.New()
	params.Set(protocol.ParamGroup, "late-group")
	params.Set(protocol.ParamAction, "getDetailData")
	rec := NewRecord(params, sink, -1, time.Hour)
	rec.Meter = metrics.Default()
	rec.attached(c, 3)
	c.mu.Lock()
	c.pending[3] = rec
	c.mu.Unlock()

	rec.onTimeout() // 直接触发超时路径，避免用例等待 15s

	late := &protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: 3,
		Headers: protocol.NewHeaders(), Data: []byte(`{"late":1}`)}
	c.HandleResponse(late)
	c.HandleResponse(late) // 分段流的后续包不应重复计数

	if n := len(sink.packets); n != 0 {
		t.Fatalf("迟到回包写入了 sink %d 次，应为 0（调用方已收到 timeout）", n)
	}
	e := metrics.Default().Errors()
	if e.LateTotal != 1 {
		t.Fatalf("lateTotal = %d, want 1（多包只计首包）", e.LateTotal)
	}
	if len(e.Late) != 1 || e.Late[0].Group != "late-group" ||
		e.Late[0].Action != "getDetailData" {
		t.Fatalf("late 聚合 = %+v", e.Late)
	}
	if e.Total != 1 {
		t.Fatalf("errors total = %d, want 1（仅 timeout 那条）", e.Total)
	}
}

// 调用方断连后的回包不是「迟到」：没有诊断价值，也不应污染迟到计数。
func TestLateResponseNotCountedAfterAbort(t *testing.T) {
	metrics.Reset()
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("abort-group")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	params := orderedjson.New()
	params.Set(protocol.ParamGroup, "abort-group")
	params.Set(protocol.ParamAction, "search")
	rec := NewRecord(params, newFakeSink(), -1, time.Hour)
	rec.Meter = metrics.Default()
	rec.attached(c, 4)
	c.mu.Lock()
	c.pending[4] = rec
	c.mu.Unlock()

	if !rec.Abort() {
		t.Fatal("Abort 未赢得终结")
	}
	c.HandleResponse(&protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: 4,
		Headers: protocol.NewHeaders(), Data: []byte("x")})

	if got := metrics.Default().Errors().LateTotal; got != 0 {
		t.Fatalf("lateTotal = %d, want 0（caller-abort 后的回包不算迟到）", got)
	}
}

// 未知序号（客户端自造 / 服务端重启前的旧响应）既不计数也不 panic。
func TestLateResponseUnknownSeqIgnored(t *testing.T) {
	metrics.Reset()
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("unknown-group")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	c.HandleResponse(&protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: 999,
		Headers: protocol.NewHeaders(), Data: []byte("x")})

	if got := metrics.Default().Errors().LateTotal; got != 0 {
		t.Fatalf("lateTotal = %d, want 0", got)
	}
}

// 迟到表必须有界：异常流量（例如全局超时）下不得无界增长。
func TestLateTrackBounded(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("bound-group")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	for i := int32(1); i <= lateTrackCap*3; i++ {
		c.trackLateTimeout(i, "act")
	}
	c.lateMu.Lock()
	n := len(c.lateSeqs)
	c.lateMu.Unlock()
	if n > lateTrackCap {
		t.Fatalf("迟到表条目 %d，超过上限 %d", n, lateTrackCap)
	}
	// 淘汰按插入序：最新的仍在，最旧的已被丢弃
	if _, _, ok := c.takeLate(lateTrackCap * 3); !ok {
		t.Fatal("最新序号不应被淘汰")
	}
	if _, _, ok := c.takeLate(1); ok {
		t.Fatal("最旧序号应已被淘汰")
	}
}

// 参数摘要：开启后错误事件附带业务参数，框架键被剔除。
func TestSysErrParamsDigest(t *testing.T) {
	metrics.SetCaptureParams(true)
	t.Cleanup(func() { metrics.SetCaptureParams(false) })

	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("params-group")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	params := orderedjson.New()
	params.Set(protocol.ParamGroup, "params-group")
	params.Set(protocol.ParamAction, "getDetailData")
	params.Set(protocol.ParamInvokeTimeout, "15000")
	params.Set("docId", "a1b2c3")
	params.Set("url", "https://example.com/detail?id=a1b2c3")

	mtr := metrics.New()
	rec := NewRecord(params, newFakeSink(), -1, time.Hour)
	rec.Meter = mtr
	rec.attached(c, 5)
	rec.onTimeout()

	e := mtr.Errors()
	if len(e.Recent) != 1 {
		t.Fatalf("recent = %d, want 1", len(e.Recent))
	}
	got := e.Recent[0].Params
	if !strings.Contains(got, "a1b2c3") {
		t.Fatalf("params 摘要缺少业务参数: %q", got)
	}
	for _, k := range []string{`"group"`, `"action"`, `"invoke_timeout"`} {
		if strings.Contains(got, k) {
			t.Fatalf("params 摘要不应包含框架键 %s: %q", k, got)
		}
	}
	if !e.CaptureParams {
		t.Fatal("captureParams 应为 true")
	}
}

// 默认关闭时不得采集参数（业务数据不进监控）。
func TestSysErrParamsOffByDefault(t *testing.T) {
	metrics.SetCaptureParams(false)

	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("no-params-group")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	params := orderedjson.New()
	params.Set(protocol.ParamGroup, "no-params-group")
	params.Set("docId", "secret-id")

	mtr := metrics.New()
	rec := NewRecord(params, newFakeSink(), -1, time.Hour)
	rec.Meter = mtr
	rec.attached(c, 6)
	rec.onTimeout()

	e := mtr.Errors()
	if len(e.Recent) != 1 || e.Recent[0].Params != "" {
		t.Fatalf("默认不应采集参数: %+v", e.Recent)
	}
	if e.CaptureParams {
		t.Fatal("captureParams 应为 false")
	}
}
