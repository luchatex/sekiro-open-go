package group

import (
	"log/slog"
	"testing"
	"time"

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
