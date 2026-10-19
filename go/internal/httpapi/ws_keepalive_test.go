package httpapi

// WS provider 心跳保活回归。
//
// 根因：coder/websocket 在 conn.Read 内部自动回 ping/消费 pong，但
// 不会解除 Read 阻塞；若 lastRead 只按数据帧更新，静默 provider
// （js-web 注册后无业务）会在 ServerReadIdle(30s) 后被读空闲看护
// 误杀 → js-web 表现为频繁重连循环。
// 修复：心跳 Ping 成功（收到 pong）即刷新 lastRead，对齐 Java 版
// Netty IdleStateHandler 的 pong 重置语义。

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/protocol"
)

// withIdleParams 注入短空闲周期（真实 30s/20s 下测试无法接受）。
// 底层为原子存储：遗留 goroutine 的读取与恢复写入不构成数据竞争。
func withIdleParams(t *testing.T, writeIdle, readIdle time.Duration) {
	t.Helper()
	oldW, oldR := protocol.ServerWriteIdle(), protocol.ServerReadIdle()
	protocol.SetServerIdle(writeIdle, readIdle)
	t.Cleanup(func() { protocol.SetServerIdle(oldW, oldR) })
}

func dialWSProvider(t *testing.T, srv *httptest.Server, g, id string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + srv.URL[len("http"):] + "/register?group=" + g + "&clientId=" + id
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	// 必须有读循环：coder/websocket 的 pong 响应发生在 Read 内部
	// （对 ping 的 pong 自动回复也如此），客户端不读则服务端 ping
	// 永远等不到 pong。真实 js-web SDK 必然有读循环（收 invoke）。
	go func() {
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// TestSilentWSProviderKeptAlive 核心回归：
// 静默 provider（从不发数据帧）在多个读空闲周期后仍必须在线——
// 保活来自服务端心跳 ping 与其 pong。
func TestSilentWSProviderKeptAlive(t *testing.T) {
	withIdleParams(t, 200*time.Millisecond, 1*time.Second)

	log := discardLogger()
	mgr := group.NewManager(log)
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, false, ""))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)

	_ = dialWSProvider(t, srv, "keepalive", "silent-1")

	// 静默 3s = 3 个读空闲周期 + 15 个心跳周期。
	// 修复前：lastRead 不被 pong 刷新，1s 后即被 closeNow 踢出注册表。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}

	ids := mgr.EnabledClientIDs("keepalive")
	if len(ids) != 1 || ids[0] != "silent-1" {
		t.Fatalf("静默 provider 被误杀：在线 = %v, want [silent-1]（修复前 1s 内被踢，js-web 频繁重连的根因）", ids)
	}
	t.Log("KEEPALIVE: 静默 provider 存活 3s（3 个读空闲周期），心跳 pong 保活生效")
}

// TestDeadWSProviderReclaimed 对照组：读空闲看护本身必须工作——
// 让心跳周期大于读空闲（无 pong 保活），静默连接应被及时回收，
// 防止「保活修复」把看护彻底弄哑。
func TestDeadWSProviderReclaimed(t *testing.T) {
	withIdleParams(t, 10*time.Second /* 心跳几乎不触发 */, 500*time.Millisecond)

	log := discardLogger()
	mgr := group.NewManager(log)
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, false, ""))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)

	_ = dialWSProvider(t, srv, "dead", "silent-2")
	time.Sleep(2 * time.Second) // 4 个读空闲周期，无任何保活

	if ids := mgr.EnabledClientIDs("dead"); len(ids) != 0 {
		t.Fatalf("无保活的静默连接未被回收：在线 = %v, want 空", ids)
	}
	t.Log("RECLAIM: 无 pong 保活的静默连接被读空闲看护及时回收")
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
