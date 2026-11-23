package httpapi

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"sekiro-go/internal/protocol"
)

// TestReadBodyUsesFrameLimit 确认 HTTP body 上限来自 protocol 的唯一配置源，
// 不留下独立的 32MB 常量。改 -max-frame-mb 后 HTTP / WS / native 同步生效。
func TestReadBodyUsesFrameLimit(t *testing.T) {
	old := protocol.MaxFrameBytes()
	defer protocol.SetMaxFrameBytes(old)

	protocol.SetMaxFrameBytes(64 << 10)
	req := httptest.NewRequest("POST", "/business/invoke",
		bytes.NewReader(make([]byte, 128<<10)))
	if _, err := readBody(httptest.NewRecorder(), req); err == nil {
		t.Fatal("body 超过帧上限应被拒绝")
	}

	req = httptest.NewRequest("POST", "/business/invoke",
		bytes.NewReader(bytes.Repeat([]byte("x"), 32<<10)))
	if got, err := readBody(httptest.NewRecorder(), req); err != nil || len(got) != 32<<10 {
		t.Fatalf("body 未超限应正常读取: got %d bytes, err=%v", len(got), err)
	}
}
