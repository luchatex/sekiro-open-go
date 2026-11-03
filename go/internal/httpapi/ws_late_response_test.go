package httpapi

// 迟到响应的 WS 端到端回归。
//
// 现场：某 group 每分钟固定 4 条 getDetailData 超时，30 分钟不断。此前实现里
// 「服务端已判超时、provider 之后才回包」的包会走完 JSON 解析 → 移除 seq →
// 重新序列化整包，最后因 pending 查空被丢弃：既白烧 CPU（详情页可达 MB 级），
// 又完全没有留下任何计数——迟到响应是判断「上游慢」还是「上游挂死」的唯一依据。
//
// 本用例走真实 WS 通道：provider 收到 invoke 后故意不回，等超时响应落地，
// 再用同一序号补一个迟到包，验证 wsHandleMessage 的解压前拦截与计数。

import (
	"context"
	"encoding/json"
	"fmt"
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
	"sekiro-go/internal/metrics"
)

func TestLateWSResponseCountedAfterTimeout(t *testing.T) {
	metrics.Reset()
	logs := &captureHandler{level: slog.LevelDebug} // 断言「解压前早丢」确实走到了
	log := slog.New(logs)
	mgr := group.NewManager(log)
	t.Cleanup(mgr.Close)
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, true, ""))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + srv.URL[len("http"):] + "/register?group=late-e2e&clientId=c1"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	seqCh := make(chan int64, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if v, ok := m["__sekiro_seq__"].(float64); ok {
				select {
				case seqCh <- int64(v):
				default:
				}
			}
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for len(mgr.EnabledClientIDs("late-e2e")) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(mgr.EnabledClientIDs("late-e2e")) == 0 {
		t.Fatal("provider 未注册进 group")
	}

	// invoke_timeout 取 600ms（<500ms 会被按默认 15s 处理，用例无法接受）
	res, err := http.Get(srv.URL + "/business/invoke?group=late-e2e&action=getDetailData&invoke_timeout=600")
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("invoke status = %d, body = %s", res.StatusCode, body)
	}
	if got := string(body); !strings.Contains(got, "timeout") {
		t.Fatalf("调用方应收到 timeout 响应，实际 = %s", got)
	}

	var seq int64
	select {
	case seq = <-seqCh:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 未收到 invoke")
	}

	// provider 迟到回包（服务端已按超时回包）
	payload := fmt.Sprintf(`{"__sekiro_seq__":%d,"status":0,"msg":"ok"}`, seq)
	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("write late response: %v", err)
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.Default().Errors().LateTotal > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	e := metrics.Default().Errors()
	if e.LateTotal != 1 {
		t.Fatalf("lateTotal = %d, want 1（迟到回包未计入）", e.LateTotal)
	}
	if len(e.Late) != 1 || e.Late[0].Group != "late-e2e" ||
		e.Late[0].Action != "getDetailData" {
		t.Fatalf("late 聚合 = %+v", e.Late)
	}
	if e.Late[0].MaxDelayMs < 0 {
		t.Fatalf("maxDelayMs = %d", e.Late[0].MaxDelayMs)
	}
	// 早丢路径：迟到包不得重建 packet、不得重新序列化整包
	if out := logs.joined(); !strings.Contains(out, "dropped before marshal") {
		t.Fatalf("未走「解压/重组之前丢弃」的早丢路径，日志 = %s", out)
	}
}
