package server

// 首包探测读超时（slowloris 防护）验证：
//   - 连上不发数据的连接应在 sniffTimeout 后被关闭，不再占 goroutine
//   - 慢但在窗口内发出合法请求的连接不能被误杀

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSniffTimeoutClosesIdleConn(t *testing.T) {
	// 缩短探测窗口（启动前指定，保证与 handleConn 读取有 happens-before）
	_, addr := startTestServerWithSniff(t, 300*time.Millisecond)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	start := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var buf [1]byte
	n, err := conn.Read(buf[:])
	if n != 0 || err == nil {
		t.Fatalf("idle conn should be closed by server, got n=%d err=%v", n, err)
	}
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("idle conn closed too late: %v", el)
	}
}

func TestSlowClientWithinSniffTimeout(t *testing.T) {
	_, addr := startTestServerWithSniff(t, 3*time.Second)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 慢速客户端：延迟后仍在窗口内发出合法请求 → 不应被误杀
	time.Sleep(500 * time.Millisecond)
	req := "GET /business/groupList HTTP/1.1\r\nHost: sekiro.test\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(line, "200") {
		t.Fatalf("slow client should be served, status line: %q", line)
	}
}
