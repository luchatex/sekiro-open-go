package server

// 优雅关停验证：ctx 取消后 Run 应正常返回 nil，在途请求完成，
// 且监听端口不再接受新连接。

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"sekiro-go/internal/config"
)

// waitListen 等待 Run 完成监听，返回可拨号的 127.0.0.1 地址。
// Run 绑定所有网卡（Addr 为 [::]:port），直接拼 URL 会走系统代理取不到
// 本机服务，故统一换成 127.0.0.1。
func waitListen(t *testing.T, s *Server) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != nil {
			ta, ok := a.(*net.TCPAddr)
			if !ok {
				t.Fatalf("unexpected listener addr type: %T", a)
			}
			return "127.0.0.1:" + strconv.Itoa(ta.Port)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not start listening")
	return ""
}

func TestRunGracefulShutdown(t *testing.T) {
	s, err := New(&config.Config{Port: 0, OpDisabled: true})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	addr := waitListen(t, s)
	// 关停前服务可用
	resp, err := http.Get("http://" + addr + "/business/groupList")
	if err != nil {
		t.Fatalf("request before shutdown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status before shutdown: %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run should return nil on graceful shutdown, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	// 关停后端口应拒绝连接（listener 已关闭）
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener should be closed after shutdown")
	}
}
