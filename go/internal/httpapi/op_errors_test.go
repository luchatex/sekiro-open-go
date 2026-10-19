package httpapi

// /op/api/errors：系统错误原因分布与最近事件的端到端验证。
// 触发路径：对无 provider 的 group 发起调用 → 分配失败 → rec.Fail →
// dispatch-failed（message 为 "pool queue empty"）。

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/metrics"
)

func fetchErrors(t *testing.T, h http.Handler) metrics.ErrorsJSON {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/op/api/errors", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/op/api/errors status = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	var out metrics.ErrorsJSON
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode errors: %v (body=%s)", err, body)
	}
	return out
}

func TestOpErrorsEndpoint(t *testing.T) {
	metrics.Reset()
	mgr := group.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := Handler(mgr, compress.NewManager(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, "")
	t.Cleanup(mgr.Close)

	// 触发一次 dispatch-failed：空池 group（首次空池立即失败）
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/business/invoke?group=g-err&action=crawl", nil))

	e := fetchErrors(t, h)
	if e.Total != 1 || len(e.Reasons) != 1 {
		t.Fatalf("total/reasons = %d/%d, want 1/1", e.Total, len(e.Reasons))
	}
	if e.Reasons[0].Reason != metrics.ErrDispatch || e.Reasons[0].Count != 1 {
		t.Fatalf("reasons[0] = %+v, want dispatch-failed×1", e.Reasons[0])
	}
	if len(e.Recent) != 1 {
		t.Fatalf("recent len = %d, want 1", len(e.Recent))
	}
	ev := e.Recent[0]
	if ev.Group != "g-err" || ev.Action != "crawl" || ev.Reason != metrics.ErrDispatch {
		t.Fatalf("recent event = %+v", ev)
	}
	if ev.Message != "pool queue empty" {
		t.Fatalf("recent message = %q, want pool queue empty", ev.Message)
	}
	if ev.Channel != "http" {
		t.Fatalf("channel = %q, want http", ev.Channel)
	}

	// stats 快照同步携带原因分布（group 级 + 全局）
	s := fetchStats(t, h)
	i := findGroup(t, s, "g-err")
	if s.Groups[i].Metrics.SysErr != 1 {
		t.Fatalf("g-err sysErr = %d, want 1", s.Groups[i].Metrics.SysErr)
	}
	if s.Groups[i].SysErrReasons[metrics.ErrDispatch] != 1 {
		t.Fatalf("g-err sysErrReasons = %v", s.Groups[i].SysErrReasons)
	}
	if s.SysErrReasons[metrics.ErrDispatch] != 1 {
		t.Fatalf("global sysErrReasons = %v", s.SysErrReasons)
	}

	// op 关闭时端点 404
	h2 := Handler(mgr, compress.NewManager(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, "")
	rec := httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/op/api/errors", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("errors with op disabled = %d, want 404", rec.Code)
	}
}
