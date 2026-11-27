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

// TestWSProgressFrameExtendsInvokeTimeout 真实 WS 链路验证长任务进度帧：
// invoke_timeout=600ms，provider 每 250ms 发一帧进度、总共 750ms 后回包。
// 若服务端把进度帧当普通响应或忽略，请求会在 600ms 超时；修复后应成功。
func TestWSProgressFrameExtendsInvokeTimeout(t *testing.T) {
	log := discardLogger()
	mgr := group.NewManager(log)
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, false, ""))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + srv.URL[len("http"):] + "/register?group=slowws&clientId=p1"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(mgr.EnabledClientIDs("slowws")) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(mgr.EnabledClientIDs("slowws")) == 0 {
		t.Fatal("provider 未注册")
	}

	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var req map[string]any
			if json.Unmarshal(data, &req) != nil {
				continue
			}
			seqf, ok := req["__sekiro_seq__"].(float64)
			if !ok {
				continue
			}
			seq := int64(seqf)
			for i := 0; i < 3; i++ {
				time.Sleep(250 * time.Millisecond)
				frame := fmt.Sprintf(`{"__sekiro_progress__":%d,"__sekiro_seq__":%d}`, seq, seq)
				if conn.Write(context.Background(), websocket.MessageText, []byte(frame)) != nil {
					return
				}
			}
			done := fmt.Sprintf(`{"__sekiro_seq__":%d,"status":0,"data":"ok"}`, seq)
			if conn.Write(context.Background(), websocket.MessageText, []byte(done)) != nil {
				return
			}
		}
	}()

	resp, err := srv.Client().Get(srv.URL +
		"/business-demo/invoke?group=slowws&action=slow&invoke_timeout=600")
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"status":0`) {
		t.Fatalf("进度帧未延长等待窗口，响应 = %s", body)
	}
}
