package httpapi

import (
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/metrics"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

func newOpTestHandler() http.Handler {
	metrics.Reset()
	return Handler(group.NewManager(slog.Default()), compress.NewManager(), slog.Default(), true, "")
}

func TestOpPage(t *testing.T) {
	h := newOpTestHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/op", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/op status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "监控报表") || !strings.Contains(body, "/op/api/stats") {
		t.Error("page content invalid")
	}
}

func TestOpStatsEmpty(t *testing.T) {
	h := newOpTestHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/op/api/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var snap metrics.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Groups == nil || len(snap.Groups) != 0 {
		t.Errorf("groups = %+v, want empty", snap.Groups)
	}
	if len(snap.Hourly.Buckets) != 24 || len(snap.Daily.Buckets) != 7 {
		t.Errorf("series lens = %d/%d, want 24/7", len(snap.Hourly.Buckets), len(snap.Daily.Buckets))
	}
}

func TestOpDisabled(t *testing.T) {
	metrics.Reset()
	h := Handler(group.NewManager(slog.Default()), compress.NewManager(), slog.Default(), false, "")
	for _, p := range []string{"/op", "/op/api/stats"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 (op disabled)", p, rec.Code)
		}
	}
}

// ---- 端到端埋点验证：fake provider 注册 → invoke → stats ----

// fastJSONEnvelope 构造 SekiroFastJson 信封 [int32 status][len][msg][len][data]。
func fastJSONEnvelope(status int32, msg, data string) []byte {
	out := make([]byte, 0, 12+len(msg)+len(data))
	var b4 [4]byte
	binary.BigEndian.PutUint32(b4[:], uint32(status))
	out = append(out, b4[:]...)
	binary.BigEndian.PutUint32(b4[:], uint32(len(msg)))
	out = append(out, b4[:]...)
	out = append(out, msg...)
	binary.BigEndian.PutUint32(b4[:], uint32(len(data)))
	out = append(out, b4[:]...)
	return append(out, data...)
}

// echoTransport 收到 invoke 后异步回 fastjson 响应（status 可控）。
type echoTransport struct {
	cli    *group.Client
	status int32 // 0=成功；非 0 模拟 provider 业务失败
	pad    int   // >0 时响应体填充指定字节，用于压缩出站统计测试
}

func (e *echoTransport) SendInvoke(_ *orderedjson.Map, seq int32) error {
	go func() {
		pkt := protocol.NewPacket(protocol.STypeInvokeResponse)
		pkt.Seq = seq
		pkt.AddHeader(protocol.PayloadContentType, protocol.ContentTypeSekiroFastJson)
		data := `{"echo":"ok"}`
		if e.pad > 0 {
			data = `{"echo":"ok","pad":"` + strings.Repeat("A", e.pad) + `"}`
		}
		if e.status != 0 {
			data = `{"err":"mock"}`
		}
		msg := ""
		if e.status != 0 {
			msg = "mock provider fail"
		}
		pkt.Data = fastJSONEnvelope(e.status, msg, data)
		e.cli.HandleResponse(pkt)
	}()
	return nil
}
func (e *echoTransport) Active() bool             { return true }
func (e *echoTransport) Close()                   {}
func (e *echoTransport) CloseAfter(time.Duration) {}

func registerEchoClient(t *testing.T, mgr *group.Manager, gName, cID string, status int32, pad ...int) *group.Client {
	t.Helper()
	g := mgr.GetOrCreate(gName)
	tr := &echoTransport{status: status}
	if len(pad) > 0 {
		tr.pad = pad[0]
	}
	cli := group.NewClient(tr, g, cID, slog.Default())
	tr.cli = cli
	g.Register(cli)
	// Register 是 actor 异步投递，等注册生效
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(mgr.EnabledClientIDs(gName)) == 1 {
			return cli
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("echo client not registered in time")
	return nil
}

type statsJSON struct {
	Groups []struct {
		Group   string `json:"group"`
		Clients int    `json:"clients"`
		Metrics struct {
			Total   int64   `json:"total"`
			Success int64   `json:"success"`
			Fail    int64   `json:"fail"`
			SysErr  int64   `json:"sysErr"`
			Rate    float64 `json:"successRate"`
			OutKB   float64 `json:"bytesOutKB"`
			WireKB  float64 `json:"bytesOutWireKB"`
		} `json:"metrics"`
		SysErrReasons map[string]int64 `json:"sysErrReasons"`
		Actions       []struct {
			Action string `json:"action"`
		} `json:"actions"`
	} `json:"groups"`
	SysErrReasons map[string]int64 `json:"sysErrReasons"`
}

func fetchStats(t *testing.T, h http.Handler) statsJSON {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/op/api/stats", nil))
	var s statsJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("stats unmarshal: %v", err)
	}
	return s
}

func findGroup(t *testing.T, s statsJSON, name string) int {
	t.Helper()
	for i := range s.Groups {
		if s.Groups[i].Group == name {
			return i
		}
	}
	t.Fatalf("group %q not in stats", name)
	return -1
}

func TestOpStatsEndToEnd(t *testing.T) {
	metrics.Reset()
	mgr := group.NewManager(slog.Default())
	h := Handler(mgr, compress.NewManager(), slog.Default(), true, "")
	registerEchoClient(t, mgr, "demo-group", "c1", 0)

	// 成功调用
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/business/invoke?group=demo-group&action=v1&param=x", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":0`) {
		t.Fatalf("invoke resp: %d %s", rec.Code, rec.Body.String())
	}

	s := fetchStats(t, h)
	i := findGroup(t, s, "demo-group")
	g := s.Groups[i]
	if g.Clients != 1 {
		t.Errorf("clients = %d, want 1", g.Clients)
	}
	if g.Metrics.Total != 1 || g.Metrics.Success != 1 {
		t.Errorf("total/success = %d/%d, want 1/1", g.Metrics.Total, g.Metrics.Success)
	}
	if g.Metrics.OutKB <= 0 {
		t.Errorf("bytesOutKB = %v, want > 0", g.Metrics.OutKB)
	}
	if len(g.Actions) != 1 || g.Actions[0].Action != "v1" {
		t.Errorf("actions = %+v, want [v1]", g.Actions)
	}
}

func TestOpStatsFailAndSysErr(t *testing.T) {
	metrics.Reset()
	mgr := group.NewManager(slog.Default())
	h := Handler(mgr, compress.NewManager(), slog.Default(), true, "")
	registerEchoClient(t, mgr, "g-fail", "c1", -1) // provider 返回失败

	// provider 业务失败（status=-1 → Fail）
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/business/invoke?group=g-fail&action=a", nil))

	// 系统错误：空池 group（首次空池立即失败，不走退避）
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/business/invoke?group=g-empty&action=a", nil))

	s := fetchStats(t, h)
	i := findGroup(t, s, "g-fail")
	if s.Groups[i].Metrics.Fail != 1 || s.Groups[i].Metrics.Success != 0 {
		t.Errorf("g-fail fail/success = %d/%d, want 1/0", s.Groups[i].Metrics.Fail, s.Groups[i].Metrics.Success)
	}
	j := findGroup(t, s, "g-empty")
	if s.Groups[j].Metrics.SysErr != 1 {
		t.Errorf("g-empty sysErr = %d, want 1", s.Groups[j].Metrics.SysErr)
	}
}

// TestOpStatsWireBytesCompressed HTTP 消费方压缩生效时，快照的实际出站
// 字节（bytesOutWireKB）应大于 0 且显著小于逻辑值（bytesOutKB）。
func TestOpStatsWireBytesCompressed(t *testing.T) {
	metrics.Reset()
	mgr := group.NewManager(slog.Default())
	h := Handler(mgr, compress.NewManager(), slog.Default(), true, "")
	registerEchoClient(t, mgr, "g-wire", "c1", 0, 200000)

	big := strings.Repeat("A", 200000) // 高可压内容
	req := httptest.NewRequest(http.MethodGet,
		"/business/invoke?group=g-wire&action=big&param="+big, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("invoke: %d", rec.Code)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", ce)
	}
	if rec.Body.Len() >= 200000 {
		t.Fatalf("response not compressed: %d bytes", rec.Body.Len())
	}

	// 响应关闭与 finishMetric 不是同一个 happens-before 边：HTTP handler
	// 先被 Closed 放行，指标随后由 provider 响应协程写入。轮询等待终态，
	// 避免慢 CI（尤其 -race）在指标落库前读到 0。
	var outKB, wireKB float64
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := fetchStats(t, h)
		g := s.Groups[findGroup(t, s, "g-wire")]
		outKB, wireKB = g.Metrics.OutKB, g.Metrics.WireKB
		if outKB > 0 && wireKB > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics not finalized: bytesOutKB=%v bytesOutWireKB=%v", outKB, wireKB)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if wireKB >= outKB {
		t.Fatalf("wire %v should be < logical %v", wireKB, outKB)
	}
}

// TestOpStatsWireBytesUncompressed 未协商压缩（无 Accept-Encoding）时，
// 透传路径同样产出实际出站计数。
func TestOpStatsWireBytesUncompressed(t *testing.T) {
	metrics.Reset()
	mgr := group.NewManager(slog.Default())
	h := Handler(mgr, compress.NewManager(), slog.Default(), true, "")
	registerEchoClient(t, mgr, "g-plain", "c1", 0)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/business/invoke?group=g-plain&action=a&param=hello", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("invoke: %d", rec.Code)
	}

	s := fetchStats(t, h)
	g := s.Groups[findGroup(t, s, "g-plain")]
	// 未压缩时 wire = HTTP 响应体字节（含 clientId 注入等信封开销），
	// 与逻辑值（纯 payload）同量级但不必相等；断言已实际统计（> 0）
	if g.Metrics.WireKB <= 0 {
		t.Fatalf("bytesOutWireKB = %v, want > 0", g.Metrics.WireKB)
	}
}
