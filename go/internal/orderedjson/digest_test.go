package orderedjson

// Digest 回归测试：错误事件附带参数摘要的实现基础。

import (
	"strings"
	"testing"
)

func TestDigestSkipsAndKeepsOrder(t *testing.T) {
	m := New()
	m.Set("group", "g1")
	m.Set("docId", "abc")
	m.Set("action", "getDetailData")
	m.Set("url", "https://example.com/d")

	got := m.Digest(func(k string) bool { return k == "group" || k == "action" }, 256)
	want := `{"docId":"abc","url":"https://example.com/d"}`
	if got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
}

func TestDigestTruncatesOnRuneBoundary(t *testing.T) {
	m := New()
	m.Set("q", strings.Repeat("中", 20)) // 每个汉字 JSON 转义前后 1 字节（不转义）
	got := m.Digest(nil, 10)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长摘要应加省略号: %q", got)
	}
	// 截断后必须是合法 UTF-8（不回退到半个 rune 的中间）
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("摘要截断破坏了 UTF-8: %q", got)
		}
	}
	if len(got) > 10+len("…")+2 { // 预算 + 省略号 + JSON 花括号余量
		t.Fatalf("摘要长度 %d 超出预算 10", len(got))
	}
}

func TestDigestEmpty(t *testing.T) {
	m := New()
	if got := m.Digest(nil, 256); got != "" {
		t.Fatalf("空 Map digest = %q, want 空串", got)
	}
	m.Set("group", "g1")
	if got := m.Digest(func(k string) bool { return k == "group" }, 256); got != "" {
		t.Fatalf("全部被跳过时 digest = %q, want 空串", got)
	}
	var nilMap *Map
	if got := nilMap.Digest(nil, 256); got != "" {
		t.Fatalf("nil Map digest = %q, want 空串", got)
	}
}
