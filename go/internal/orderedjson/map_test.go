package orderedjson

import (
	"encoding/json"
	"testing"
)

func TestParsePreservesOrder(t *testing.T) {
	src := `{"group":"demo","action":"fetch","zeta":1,"alpha":"中文","nested":{"y":2,"a":true},"arr":[1,"x"]}`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"group", "action", "zeta", "alpha", "nested", "arr"}
	if len(m.keys) != len(want) {
		t.Fatalf("key count %d want %d", len(m.keys), len(want))
	}
	for i, k := range want {
		if m.keys[i] != k {
			t.Fatalf("key[%d]=%s want %s", i, m.keys[i], k)
		}
	}
	if m.GetString("alpha") != "中文" {
		t.Fatal("chinese value broken")
	}
	if m.GetInt("zeta") != 1 {
		t.Fatal("int value broken")
	}

	// 序列化往返：键序与数字字面量不变
	out, err := m.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != src {
		t.Fatalf("marshal mismatch:\n got %s\nwant %s", out, src)
	}
}

func TestNumberLiteralPreserved(t *testing.T) {
	m, _ := Parse([]byte(`{"a":1.50,"b":1e3}`))
	out, _ := m.MarshalJSON()
	// json.Number 保留原始字面量（fastjson 同样保留原文）
	if string(out) != `{"a":1.50,"b":1e3}` {
		t.Fatalf("number literal changed: %s", out)
	}
}

func TestFormParse(t *testing.T) {
	m, err := FormParse([]byte("group=demo+group&param=%E4%B8%AD%E6%96%87&empty="))
	if err != nil {
		t.Fatalf("form parse: %v", err)
	}
	if m.GetString("group") != "demo group" {
		t.Fatalf("group=%q", m.GetString("group"))
	}
	if m.GetString("param") != "中文" {
		t.Fatalf("param=%q", m.GetString("param"))
	}
	if m.GetString("empty") != "" {
		t.Fatal("empty broken")
	}
}

func TestSetAllBodyWins(t *testing.T) {
	q, _ := Parse([]byte(`{"group":"q","action":"qa"}`))
	body, _ := Parse([]byte(`{"action":"ba","extra":1}`))
	q.SetAll(body)
	if q.GetString("action") != "ba" {
		t.Fatal("body should win")
	}
	if q.GetString("group") != "q" {
		t.Fatal("query kept")
	}
	if q.GetInt("extra") != 1 {
		t.Fatal("extra missing")
	}
	// 序: group, action(原位), extra
	if q.keys[0] != "group" || q.keys[1] != "action" || q.keys[2] != "extra" {
		t.Fatalf("order: %v", q.keys)
	}
}

func TestRemoveKeepsOrder(t *testing.T) {
	m, _ := Parse([]byte(`{"a":1,"b":2,"c":3,"__sekiro_seq__":9}`))
	m.Remove("__sekiro_seq__")
	out, _ := m.MarshalJSON()
	if string(out) != `{"a":1,"b":2,"c":3}` {
		t.Fatalf("after remove: %s", out)
	}
	var std map[string]any
	if err := json.Unmarshal(out, &std); err != nil {
		t.Fatalf("not valid json: %v", err)
	}
}
