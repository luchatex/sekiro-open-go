package httpapi

// 调用参数日志分级验证：
//   - Info（默认）：只记 group/action，业务参数（含 secret）不落盘
//   - Debug（--debug）：完整参数入日志，且超长截断

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/protocol"
)

// captureHandler 收集日志文本（按级别过滤，模拟 slog.HandlerOptions.Level）。
type captureHandler struct {
	mu    sync.Mutex
	level slog.Level
	lines []string
}

func (h *captureHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.level
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(a.Value.String())
		return true
	})
	h.mu.Lock()
	h.lines = append(h.lines, b.String())
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
func (h *captureHandler) joined() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.lines, "\n")
}

func invokeRequest(t *testing.T, h *captureHandler, level slog.Level, query string) string {
	t.Helper()
	h.level = level
	h.mu.Lock()
	h.lines = nil
	h.mu.Unlock()

	log := slog.New(h)
	mgr := group.NewManager(log)
	hdl := Handler(mgr, compress.NewManager(), log, false)
	req := httptest.NewRequest("GET", "/business/invoke?"+query, nil)
	rec := httptest.NewRecorder()
	hdl.ServeHTTP(rec, req)
	return h.joined()
}

func TestInvokeLogInfoOmitsBusinessParams(t *testing.T) {
	h := &captureHandler{}
	out := invokeRequest(t, h, slog.LevelInfo, "group=g1&action=a1&secret=SUPER_SECRET")
	if !strings.Contains(out, "sekiro invoke request") {
		t.Fatalf("invoke log missing: %s", out)
	}
	if strings.Contains(out, "SUPER_SECRET") {
		t.Fatalf("business params must not be logged at Info level: %s", out)
	}
	if !strings.Contains(out, "group=g1") || !strings.Contains(out, "action=a1") {
		t.Fatalf("group/action should be logged: %s", out)
	}
}

func TestInvokeLogDebugIncludesParamsAndTruncates(t *testing.T) {
	h := &captureHandler{}
	out := invokeRequest(t, h, slog.LevelDebug, "group=g1&action=a1&secret=SUPER_SECRET")
	if !strings.Contains(out, "SUPER_SECRET") {
		t.Fatalf("debug level should log full params: %s", out)
	}

	// 超长参数：params 字段应被截断（含省略标记）
	long := "group=g1&action=a1&big=" + strings.Repeat("x", 5000)
	out = invokeRequest(t, h, slog.LevelDebug, long)
	if !strings.Contains(out, "omitted") {
		t.Fatalf("long params should be truncated: len=%d", len(out))
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "params=") && len(line) > maxLogParamsLen+64 {
			t.Fatalf("params field not truncated: %d bytes", len(line))
		}
	}
}

// 分段流/正常路径之外：参数缺失时日志不应报错（nil 安全）
func TestInvokeLogWithoutParams(t *testing.T) {
	h := &captureHandler{level: slog.LevelInfo}
	log := slog.New(h)
	mgr := group.NewManager(log)
	hdl := Handler(mgr, compress.NewManager(), log, false)
	req := httptest.NewRequest("GET", "/business/invoke", nil)
	hdl.ServeHTTP(httptest.NewRecorder(), req)
	if out := h.joined(); !strings.Contains(out, "sekiro invoke request") {
		t.Fatalf("invoke log missing for empty params: %s", out)
	}
	_ = protocol.ParamGroup
}
