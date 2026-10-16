package group

// Record.Abort 回归：消费方断开时终结 Record，但不写回任何响应。

import (
	"log/slog"
	"testing"
	"time"

	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// TestAbortSkipsSinkAndClearsPending 核心断言：
// Abort 后 pending 必须清空（否则会挂到超时），且不产生任何响应写入。
func TestAbortSkipsSinkAndClearsPending(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("abort")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())
	g.register(c)
	waitActor()

	sink := newFakeSink()
	rec := NewRecord(orderedjson.New(), sink, -1, 10*time.Second)
	c.ForwardInvoke(rec)

	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("前置条件失败：pending = %d, want 1", n)
	}

	rec.Abort()

	// pending 必须立即清空
	c.mu.Lock()
	n = len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("Abort 后 pending 残留 %d 条，want 0", n)
	}

	// 不得产生任何响应写入（消费方已断开）
	sink.mu.Lock()
	got := len(sink.common) + len(sink.packets)
	sink.mu.Unlock()
	if got != 0 {
		t.Fatalf("Abort 写回了 %d 个响应，消费方已断开时不应写入", got)
	}
	t.Log("ABORT: pending 清空且无响应写入")

	// 后续 provider 响应到达不应重复终结（done 已置位）
	rec.deliver(&protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: rec.seq,
		Headers: protocol.NewHeaders(), Data: []byte(`{"late":1}`)})
	sink.mu.Lock()
	got = len(sink.common) + len(sink.packets)
	sink.mu.Unlock()
	if got != 0 {
		t.Fatalf("Abort 后的迟到响应仍被投递（%d 个）", got)
	}
}

// TestAbortStopsTimer Abort 必须停掉超时定时器，避免 15s 后空转触发
// 二次终结（那会对已断开的连接再写一次响应）。
func TestAbortStopsTimer(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("abort-timer")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	sink := newFakeSink()
	rec := NewRecord(orderedjson.New(), sink, -1, 600*time.Millisecond)
	rec.attached(c, 11)
	rec.Abort()

	time.Sleep(900 * time.Millisecond) // 远超 600ms 超时
	sink.mu.Lock()
	got := len(sink.common) + len(sink.packets)
	sink.mu.Unlock()
	if got != 0 {
		t.Fatalf("Abort 后定时器仍在 %s 后触发，写回了 %d 个响应", "600ms", got)
	}
	t.Log("ABORT-TIMER: 定时器已停，超时后无二次终结")
}

// TestAbortIdempotent 重复 Abort（含与迟到响应并发）必须幂等。
func TestAbortIdempotent(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("abort-idem")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())

	sink := newFakeSink()
	rec := NewRecord(orderedjson.New(), sink, -1, 600*time.Millisecond)
	rec.attached(c, 12)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			rec.Abort()
			rec.fail("concurrent fail", "")
			rec.deliver(&protocol.Packet{Type: protocol.STypeInvokeResponse, Seq: 12,
				Headers: protocol.NewHeaders(), Data: []byte(`{}`)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("并发 Abort/fail/deliver 死锁")
	}

	// 首个 Abort 赢：不应有任何写入
	sink.mu.Lock()
	got := len(sink.common) + len(sink.packets)
	sink.mu.Unlock()
	if got != 0 {
		t.Fatalf("并发场景下仍写回了 %d 个响应", got)
	}
}
