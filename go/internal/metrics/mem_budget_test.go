package metrics

// 快照分配预算回归护栏。
//
// 背景：/op/api/stats 每 5s 被前端拉一次，单次分配量直接决定 GC 压力。
// 早期实现为「每个 group 的每个桶都 new 一个 Metrics」，20 个 group 时
// 单次约 121 KB（其中约 84 KB 来自 group 级桶聚合）。改为定长数组按下标
// 累加、并预分配 Groups/Actions 后降到约 36 KB。本用例锁住这个量级，
// 防止有人改回 map 聚合。

import (
	"runtime"
	"testing"
	"time"
)

// snapshotBudgetPerCall 单次快照的分配预算（60 个维度、20 个 group）。
// 实测 36 KB，留约 25% 余量以吸收 Go 版本/平台差异。
const snapshotBudgetPerCall = 46 << 10

func TestSnapshotAllocBudget(t *testing.T) {
	r := New()
	now := time.Now()
	curH := now.Unix() / 3600
	today := localDayStart(now)

	r.mu.Lock()
	for gi := 0; gi < 20; gi++ {
		for ai := 0; ai < 3; ai++ {
			e := r.entityLocked(string(rune('a'+gi)), string(rune('A'+ai)))
			for k := 0; k < 26; k++ {
				e.hourLocked(curH-int64(k)).Total++
			}
			for k := 0; k < 8; k++ {
				e.dayLocked(today.AddDate(0, 0, -k).Unix()).Total++
			}
			e.reasons = map[string]int64{ErrDispatch: 1}
		}
	}
	r.mu.Unlock()

	names := make([]string, 0, 20)
	for gi := 0; gi < 20; gi++ {
		names = append(names, string(rune('a'+gi)))
	}
	clients := func(string) int { return 1 }

	// TotalAlloc 单调递增、与 GC 时机无关，是确定性指标
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const n = 100
	for i := 0; i < n; i++ {
		_ = r.Snapshot(clients, names)
	}
	runtime.ReadMemStats(&after)

	perCall := float64(after.TotalAlloc-before.TotalAlloc) / n
	if perCall > snapshotBudgetPerCall {
		t.Fatalf("Snapshot 单次分配 %.1f KB，超出预算 %.0f KB",
			perCall/1024, float64(snapshotBudgetPerCall)/1024)
	}
	t.Logf("Snapshot 单次分配 %.1f KB（预算 %.0f KB，60 维度 / 20 group）",
		perCall/1024, float64(snapshotBudgetPerCall)/1024)
}
