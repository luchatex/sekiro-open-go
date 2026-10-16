package server

import (
	"net"
	"sync"
	"testing"
	"time"
)

// Feed 发生在 Close 之后：连接应被关闭而非 panic 或永久阻塞。
func TestFeedAfterClose(t *testing.T) {
	l := newConnListener()
	l.Close()
	done := make(chan struct{})
	_, c := net.Pipe()
	go func() {
		l.Feed(c)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Feed blocked after Close")
	}
	c.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("expected conn closed when fed to closed listener")
	}
}

// Accept 在 Close 后应返回 net.ErrClosed。
func TestAcceptAfterClose(t *testing.T) {
	l := newConnListener()
	l.Close()
	if _, err := l.Accept(); err != net.ErrClosed {
		t.Fatalf("expected net.ErrClosed, got %v", err)
	}
}

// 并发 Feed + Close：修复前 Feed 在锁外向已 close 的 channel 发送，
// 触发 send on closed channel panic。反复压测该时序窗口。
func TestConcurrentFeedClose(t *testing.T) {
	for i := 0; i < 100; i++ {
		l := newConnListener()
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c1, c2 := net.Pipe()
				l.Feed(c1)
				c1.Close()
				c2.Close()
			}()
		}
		l.Close()
		wg.Wait()
	}
}
