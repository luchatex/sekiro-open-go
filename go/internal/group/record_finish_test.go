package group

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"sekiro-go/internal/metrics"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// 回归测试（真实 Java SDK 联调发现）：一次性响应送达后 Record 必须终结——
// 停掉超时定时器并从 pending 移除。否则响应完成后定时器仍空转，
// 超时后二次写响应（HTTP gzip 场景会把未压缩数据写进已关闭的压缩流）。
func TestRecordFinishOnOnceResponse(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("t-group")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	sink := newFakeSink()
	params := orderedjson.New()
	rec := NewRecord(params, sink, -1, 600*time.Millisecond) // >500ms 生效
	rec.attached(c, 7)

	// 一次性响应（无 SEGMENT_STREAM_FLAG）→ 应终结
	rec.deliver(&protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: 7,
		Headers: protocol.NewHeaders(), Data: []byte(`{"ok":1}`)})

	if len(sink.packets) != 1 {
		t.Fatalf("packets = %d, want 1", len(sink.packets))
	}
	// pending 应已清理
	c.mu.Lock()
	_, stillPending := c.pending[7]
	c.mu.Unlock()
	if stillPending {
		t.Fatal("record still in pending after once-response")
	}

	// 等待超过超时时间：不得有第二次响应（timeout 路径应被取消）
	time.Sleep(900 * time.Millisecond)
	if n := len(sink.packets) + len(sink.common); n != 1 {
		t.Fatalf("sink got %d responses total, want 1 (timer not cancelled after once-response)", n)
	}
}

// 分段流首包不终结，终结包（无 flag）终结。
func TestAttachedAfterFinishDoesNotCreateTimer(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("attach-after-finish")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())
	rec := NewRecord(orderedjson.New(), newFakeSink(), -1, time.Hour)

	// Disconnect 可在 pending 已登记、attached 尚未执行时终结 Record。
	// 已终结的 Record 不得再创建长定时器保留 client/sink/params。
	rec.fail("sekiro upstream lose connection", c.ID)
	rec.attached(c, 9)
	rec.mu.Lock()
	timer := rec.timer
	rec.mu.Unlock()
	if timer != nil {
		t.Fatal("Record 终结后 attached 仍创建了超时定时器")
	}
}

func TestRecordFinishOnSegmentTerminator(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("t-group2")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	sink := newFakeSink()
	rec := NewRecord(orderedjson.New(), sink, -1, 600*time.Millisecond)
	rec.attached(c, 8)

	mkSeg := func(data string) *protocol.Packet {
		h := protocol.NewHeaders()
		h.Set(protocol.SegmentStreamFlag, "true")
		return &protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: 8, Headers: h, Data: []byte(data)}
	}

	rec.deliver(mkSeg("part1"))
	rec.deliver(mkSeg("part2"))
	if len(sink.packets) != 2 {
		t.Fatalf("packets = %d, want 2 (segment packets must not finish record)", len(sink.packets))
	}
	// 终结包：无 flag
	rec.deliver(&protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: 8,
		Headers: protocol.NewHeaders(), Data: []byte("")})
	if len(sink.packets) != 3 {
		t.Fatalf("packets = %d, want 3", len(sink.packets))
	}
	c.mu.Lock()
	_, stillPending := c.pending[8]
	c.mu.Unlock()
	if stillPending {
		t.Fatal("record still in pending after segment terminator")
	}

	time.Sleep(900 * time.Millisecond)
	if n := len(sink.packets) + len(sink.common); n != 3 {
		t.Fatalf("sink got %d responses total, want 3 (timer not cancelled after terminator)", n)
	}
}

func TestEmptyFlaggedSegmentTerminatesRecord(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("empty-segment")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())
	sink := newFakeSink()
	rec := NewRecord(orderedjson.New(), sink, -1, 600*time.Millisecond)
	rec.attached(c, 10)
	c.mu.Lock()
	c.pending[10] = rec
	c.mu.Unlock()

	h := protocol.NewHeaders()
	h.Set(protocol.SegmentStreamFlag, "true")
	rec.deliver(&protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: 10, Headers: h})

	rec.mu.Lock()
	done := rec.done
	rec.mu.Unlock()
	if !done {
		t.Fatal("带分段标记的空终结包未终结 Record")
	}
	c.mu.Lock()
	_, pending := c.pending[10]
	c.mu.Unlock()
	if pending {
		t.Fatal("带分段标记的空终结包仍残留 pending")
	}

	time.Sleep(900 * time.Millisecond)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.common) != 0 {
		t.Fatalf("空终结包之后仍触发 timeout: %+v", sink.common)
	}
}

// mutatingSink 模拟 invokerSink：转发时原地压缩 p.Data。
type mutatingSink struct{ *fakeSink }

func (s *mutatingSink) RespondPacket(p *protocol.Packet) {
	p.Data = []byte("compressed")
	s.fakeSink.RespondPacket(p)
}

// TestMetricUsesLogicalBytesBeforeSinkMutation 指标必须在 sink 改写 p.Data
// 之前采集，否则 invoker 通道会把压缩后字节记成逻辑流量，并误判 status。
func TestMetricUsesLogicalBytesBeforeSinkMutation(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("metric-logical")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	params := orderedjson.New()
	params.Set(protocol.ParamGroup, "g-metric")
	params.Set(protocol.ParamAction, "a")
	mtr := metrics.New()
	rec := NewRecord(params, &mutatingSink{fakeSink: newFakeSink()}, -1, time.Second)
	rec.Meter = mtr
	rec.attached(c, 1)

	logical := bytes.Repeat([]byte("x"), 1000)
	rec.deliver(&protocol.Packet{
		Type: protocol.STypeInvokeResponse, Seq: 1,
		Headers: protocol.NewHeaders(), Data: logical,
	})

	snap := mtr.Snapshot(nil, nil)
	if len(snap.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(snap.Groups))
	}
	wantKB := float64(len(logical)) / 1024
	if got := snap.Groups[0].Metrics.BytesOutKB; got != wantKB {
		t.Fatalf("bytesOutKB = %v, want logical %v (sink mutation leaked into metric)", got, wantKB)
	}
	if got := snap.Groups[0].Metrics.Success; got != 1 {
		t.Fatalf("success = %d, want 1", got)
	}
}
