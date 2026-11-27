package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
)

// TestOldWSClientLongInvokeWithAliasTimeout 验证「两边都不改」的兼容路径：
// provider 使用旧客户端行为（不声明 hb=1、不发进度帧），调用方仍写历史拼写
// invoke_timeOut=1200；服务端靠别名识别超时，900ms 后才回包也应成功。
func TestOldWSClientLongInvokeWithAliasTimeout(t *testing.T) {
	log := discardLogger()
	mgr := group.NewManager(log)
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, false, ""))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 刻意不带 hb=1：模拟官方 sekiroWeb.js 的注册方式。
	wsURL := "ws" + srv.URL[len("http"):] + "/register?group=oldclient&clientId=p1"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(mgr.EnabledClientIDs("oldclient")) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(mgr.EnabledClientIDs("oldclient")) == 0 {
		t.Fatal("provider 未注册")
	}

	go func() {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var req map[string]any
		if json.Unmarshal(data, &req) != nil {
			return
		}
		seqf, ok := req["__sekiro_seq__"].(float64)
		if !ok {
			return
		}
		time.Sleep(900 * time.Millisecond)
		done := fmt.Sprintf(`{"__sekiro_seq__":%d,"status":0,"data":"old-ok"}`, int64(seqf))
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(done))
	}()

	resp, err := srv.Client().Get(srv.URL +
		"/business-demo/invoke?group=oldclient&action=slow&invoke_timeOut=1200")
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"status":0`) ||
		!strings.Contains(string(body), "old-ok") {
		t.Fatalf("旧客户端 + 历史拼写应成功，响应 = %s", body)
	}
}
