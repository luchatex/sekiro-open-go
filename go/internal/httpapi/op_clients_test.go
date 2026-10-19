package httpapi

// /op/api/clients 与 /op/api/clients/kick 的端到端验证（真实 WS 连接）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
)

type clientNode struct {
	Group       string `json:"group"`
	ClientID    string `json:"clientId"`
	IP          string `json:"ip"`
	Transport   string `json:"transport"`
	Compression string `json:"compression"`
	ConnectedAt string `json:"connectedAt"`
	LastActive  string `json:"lastActive"`
	Online      bool   `json:"online"`
}

func fetchClients(t *testing.T, srv *httptest.Server) []clientNode {
	t.Helper()
	resp, err := http.Get(srv.URL + "/op/api/clients")
	if err != nil {
		t.Fatalf("get clients: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Clients []clientNode `json:"clients"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode clients: %v", err)
	}
	return out.Clients
}

func postKick(t *testing.T, srv *httptest.Server, group, id string) bool {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"group": group, "clientId": id})
	resp, err := http.Post(srv.URL+"/op/api/clients/kick", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("kick: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kick status = %d", resp.StatusCode)
	}
	var out struct {
		Kicked bool `json:"kicked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode kick: %v", err)
	}
	return out.Kicked
}

// TestOpClientsListAndKick 端到端：WS 注册 → 列表可见（IP/通道/时间）→
// kick → 连接断开、注册表移除。
func TestOpClientsListAndKick(t *testing.T) {
	withIdleParams(t, 1*time.Second, 5*time.Second) // 保活别干扰踢下线验证

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := group.NewManager(log)
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, true, ""))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)

	_ = dialWSProvider(t, srv, "nodes", "dev-ws-1")

	// 列表可见（轮询等注册经 actor 生效）
	var node *clientNode
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for i := range fetchClients(t, srv) {
			c := fetchClients(t, srv)[i]
			if c.Group == "nodes" && c.ClientID == "dev-ws-1" {
				node = &c
				break
			}
		}
		if node != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if node == nil {
		t.Fatal("registered ws provider missing from /op/api/clients")
	}
	if node.IP == "" || node.Transport != "ws" || !node.Online {
		t.Fatalf("node info incomplete: %+v", node)
	}
	if _, err := time.Parse(time.RFC3339, node.ConnectedAt); err != nil {
		t.Fatalf("connectedAt not RFC3339: %q", node.ConnectedAt)
	}
	t.Logf("CLIENTS: node visible: ip=%s transport=%s online=%v", node.IP, node.Transport, node.Online)

	// kick → kicked=true → 连接断开、注册表移除
	if !postKick(t, srv, "nodes", "dev-ws-1") {
		t.Fatal("kick returned kicked=false")
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		online := false
		for _, c := range fetchClients(t, srv) {
			if c.Group == "nodes" && c.ClientID == "dev-ws-1" && c.Online {
				online = true
			}
		}
		if !online && len(mgr.EnabledClientIDs("nodes")) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client not reclaimed after kick: clients=%v ids=%v",
				fetchClients(t, srv), mgr.EnabledClientIDs("nodes"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Log("KICK: 连接断开并彻底注销")

	// 再踢不在线的：kicked=false
	if postKick(t, srv, "nodes", "dev-ws-1") {
		t.Fatal("second kick should return kicked=false")
	}
}

// TestWSCompressionNegotiation 压缩协商可观测：
// 客户端声明 permessage-deflate → "deflate"；未声明（默认 DialOptions）→ "off"。
func TestWSCompressionNegotiation(t *testing.T) {
	withIdleParams(t, 1*time.Second, 5*time.Second)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := group.NewManager(log)
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, true, ""))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)

	// 声明 permessage-deflate 的拨号（模拟浏览器/js-web 默认行为）
	dialWithDeflate := func(g, id string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		wsURL := "ws" + srv.URL[len("http"):] + "/register?group=" + g + "&clientId=" + id
		conn, _, err := websocket.Dial(ctx, wsURL,
			&websocket.DialOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			t.Fatalf("ws dial(deflate): %v", err)
		}
		go func() {
			for {
				if _, _, err := conn.Read(context.Background()); err != nil {
					return
				}
			}
		}()
		t.Cleanup(func() { conn.CloseNow() })
	}
	dialWithDeflate("comp", "dev-deflate")
	_ = dialWSProvider(t, srv, "comp", "dev-plain") // 默认拨号不声明扩展

	comp := map[string]string{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		comp = map[string]string{}
		for _, c := range fetchClients(t, srv) {
			if c.Group == "comp" {
				comp[c.ClientID] = c.Compression
			}
		}
		if len(comp) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if comp["dev-deflate"] != "deflate" {
		t.Fatalf("deflate client compression = %q, want deflate", comp["dev-deflate"])
	}
	if comp["dev-plain"] != "off" {
		t.Fatalf("plain client compression = %q, want off", comp["dev-plain"])
	}
	t.Logf("COMPRESSION: deflate=%s plain=%s", comp["dev-deflate"], comp["dev-plain"])
}

// TestOpKickValidation 参数校验与 op 关闭路径。
func TestOpKickValidation(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := group.NewManager(log)
	h := Handler(mgr, compress.NewManager(), log, true, "")

	// GET 不允许
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/op/api/clients/kick", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET kick = %d, want 405", rec.Code)
	}

	// 缺参数
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/op/api/clients/kick",
		bytes.NewReader([]byte(`{}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body kick = %d, want 400", rec.Code)
	}

	// opEnabled=false 时 404
	h2 := Handler(mgr, compress.NewManager(), log, false, "")
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/op/api/clients", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("clients with op disabled = %d, want 404", rec.Code)
	}
}
