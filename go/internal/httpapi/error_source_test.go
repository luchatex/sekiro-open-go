package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/metrics"
)

// TestErrorEventCarriesSourceIP 错误事件必须带调用方来源 IP，且反代后取
// 可信转发头（X-Real-IP）；这样 nginx access log 不记录 invoke 参数时，
// 仍能从 /op/api/errors 直接看出是谁在发失败请求。
func TestErrorEventCarriesSourceIP(t *testing.T) {
	metrics.Reset()
	t.Cleanup(func() { metrics.Reset() })

	mgr := group.NewManager(slog.Default())
	h := Handler(mgr, compress.NewManager(), slog.Default(), true, "")

	cases := []struct {
		name       string
		remoteAddr string
		headerIP   string
		want       string
	}{
		{"直连", "203.0.113.9:4567", "", "203.0.113.9"},
		{"反代", "127.0.0.1:1234", "198.51.100.7", "198.51.100.7"},
	}
	for i, c := range cases {
		req := httptest.NewRequest(http.MethodGet,
			"/business/invoke?group=missing&action=a", nil)
		req.RemoteAddr = c.remoteAddr
		if c.headerIP != "" {
			req.Header.Set("X-Real-IP", c.headerIP)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)

		recent := metrics.Default().Errors().Recent
		if len(recent) != i+1 {
			t.Fatalf("%s: recent 长度 = %d, want %d", c.name, len(recent), i+1)
		}
		if got := recent[0].SourceIP; got != c.want {
			t.Errorf("%s: sourceIp = %q, want %q", c.name, got, c.want)
		}
	}
}
