package group

import (
	"log/slog"
	"testing"
	"time"

	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// TestTouchPendingExtendsTimeout 模拟慢页面：600ms 超时，300ms 时发一次
// 进度帧；若不刷新，300+400=700ms 时必然超时。进度帧必须把窗口滚动到
// 最后一次活动 + timeout，并为后续工作者留出完成时间。
func TestTouchPendingExtendsTimeout(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("slow-page")
	tr := newFakeTransport()
	c := NewClient(tr, g, "dev1", slog.Default())
	g.register(c)
	waitActor()

	sink := newFakeSink()
	params, _ := orderedjson.Parse([]byte(`{"group":"slow-page","action":"slow"}`))
	rec := NewRecord(params, sink, -1, 600*time.Millisecond)
	c.mu.Lock()
	c.pending[7] = rec
	c.mu.Unlock()
	rec.attached(c, 7)

	time.Sleep(300 * time.Millisecond)
	if !c.TouchPending(7) {
		t.Fatal("TouchPending(7) = false, want true")
	}
	time.Sleep(400 * time.Millisecond)

	select {
	case <-sink.Closed():
		t.Fatal("进度帧后仍按原超时终结，未刷新等待窗口")
	default:
	}

	p := protocol.NewPacket(protocol.STypeInvokeResponse)
	p.Seq = 7
	p.Data = []byte(`{"status":0,"data":"ok"}`)
	rec.deliver(p)
	if len(sink.common) != 0 {
		t.Fatalf("正常回包不应产生 CommonRes: %+v", sink.common)
	}
}
