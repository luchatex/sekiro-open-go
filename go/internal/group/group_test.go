package group

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// ---- 测试用 Transport ----

type fakeTransport struct {
	mu         sync.Mutex
	active     bool
	sendCalls  []int32
	closeNow   bool
	closeAfter []time.Duration
	closedCh   chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{active: true, closedCh: make(chan struct{})}
}

func (f *fakeTransport) SendInvoke(_ *orderedjson.Map, seq int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls = append(f.sendCalls, seq)
	return nil
}

func (f *fakeTransport) Active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active && !f.closeNow
}

func (f *fakeTransport) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeNow = true
	select {
	case <-f.closedCh:
	default:
		close(f.closedCh)
	}
}

func (f *fakeTransport) CloseAfter(d time.Duration) {
	f.mu.Lock()
	f.closeAfter = append(f.closeAfter, d)
	f.mu.Unlock()
	time.AfterFunc(d, f.Close)
}

// ---- 测试用 Sink ----

type fakeSink struct {
	mu      sync.Mutex
	common  []commonCall
	packets []*protocol.Packet
	closed  chan struct{}
	once    sync.Once
}

type commonCall struct {
	status  int
	message string
	seq     int32
}

func newFakeSink() *fakeSink {
	return &fakeSink{closed: make(chan struct{})}
}

func (s *fakeSink) RespondCommon(status int, message, _ string, seq int32) {
	s.mu.Lock()
	s.common = append(s.common, commonCall{status, message, seq})
	s.mu.Unlock()
	s.once.Do(func() { close(s.closed) })
}

func (s *fakeSink) RespondPacket(p *protocol.Packet) {
	s.mu.Lock()
	s.packets = append(s.packets, p)
	s.mu.Unlock()
	s.once.Do(func() { close(s.closed) })
}

func (s *fakeSink) Closed() <-chan struct{} { return s.closed }

// ---- 用例 ----

func newTestManager() *Manager {
	return NewManager(slog.New(slog.NewTextHandler(&testWriter{}, nil)))
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestAllocateModes(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	// 空池 → ErrPoolEmpty（"pool queue empty"）
	if _, err := m.Allocate("g1", "", ""); err != ErrPoolEmpty {
		t.Fatalf("empty pool: %v", err)
	}

	tr := newFakeTransport()
	g := m.GetOrCreate("g1")
	c := NewClient(tr, g, "client-a", slog.Default())
	g.register(c)
	waitActor()

	// 轮询
	got, err := m.Allocate("g1", "", "")
	if err != nil || got.ID != "client-a" {
		t.Fatalf("rotate allocate: %v %v", got, err)
	}
	// 一致性哈希（任意 key 都应命中唯一节点）
	got, err = m.Allocate("g1", "", "some-key")
	if err != nil || got.ID != "client-a" {
		t.Fatalf("consistent allocate: %v %v", got, err)
	}
	// bind_client 定向
	got, err = m.Allocate("g1", "client-a", "")
	if err != nil || got.ID != "client-a" {
		t.Fatalf("bind allocate: %v %v", got, err)
	}
	// bind_client 不存在
	if _, err := m.Allocate("g1", "ghost", ""); err == nil {
		t.Fatal("expected miss for ghost client")
	}
}

func TestDuplicateRegisterKeepsNewAlive(t *testing.T) {
	// Java 注释 BugFix：重复注册时旧连接 30s 延迟关闭，新连接立即生效
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("dup")

	oldTr := newFakeTransport()
	oldC := NewClient(oldTr, g, "dev1", slog.Default())
	g.register(oldC)
	waitActor()

	newTr := newFakeTransport()
	newC := NewClient(newTr, g, "dev1", slog.Default())
	g.register(newC)
	waitActor()

	// ring 应指向新 client
	got, err := m.Allocate("dup", "", "k")
	if err != nil || got != newC {
		t.Fatalf("ring should map to new client: %v %v", got, err)
	}
	// 旧连接应被延迟 30s 关闭（不是立即关闭）
	oldTr.mu.Lock()
	n := len(oldTr.closeAfter)
	oldTr.mu.Unlock()
	if n != 1 {
		t.Fatalf("old transport should be CloseAfter once, got %d", n)
	}
	if oldC.Active() && !oldC.IsDown() {
		t.Fatal("old client should be marked offline")
	}
}

func TestUnregisterIdentityCheck(t *testing.T) {
	// Java 注释 BugFix：只有同一 channel 才从环里删除
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("ident")

	tr1 := newFakeTransport()
	c1 := NewClient(tr1, g, "dev1", slog.Default())
	g.register(c1)
	waitActor()

	tr2 := newFakeTransport()
	c2 := NewClient(tr2, g, "dev1", slog.Default())
	g.register(c2)
	waitActor()

	// 旧连接断开 → 注销，但不能删掉新连接的环表项
	g.unregister(c1)
	waitActor()

	got, err := m.Allocate("ident", "", "k")
	if err != nil || got != c2 {
		t.Fatalf("new client must survive old unregister: %v %v", got, err)
	}
}

func TestReconnectChurnDoesNotRetainClients(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("churn")

	// 真实断线路径：register 后 unregister。即使没有调用触发轮询清理，
	// rotate 也不能永久引用已经断开的 Client 及其 Transport/pending。
	for i := 0; i < 100; i++ {
		c := NewClient(newFakeTransport(), g, "dev1", slog.Default())
		g.register(c)
		g.unregister(c)
	}
	syncActor(g)
	if got := len(g.rotate); got != 0 {
		t.Fatalf("断线重连后 rotate 保留 %d 个历史 Client，want 0", got)
	}
}

func TestUnknownGroupQueriesDoNotCreateGroup(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	if _, err := m.Allocate("ghost", "", ""); err != ErrPoolEmpty {
		t.Fatalf("未知 group 分配错误 = %v, want %v", err, ErrPoolEmpty)
	}
	if got := m.EnabledClientIDs("ghost"); len(got) != 0 {
		t.Fatalf("未知 group 客户端列表 = %v, want empty", got)
	}
	if m.KickClient("ghost", "dev1") {
		t.Fatal("未知 group 不应踢到设备")
	}
	if got := m.Names(); len(got) != 0 {
		t.Fatalf("只读/失败请求创建了 group: %v", got)
	}
}

func TestForwardInvokeAfterDisconnectFailsWithoutSend(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("disconnected")
	tr := newFakeTransport()
	c := NewClient(tr, g, "dev1", slog.Default())
	g.register(c)
	waitActor()
	c.Disconnect()
	waitActor()

	sink := newFakeSink()
	rec := NewRecord(orderedjson.New(), sink, -1, time.Second)
	c.ForwardInvoke(rec)

	tr.mu.Lock()
	sends := len(tr.sendCalls)
	tr.mu.Unlock()
	if sends != 0 {
		t.Fatalf("断线 Client 仍收到 %d 次下发，want 0", sends)
	}
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if pending != 0 {
		t.Fatalf("断线后下发残留 %d 条 pending", pending)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.common) != 1 || sink.common[0].message != "sekiro upstream lose connection" {
		t.Fatalf("断线后下发的失败响应 = %+v", sink.common)
	}
}

func TestEmptyPoolBackoff(t *testing.T) {
	// 退避策略：≤5 次立即失败；第 6 次起延时 (n-4)s
	m := newTestManager()
	defer m.Close()
	m.GetOrCreate("backoff") // 退避只作用于已存在的空 group；未知 group 快速失败

	start := time.Now()
	for i := 1; i <= 6; i++ {
		t0 := time.Now()
		_, err := m.Allocate("backoff", "", "")
		if err != ErrPoolEmpty {
			t.Fatalf("iter %d: %v", i, err)
		}
		el := time.Since(t0)
		if i <= 5 && el > 200*time.Millisecond {
			t.Fatalf("iter %d should fail fast, took %v", i, el)
		}
		if i == 6 && el < 1500*time.Millisecond {
			t.Fatalf("iter 6 should delay ~2s, took %v (start offset %v)", el, time.Since(start))
		}
	}
}

func TestAllocateContextCancelDuringBackoff(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	m.GetOrCreate("cancel-backoff")
	for i := 0; i < 5; i++ {
		if _, err := m.Allocate("cancel-backoff", "", ""); err != ErrPoolEmpty {
			t.Fatalf("前置空池调用 %d: %v", i+1, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := m.AllocateContext(ctx, "cancel-backoff", "", "")
		result <- err
	}()
	time.Sleep(50 * time.Millisecond) // 进入第 6 次退避等待
	cancel()

	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("取消后退避返回 %v, want context.Canceled", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("调用方取消后仍卡在空池退避中")
	}
}

func TestInvokeFlowTimeout(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("flow")
	tr := newFakeTransport()
	c := NewClient(tr, g, "dev1", slog.Default())
	g.register(c)
	waitActor()

	sink := newFakeSink()
	params, _ := orderedjson.Parse([]byte(`{"group":"flow","action":"a"}`))
	rec := NewRecord(params, sink, -1, 300*time.Millisecond) // <500ms → 提升到默认 15s
	rec2 := NewRecord(params, sink, -1, 0)
	_ = rec2
	rec.attached(c, 7)

	select {
	case <-sink.Closed():
		sink.mu.Lock()
		cc := sink.common[0]
		sink.mu.Unlock()
		if cc.message != "timeout" || cc.status != -1 || cc.seq != 7 {
			t.Fatalf("timeout response: %+v", cc)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timeout record did not fire (should use default 15s)")
	}
}

func TestInvokeFlowResponse(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("flow2")
	tr := newFakeTransport()
	c := NewClient(tr, g, "dev1", slog.Default())
	g.register(c)
	waitActor()

	sink := newFakeSink()
	params, _ := orderedjson.Parse([]byte(`{"group":"flow2","action":"a"}`))
	rec := NewRecord(params, sink, -1, 5*time.Second)
	c.ForwardInvoke(rec)

	tr.mu.Lock()
	seqs := tr.sendCalls
	tr.mu.Unlock()
	if len(seqs) != 1 {
		t.Fatalf("expected 1 send, got %d", len(seqs))
	}

	// 模拟 provider 响应（v2 类型 C→S 降级 + clientId 注入）
	p := protocol.NewPacket(protocol.CTypeInvokeResponse)
	p.Seq = seqs[0]
	p.Data = []byte(`{"status":0,"data":"ok"}`)
	c.HandleResponse(p)

	select {
	case <-sink.Closed():
		sink.mu.Lock()
		got := sink.packets[0]
		sink.mu.Unlock()
		if got.Type != protocol.STypeInvokeResponse {
			t.Fatalf("type should downgrade to S_TYPE_INVOKE_RESPONSE, got %v", got.Type)
		}
		if got.GetHeader(protocol.HeaderClientID) != "dev1" {
			t.Fatal("clientId header missing")
		}
		if string(got.Data) != `{"status":0,"data":"ok"}` {
			t.Fatalf("data: %s", got.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no response delivered")
	}
}

// syncRespTransport 在 SendInvoke 返回前同步回包（provider 毫秒级响应的
// 极端形态），确定性复现 deliver 先于 attached 的时序窗口。
type syncRespTransport struct {
	*fakeTransport
	client *Client
}

func (f *syncRespTransport) SendInvoke(_ *orderedjson.Map, seq int32) error {
	p := protocol.NewPacket(protocol.CTypeInvokeResponse)
	p.Seq = seq
	p.Data = []byte(`{"status":0,"data":"ok"}`)
	f.client.HandleResponse(p)
	return nil
}

func TestForwardInvokeSyncResponseKeepsClientID(t *testing.T) {
	// 回归：SendInvoke 内同步回包时，deliver 先于 attached 执行会导致
	// SEKIRO_CLIENT_ID 头为空（e2e -cover 下偶发 "clientId header
	// missing" 的根因）。修复后 attached 先于 SendInvoke，头必存在。
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("sync-resp")
	tr := &syncRespTransport{fakeTransport: newFakeTransport()}
	c := NewClient(tr, g, "dev1", slog.Default())
	tr.client = c
	g.register(c)
	waitActor()

	sink := newFakeSink()
	params, _ := orderedjson.Parse([]byte(`{"group":"sync-resp","action":"a"}`))
	rec := NewRecord(params, sink, -1, 5*time.Second)
	c.ForwardInvoke(rec)

	select {
	case <-sink.Closed():
		sink.mu.Lock()
		got := sink.packets[0]
		sink.mu.Unlock()
		if got.GetHeader(protocol.HeaderClientID) != "dev1" {
			t.Fatalf("clientId header lost on sync response: %q",
				got.GetHeader(protocol.HeaderClientID))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no response delivered")
	}
}

func TestDisconnectFailsPending(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("lost")
	tr := newFakeTransport()
	c := NewClient(tr, g, "dev1", slog.Default())
	g.register(c)
	waitActor()

	sink := newFakeSink()
	params, _ := orderedjson.Parse([]byte(`{}`))
	rec := NewRecord(params, sink, -1, 5*time.Second)
	c.ForwardInvoke(rec)

	tr.Close()
	c.Disconnect()
	waitActor()

	select {
	case <-sink.Closed():
		sink.mu.Lock()
		cc := sink.common[0]
		sink.mu.Unlock()
		if cc.message != "sekiro upstream lose connection" {
			t.Fatalf("message: %q", cc.message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending not failed on disconnect")
	}
}

func TestConsistentDistribution(t *testing.T) {
	// 多节点下一致性哈希：同一 key 稳定命中
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("dist")
	for i := 0; i < 5; i++ {
		tr := newFakeTransport()
		c := NewClient(tr, g, "dev-"+string(rune('a'+i)), slog.Default())
		g.register(c)
	}
	waitActor()

	first, err := m.Allocate("dist", "", "stable-key")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got, _ := m.Allocate("dist", "", "stable-key")
		if got != first {
			t.Fatalf("consistent hash unstable: %s vs %s", got.ID, first.ID)
		}
	}
}

// syncActor 用 actor 队列屏障建立 happens-before；测试不得用 Sleep
// 后直接读取 actor 私有状态（-race 会判数据竞争）。
func syncActor(g *Group) {
	done := make(chan struct{}, 1)
	g.post(func() { done <- struct{}{} })
	<-done
}

func waitActor() { time.Sleep(50 * time.Millisecond) }
