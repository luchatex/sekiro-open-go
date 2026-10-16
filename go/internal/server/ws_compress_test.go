package server

// WS permessage-deflate 压缩协商 + 大载荷乱码验证。
//
// 验证三点：
//  1. 支持压缩的客户端协商生效（响应头含 permessage-deflate）
//  2. 3MB 中文大载荷经压缩 WS 通道往返无 U+FFFD、内容逐字节一致
//  3. 不支持压缩的客户端（不带扩展头）仍正常工作（降级兼容）

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialWSWithCompression 用 coder/websocket 拨号注册（支持压缩协商），
// 返回 (conn, 响应头)。
func dialWSWithCompression(t *testing.T, addr, group, id string, mode websocket.CompressionMode) (*websocket.Conn, http.Header) {
	t.Helper()
	wsURL := "ws://" + addr + "/register?group=" + group + "&clientId=" + id
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: mode,
		HTTPClient:      &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn, resp.Header
}

// wsEchoProvider 常驻读循环：收到 S_TYPE_INVOKE(32) 的 JSON 文本帧，
// 以 JSON 文本帧响应 `{"status":0,"data":<payload>}`（对齐 sekiroWeb.js）。
func wsEchoProvider(ctx context.Context, t *testing.T, conn *websocket.Conn, payload string) {
	for {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if mt != websocket.MessageText {
			continue
		}
		req := string(data)
		seqStart := strings.Index(req, `"__sekiro_seq__":`)
		if seqStart < 0 {
			continue
		}
		rest := req[seqStart+len(`"__sekiro_seq__":`):]
		end := strings.IndexAny(rest, ",}")
		if end < 0 {
			continue
		}
		seq := strings.TrimSpace(rest[:end])
		resp := `{"status":0,"__sekiro_seq__":` + seq + `,"data":` + quoteJSON(payload) + `}`
		if err := conn.Write(ctx, websocket.MessageText, []byte(resp)); err != nil {
			return
		}
	}
}

// quoteJSON 最小 JSON 字符串转义（payload 仅含安全字符时直出）。
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestE2E_WSCompression_BigChinesePayload 压缩协商生效 + 3MB 中文无乱码。
func TestE2E_WSCompression_BigChinesePayload(t *testing.T) {
	srv, addr := startTestServer(t)
	_ = srv

	conn, hdr := dialWSWithCompression(t, addr, "ws-comp", "comp-1", websocket.CompressionContextTakeover)
	if ext := hdr.Get("Sec-Websocket-Extensions"); !strings.Contains(ext, "permessage-deflate") {
		t.Fatalf("compression not negotiated, Sec-WebSocket-Extensions=%q", ext)
	}

	// 3MB 中文载荷（1,048,575 汉字 = 3,145,725 字节；长度取 3 的倍数
	// 避免按字符截断时把末尾多字节汉字切成非法 UTF-8 序列）
	unit := "中文字符串测试"
	var big strings.Builder
	for big.Len() < 1048575*3 {
		big.WriteString(unit)
	}
	// 按 rune 截断到 1048575 个汉字
	runes := []rune(big.String())
	payload := string(runes[:1048575])
	raw := []byte(payload)
	if len(raw) != 1048575*3 {
		t.Fatalf("payload byte len = %d, want %d", len(raw), 1048575*3)
	}
	wantHash := sha256.Sum256(raw)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wsEchoProvider(ctx, t, conn, payload)

	// HTTP invoke 往返 3 轮
	client := &http.Client{Timeout: 30 * time.Second}
	for i := 1; i <= 3; i++ {
		resp, err := client.Get("http://" + addr + "/business/invoke?group=ws-comp&action=bigChinese")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("run %d: http %d", i, resp.StatusCode)
		}
		if bad := strings.Count(string(body), "\uFFFD"); bad != 0 {
			t.Fatalf("run %d: found %d replacement chars (U+FFFD)", i, bad)
		}
		// 提取 data 字段内容：一次性透传分支响应格式为
		// {"clientId":"<id>","data":"<payload>","status":0}（对齐原版
		// fastjson toJSONString 默认 SortField → 字典序），data 值内无引号/反斜杠
		marker := `"data":"`
		idx := bytes.Index(body, []byte(marker))
		if idx < 0 {
			t.Fatalf("run %d: no data field", i)
		}
		rest := body[idx+len(marker):]
		endIdx := bytes.Index(rest, []byte(`","status":0}`))
		if endIdx < 0 {
			t.Fatalf("run %d: data not terminated by status suffix: %q", i, string(rest[max(0, len(rest)-40):]))
		}
		got := unquoteJSONFast(string(rest[:endIdx]))
		if got != payload {
			t.Fatalf("run %d: payload mismatch, len got=%d want=%d", i, len(got), len(payload))
		}
		gotHash := sha256.Sum256([]byte(got))
		if hex.EncodeToString(gotHash[:]) != hex.EncodeToString(wantHash[:]) {
			t.Fatalf("run %d: sha256 mismatch", i)
		}
		t.Logf("run %d: %d bytes roundtrip OK, sha256=%s", i, len(body), hex.EncodeToString(gotHash[:])[:16])
	}
}

// unquoteJSONFast 对 payload 中仅可能出现的转义做反转义（测试用）。
func unquoteJSONFast(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n")
	return r.Replace(s)
}

// TestE2E_WSCompression_NonCompressClient 不带压缩扩展的客户端降级兼容：
// 握手正常升级（101）且注册可见。
func TestE2E_WSCompression_NonCompressClient(t *testing.T) {
	srv, addr := startTestServer(t)
	_ = srv

	// 原始帧客户端握手请求不带 Sec-WebSocket-Extensions（不支持压缩的端）
	raw := dialWS(t, addr, "ws-nocomp", "nocomp-1")
	defer raw.close()

	// 注册可见即证明降级通道工作正常
	resp, err := http.Get("http://" + addr + "/business/clientQueue?group=ws-nocomp")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "nocomp-1") {
		t.Fatalf("non-compress client not registered: %s", b)
	}
}
