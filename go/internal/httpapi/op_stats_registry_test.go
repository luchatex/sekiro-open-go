package httpapi

// /op/api/stats 的 group 可见性回归：
// 注册了 client 但尚无调用流量的 group 也必须出现在快照中——
// 修复前 groups 只从 metrics 维度表构建，「client 注册后监控页不可见、
// 有了真实请求才出现」。

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/metrics"
)

// TestStatsShowsRegisteredGroupWithoutTraffic 核心回归。
func TestStatsShowsRegisteredGroupWithoutTraffic(t *testing.T) {
	metrics.Reset()
	mgr := group.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := Handler(mgr, compress.NewManager(), slog.Default(), true, "")

	// 注册一个假 provider，不产生任何调用
	g := mgr.GetOrCreate("fresh-group")
	g.Register(group.NewClient(&silentTransport{active: true}, g, "fresh-1", slog.Default()))

	// 轮询等注册经 actor 生效（最多 2s）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := fetchStats(t, h)
		if len(s.Groups) > 0 && s.Groups[0].Group == "fresh-group" {
			gj := s.Groups[0]
			if gj.Clients != 1 {
				t.Fatalf("clients = %d, want 1", gj.Clients)
			}
			if gj.Metrics.Total != 0 {
				t.Fatalf("零流量 group 的 total = %d, want 0", gj.Metrics.Total)
			}
			t.Log("STATS-VISIBILITY: 注册后立即可见（clients=1, total=0）")

			// 未注册的 group 不得凭空出现
			s2 := fetchStats(t, h)
			for _, gj2 := range s2.Groups {
				if gj2.Group == "ghost-group" {
					t.Fatalf("未注册 group %q 不应出现", gj2.Group)
				}
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("注册后的 group 未出现在 /op/api/stats（修复前只在有调用后才可见）")
}
