package orderedjson

import (
	"bytes"
	"testing"
)

// 期望值来自 tests/lib 私有 jar 驱动原版 fastjson 的实证探针
// （FjProbe3/FjProbe4.java，见 tests/SAMPLES-VERIFY.md）。

// TestAppendJSONStringFastjsonAligned 对齐 fastjson SerializeWriter.writeString：
// 不转义 HTML/斜杠/单引号，控制字符短形式 + \u00XX 大写十六进制（含 0x7F）。
func TestAppendJSONStringFastjsonAligned(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<html>&"quote"\back</html>`, `"<html>&\"quote\"\\back</html>"`},
		{`http://example.com/a?b=1&c=2`, `"http://example.com/a?b=1&c=2"`},
		{"中文内容", `"中文内容"`},
		{`it's "q" /slash`, `"it's \"q\" /slash"`},
		{"bell\x07del\x7f", `"bell\u0007del\u007F"`},
		{"nl\n\ttab\r\bf\ff\bb\x00\x1a", `"nl\n\ttab\r\bf\ff\bb\u0000\u001A"`},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		AppendJSONString(&buf, c.in)
		if buf.String() != c.want {
			t.Errorf("AppendJSONString(%q) = %s, want %s", c.in, buf.String(), c.want)
		}
	}
}

// TestMarshalJSONSortedFastjsonAligned 对齐 fastjson toJSONString 默认 SortField：
// 递归字典序（嵌套对象与数组内对象均排序），数字字面量保留。
func TestMarshalJSONSortedFastjsonAligned(t *testing.T) {
	src := `{"zz":{"m2":1,"a1":"<b>&x</b>","kk":[{"y":2,"b":3}]},"aa":1.50,"mM":"http://e.com/p?q=1&z=2"}`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m.Set("clientId", "dev-1")
	out, err := m.MarshalJSONSorted()
	if err != nil {
		t.Fatalf("marshal sorted: %v", err)
	}
	// FjProbe3 实证输出（parse → put clientId → toJSONString）
	want := `{"aa":1.50,"clientId":"dev-1","mM":"http://e.com/p?q=1&z=2","zz":{"a1":"<b>&x</b>","kk":[{"b":3,"y":2}],"m2":1}}`
	if string(out) != want {
		t.Fatalf("sorted marshal mismatch:\n got %s\nwant %s", out, want)
	}
}

// TestMarshalInsertOrderKept MarshalJSON 仍保持插入序（OrderedField 语义，
// 请求参数转发路径依赖）。
func TestMarshalInsertOrderKept(t *testing.T) {
	src := `{"zz":1,"aa":"<b>","mm":{"y":2,"a":1}}`
	m, _ := Parse([]byte(src))
	out, err := m.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != src {
		t.Fatalf("insert order broken:\n got %s\nwant %s", out, src)
	}
}
