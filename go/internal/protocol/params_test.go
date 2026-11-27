package protocol

import (
	"testing"
	"time"

	"sekiro-go/internal/orderedjson"
)

func TestInvokeTimeoutMillis(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*orderedjson.Map)
		want  int
	}{
		{"nil params", nil, 0},
		{"正确拼写", func(m *orderedjson.Map) { m.Set(ParamInvokeTimeout, "100000") }, 100000},
		{"历史拼写", func(m *orderedjson.Map) { m.Set(ParamInvokeTimeoutAlias, "100000") }, 100000},
		{"两者共存时正确拼写优先", func(m *orderedjson.Map) {
			m.Set(ParamInvokeTimeoutAlias, "100000")
			m.Set(ParamInvokeTimeout, "30000")
		}, 30000},
		{"正确拼写为0时回落", func(m *orderedjson.Map) {
			m.Set(ParamInvokeTimeout, "0")
			m.Set(ParamInvokeTimeoutAlias, "60000")
		}, 60000},
	}
	for _, c := range cases {
		var m *orderedjson.Map
		if c.setup != nil {
			m = orderedjson.New()
			c.setup(m)
		}
		if got := InvokeTimeoutMillis(m); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestDefaultInvokeTimeoutIs30s(t *testing.T) {
	if got := DefaultInvokeTimeout; got != 30*time.Second {
		t.Fatalf("DefaultInvokeTimeout = %v, want 30s", got)
	}
}
