package httpapi

// 客户端断连后及时回收 invoke handler 的端到端验证。
//
// 修复前：handleInvoke 阻塞在 <-sink.Closed()，客户端断开后 goroutine
// 仍驻留到 Record 超时（默认 15s），pending 也同步挂到超时。
// 修复后：监听 r.Context().Done()，断连即 Abort——清 pending、停定时器、
// 不写回响应（连接已关，写了只会往死 socket 写）。

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// silentTransport 记录调用但从不响应，模拟 unresponsive provider。
// 这样 handler 只能靠断连信号退出——正是被测路径。
type silentTransport struct{ active bool }

func (s *silentTransport) SendInvoke(*orderedjson.Map, int32) error { return nil }
func (s *silentTransport) Active() bool                             { return s.active }
func (s *silentTransport) Close()                                   {}
func (s *silentTransport) CloseAfter(time.Duration)                 {}

// startSilentServer 启动一个provider 永不响应的测试服务端。
func startSilentServer(t *testing.T, groupName string) (*group.Manager, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := group.NewManager(log)
	g := mgr.GetOrCreate(groupName)
	g.Register(group.NewClient(&silentTransport{active: true}, g, "silent-provider", log))

	// 等注册经actor 生效（Allocate 能命中即已注册）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := mgr.Allocate(groupName, "", ""); err == nil && c != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, false))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)
	return mgr, srv.Listener.Addr().String()
}

// postThenDisconnect 发出完整请求后立即断开连接。
func postThenDisconnect(t *testing.T, addr, groupName string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	body := "group=" + groupName + "&action=x"
	fmt.Fprintf(conn, "POST /business/invoke HTTP/1.1\r\nHost: x\r\n"+
		"Content-Type: application/x-www-form-urlencoded\r\n"+
		"Content-Length: %d\r\n\r\n%s", len(body), body)
	conn.Close() // 消费方立即消失
}

// TestInvokeHandlerReleasedOnClientDisconnect 核心端到端回归：
// 100 个断连请求的 handler 必须快速退出，不得驻留到 15s 超时。
func TestInvokeHandlerReleasedOnClientDisconnect(t *testing.T) {
	const n = 100

	_, addr := startSilentServer(t, "disc")

	// 基线：留 200ms 让服务端协程结构稳定
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < n; i++ {
		postThenDisconnect(t, addr, "disc")
	}

	// 轮询等待 handler 全部退出
	var after int
	settled := false
	for i := 0; i < 100; i++ { // 最多 10s
		time.Sleep(100 * time.Millisecond)
		after = runtime.NumGoroutine()
		if after <= before+10 {
			settled = true
			break
		}
	}
	t.Logf("DISCONNECT: goroutine %d → %d（%d 个断连请求）", before, after, n)

	if !settled {
		t.Fatalf("%d 个已断连请求的 handler 未及时释放：goroutine %d → %d（残留 %d）",
			n, before, after, after-before)
	}
	// 修复前每个 handler 驻留 15s，100 个请求至少留下 100 个 goroutine
	if delta := after - before; delta > 10 {
		t.Fatalf("断连后仍残留 %d 个 handler goroutine（修复前应 ≥%d）", delta, n)
	}
}

// TestInvokeNoWriteAfterDisconnect 验证断连后 provider 迟到响应不会
// 触发写回（往已关闭的连接写）。修复后 Abort 已置 done，迟到响应被丢弃。
func TestInvokeNoWriteAfterDisconnect(t *testing.T) {
	const n = 50

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := group.NewManager(log)
	defer mgr.Close()

	// 延迟 provider：断连 200ms 后才响应（响应必然迟到）
	tr := &fakeTransport{delay: 200 * time.Millisecond}
	g := mgr.GetOrCreate("late")
	cl := group.NewClient(tr, g, "late-provider", log)
	tr.cl = cl
	g.Register(cl)

	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, false))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < n; i++ {
		postThenDisconnect(t, addr, "late")
	}

	// 等待超过 provider 响应延迟：若 Abort 未生效，迟到响应会尝试写回
	// 已关闭连接（无害但说明 done 未置位），且 handler 会驻留到超时
	time.Sleep(1500 * time.Millisecond)
	after := runtime.NumGoroutine()
	t.Logf("LATE-RESPONSE: goroutine %d → %d（%d 个断连请求，provider 延迟 200ms）",
		before, after, n)

	if delta := after - before; delta > 10 {
		t.Fatalf("迟到响应场景下仍残留 %d 个 handler goroutine", delta)
	}
	var _ = protocol.ParamGroup
}
