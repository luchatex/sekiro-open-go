package server

// WS 异常关闭码验证：服务端对非法帧/参数应回合法的应用级关闭码 4400
// （修复前用 HTTP 风格 400，非 RFC 6455 合法关闭码，coder/websocket
// 校验拒绝发送，close 帧根本到不了客户端）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWSCloseCodeOnBadFrame(t *testing.T) {
	_, addr := startTestServer(t)
	conn, _ := dialWSWithCompression(t, addr, "ws-close", "c1", websocket.CompressionDisabled)

	// 发送非文本帧 → 服务端应以 4400 关闭（修复前 400 发不出去，
	// 客户端只能干等读空闲 30s 强关）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatalf("write binary frame: %v", err)
	}
	_, _, err := conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CloseError, got %v", err)
	}
	if ce.Code != 4400 {
		t.Fatalf("close code: got %d, want 4400", ce.Code)
	}
	if ce.Reason != "error ws request: non-text frame" {
		t.Fatalf("close reason: %q", ce.Reason)
	}
}

func TestWSCloseCodeOnBadJSON(t *testing.T) {
	_, addr := startTestServer(t)
	conn, _ := dialWSWithCompression(t, addr, "ws-close2", "c1", websocket.CompressionDisabled)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("not-json")); err != nil {
		t.Fatalf("write bad json: %v", err)
	}
	_, _, err := conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CloseError, got %v", err)
	}
	if ce.Code != 4400 {
		t.Fatalf("close code: got %d, want 4400", ce.Code)
	}
}

func TestWSCloseCodeOnMissingSeq(t *testing.T) {
	_, addr := startTestServer(t)
	conn, _ := dialWSWithCompression(t, addr, "ws-close3", "c1", websocket.CompressionDisabled)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"status":0}`)); err != nil {
		t.Fatalf("write no-seq json: %v", err)
	}
	_, _, err := conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CloseError, got %v", err)
	}
	if ce.Code != 4400 {
		t.Fatalf("close code: got %d, want 4400", ce.Code)
	}
}
