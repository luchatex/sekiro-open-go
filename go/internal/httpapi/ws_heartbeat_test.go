package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/protocol"
)

// 应用层心跳：浏览器无法发 WS Ping，静默 provider 在半开链路上永远发现不了
// 断开。服务端在注册带 hb=1 时下发能力通告并回执心跳；未声明的连接保持
// 旧行为（无 seq 帧一律 4400 关闭），保证老客户端零影响。

func newHeartbeatServer(t *testing.T) *httptest.Server {
	t.Helper()
	withIdleParams(t, 1*time.Second, 5*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := group.NewManager(log)
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, true, ""))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)
	return srv
}

// readFrame 带超时读一帧文本（心跳场景下服务端不一定有业务帧，不能死等）。
func readFrame(t *testing.T, conn *websocket.Conn) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	return string(data), err
}

func TestWSHeartbeatHandshake(t *testing.T) {
	srv := newHeartbeatServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + srv.URL[len("http"):] + "/register?group=hb&clientId=dev-hb&hb=1"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	// 1) 声明 hb=1 → 立刻收到能力通告
	hello, err := readFrame(t, conn)
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if !strings.Contains(hello, protocol.WSServerHelloKey) {
		t.Fatalf("第一帧应为能力通告，实际: %q", hello)
	}

	// 2) 心跳帧（无 __sekiro_seq__）应被回执，且连接保持
	for i := 0; i < 2; i++ {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"__sekiro_hb__":123}`)); err != nil {
			t.Fatalf("write heartbeat: %v", err)
		}
		ack, err := readFrame(t, conn)
		if err != nil {
			t.Fatalf("read ack #%d: %v", i, err)
		}
		if !strings.Contains(ack, protocol.WSHeartbeatKey) || !strings.Contains(ack, "123") {
			t.Fatalf("回执应原样回显时间戳，实际: %q", ack)
		}
	}
}

// 未声明 hb=1 的连接必须保持旧行为：无 seq 的帧 → 4400 关闭。
// （老客户端不知道有心跳，若服务端放宽会改变其语义）
func TestWSSeqlessFrameStillClosesWithoutDeclaration(t *testing.T) {
	srv := newHeartbeatServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + srv.URL[len("http"):] + "/register?group=hb2&clientId=dev-plain"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"__sekiro_hb__":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("未声明 hb 的连接发无 seq 帧应被关闭")
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) || int(ce.Code) != wsCloseBadRequest {
		t.Fatalf("期望 4400 关闭，实际: %v", err)
	}
}

// 心跳帧不应污染 invoke 链路：发心跳后仍能正常被派发调用。
func TestWSHeartbeatDoesNotBreakInvoke(t *testing.T) {
	srv := newHeartbeatServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + srv.URL[len("http"):] + "/register?group=hb3&clientId=dev-inv&hb=1"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	if _, err := readFrame(t, conn); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"__sekiro_hb__":1}`)); err != nil {
		t.Fatalf("write hb: %v", err)
	}
	if _, err := readFrame(t, conn); err != nil { // ack
		t.Fatalf("read ack: %v", err)
	}

	// 服务端下发 invoke：读一帧应能拿到带 seq 的请求；回一个合法响应，
	// 让 HTTP 调用方立刻返回（否则要等 15s 超时，测试会莫名变慢）
	type frame struct {
		req string
		err error
	}
	got := make(chan frame, 1)
	go func() {
		req, err := readFrame(t, conn)
		got <- frame{req, err}
	}()
	go func() {
		resp, err := http.Get(srv.URL + "/business/invoke?group=hb3&action=ping&bind_client=dev-inv")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	select {
	case f := <-got:
		if f.err != nil {
			t.Fatalf("读取 invoke 请求失败: %v", f.err)
		}
		if !strings.Contains(f.req, protocol.WSSeqNumber) {
			t.Fatalf("应收到带 seq 的 invoke 请求，实际: %q", f.req)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(f.req), &m); err == nil {
			if seq, ok := m[protocol.WSSeqNumber].(float64); ok {
				body, _ := json.Marshal(map[string]any{
					protocol.WSSeqNumber: int(seq), "status": 0, "data": "pong",
				})
				_ = conn.Write(context.Background(), websocket.MessageText, body)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到 invoke 请求")
	}
}
