package group

// 监控节点信息采集与踢下线回归。

import (
	"log/slog"
	"testing"
	"time"
)

// addrTransport 实现全部可选接口，验证信息聚合。
type addrTransport struct {
	fakeTransport
	addr string
	kind string
	last time.Time
	comp string
}

func (f *addrTransport) PeerAddr() string      { return f.addr }
func (f *addrTransport) TransportKind() string { return f.kind }
func (f *addrTransport) LastActive() time.Time { return f.last }
func (f *addrTransport) Compression() string   { return f.comp }

func registerAndWait(t *testing.T, m *Manager, gName, id string) *addrTransport {
	t.Helper()
	tr := &addrTransport{
		fakeTransport: fakeTransport{active: true, closedCh: make(chan struct{})},
		addr:          "10.0.0.1:12345",
		kind:          "ws",
		last:          time.Now(),
		comp:          "deflate",
	}
	g := m.GetOrCreate(gName)
	g.Register(NewClient(tr, g, id, slog.Default()))
	waitActor()
	return tr
}

// TestClientDetailsAggregation 节点信息聚合：字段齐全、online 正确。
func TestClientDetailsAggregation(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	registerAndWait(t, m, "g1", "dev-1")

	infos := m.ClientDetails()
	if len(infos) != 1 {
		t.Fatalf("details = %d entries, want 1", len(infos))
	}
	i := infos[0]
	if i.Group != "g1" || i.ClientID != "dev-1" {
		t.Fatalf("group/clientId = %s/%s, want g1/dev-1", i.Group, i.ClientID)
	}
	if i.IP != "10.0.0.1:12345" || i.Transport != "ws" {
		t.Fatalf("ip/transport = %s/%s", i.IP, i.Transport)
	}
	if i.Compression != "deflate" {
		t.Fatalf("compression = %q, want deflate", i.Compression)
	}
	if !i.Online {
		t.Fatal("online = false, want true")
	}
	if i.ConnectedAt.IsZero() {
		t.Fatal("connectedAt not set")
	}
	if i.LastActive == nil || i.LastActive.IsZero() {
		t.Fatal("lastActive not set")
	}
	t.Logf("DETAILS: %+v", i)
}

// TestClientDetailsNoCompressionProvider 未实现 CompressionProvider 的
// transport（如 native、测试 fake）Compression 字段为空。
func TestClientDetailsNoCompressionProvider(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	g := m.GetOrCreate("g-plain")
	g.Register(NewClient(&fakeTransport{active: true, closedCh: make(chan struct{})},
		g, "dev-plain", slog.Default()))
	waitActor()

	infos := m.ClientDetails()
	if len(infos) != 1 || infos[0].Compression != "" {
		t.Fatalf("compression should be empty without provider, got %+v", infos)
	}
}

// TestClientDetailsOffline 离线（fake 标记死亡）后 Online 翻转。
func TestClientDetailsOffline(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	tr := registerAndWait(t, m, "g2", "dev-2")
	tr.mu.Lock()
	tr.active = false
	tr.mu.Unlock()

	infos := m.ClientDetails()
	if len(infos) != 1 || infos[0].Online {
		t.Fatalf("offline client should report Online=false, got %+v", infos)
	}
}

// TestKickClient 踢下线：连接被关闭（fake 收到 Close），随后注册表移除。
func TestKickClient(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	tr := registerAndWait(t, m, "g3", "dev-3")

	if !m.KickClient("g3", "dev-3") {
		t.Fatal("kick returned false, want true")
	}

	// fake transport 应收到 Close
	select {
	case <-tr.closedCh:
	default:
		t.Fatal("transport not closed after kick")
	}

	// 连接关闭 → Disconnect → 注册表移除（异步，轮询等待）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.EnabledClientIDs("g3")) == 0 {
			t.Log("KICK: 连接已断开并从注册表移除")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("client still online after kick: %v", m.EnabledClientIDs("g3"))
}

// TestKickMissingClient 踢不存在的 client：安全返回 false。
func TestKickMissingClient(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	if m.KickClient("ghost-group", "ghost") {
		t.Fatal("kick on missing client returned true")
	}
}

// TestClientDetailsEmpty 无 client 时返回空列表（JSON 输出为 []）。
func TestClientDetailsEmpty(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	infos := m.ClientDetails()
	if infos == nil || len(infos) != 0 {
		t.Fatalf("details = %v, want empty non-nil", infos)
	}
}
