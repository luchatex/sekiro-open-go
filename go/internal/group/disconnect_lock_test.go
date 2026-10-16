package group

// Disconnect 锁边界回归：fail（含写回消费方的阻塞 IO）必须在锁外执行。
//
// 修复前的行为：Disconnect 持c.mu 逐个 rec.fail，而 fail 会经
// sink.RespondCommon 写回消费方（HTTP 场景即写 TCP 响应，读端滞后时
// 阻塞）。于是一个慢消费方就能按住整个 client 的锁，期间
// 新到的 provider 响应（HandleResponse 需 c.mu）与新下发调用
// （ForwardInvoke 需 c.mu）全部阻塞——单点慢被放大为provider 整体不可用。

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// blockingSink 在 RespondCommon 上阻塞指定时长，模拟读端滞后的消费方
// （HTTP 响应写不出去时TCP 写阻塞的真实情形）。
type blockingSink struct {
	release   chan struct{}
	blockFor  time.Duration
	failCount int
	mu        sync.Mutex
	once      sync.Once
}

func newBlockingSink(d time.Duration) *blockingSink {
	return &blockingSink{release: make(chan struct{}), blockFor: d}
}

func (s *blockingSink) RespondCommon(int, string, string, int32) {
	s.mu.Lock()
	s.failCount++
	s.mu.Unlock()
	select {
	case <-time.After(s.blockFor):
	case <-s.release:
	}
	s.once.Do(func() {})
}

func (s *blockingSink) RespondPacket(*protocol.Packet) {}
func (s *blockingSink) Closed() <-chan struct{}        { return nil }

// TestDisconnectFailOutsideLock 核心回归：
// 一个 pending 的消费方阻塞 1.5s 时，Disconnect 期间的
// 新调用下发（ForwardInvoke）必须立即完成，不得被拖住。
func TestDisconnectFailOutsideLock(t *testing.T) {
	const blockFor = 1500 * time.Millisecond

	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("disc-lock")
	tr := newFakeTransport()
	c := NewClient(tr, g, "client-a", slog.Default())
	g.register(c)
	waitActor()

	// 一个读端滞后的消费方：pending 中的 record，其 fail 会阻塞 blockFor
	slow := newBlockingSink(blockFor)
	slowRec := NewRecord(orderedjson.New(), slow, -1, 10*time.Second)
	c.ForwardInvoke(slowRec)

	// 触发 Disconnect：锁内摘走 pending 后，锁外 fail 阻塞 blockFor
	disconnected := make(chan struct{})
	go func() {
		c.Disconnect()
		waitActor()
		close(disconnected)
	}()

	// 等 Disconnect 的 actor 回调真正开始（pending 已被摘走）
	time.Sleep(200 * time.Millisecond)

	// 关键断言：此刻发一个新调用，必须在 200ms 内完成
	// （修复前会被慢消费方的 fail 阻塞约 1.3s）
	fast := newFakeSink()
	start := time.Now()
	c.ForwardInvoke(NewRecord(orderedjson.New(), fast, -1, 10*time.Second))
	elapsed := time.Since(start)

	if elapsed > 300*time.Millisecond {
		t.Fatalf("Disconnect 期间的 fail 持锁阻塞了 %v，新调用被拖住（应<300ms）", elapsed)
	}
	t.Logf("LOCK-BOUNDARY: Disconnect 中慢消费方阻塞 %v 时，新调用仅耗时 %v",
		blockFor, elapsed.Round(time.Millisecond))

	// 收尾：等 Disconnect 完成，确认慢消费方最终确实收到失败响应
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect 未在预期时间内完成")
	}
	slow.mu.Lock()
	n := slow.failCount
	slow.mu.Unlock()
	if n != 1 {
		t.Fatalf("慢消费方收到 %d 次失败响应，want 1", n)
	}
}

// TestDisconnectFailsAllPending 功能不变性：
// 锁边界调整后，pending 必须被清空且每条 record 恰好失败一次。
func TestDisconnectFailsAllPending(t *testing.T) {
	const n = 50

	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("disc-all")
	c := NewClient(newFakeTransport(), g, "client-a", slog.Default())
	g.register(c)
	waitActor()

	var sinks []*fakeSink
	for i := 0; i < n; i++ {
		s := newFakeSink()
		sinks = append(sinks, s)
		c.ForwardInvoke(NewRecord(orderedjson.New(), s, -1, 10*time.Second))
	}

	c.Disconnect()
	waitActor()

	c.mu.Lock()
	remain := len(c.pending)
	c.mu.Unlock()
	if remain != 0 {
		t.Fatalf("Disconnect 后 pending 残留 %d 条，want 0", remain)
	}

	failed, twice := 0, 0
	for _, s := range sinks {
		s.mu.Lock()
		cnt := len(s.common)
		s.mu.Unlock()
		switch cnt {
		case 1:
			failed++
		case 0:
			t.Errorf("有消费方未收到失败响应")
		default:
			twice++
		}
	}
	if failed != n || twice != 0 {
		t.Fatalf("failed=%d want %d, 多重响应=%d", failed, n, twice)
	}
	t.Logf("DISCONNECT-ALL: %d 条 pending 全部恰好失败一次", failed)
}
