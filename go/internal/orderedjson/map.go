// Package orderedjson 提供保序 JSON 对象，对齐 fastjson Feature.OrderedField
// 与 Java 版请求参数的 LinkedHashMap 语义：键序 = 插入序，转发报文逐键可复现。
package orderedjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

type Map struct {
	keys []string
	vals []any
	idx  map[string]int
}

func New() *Map {
	return &Map{idx: make(map[string]int)}
}

func (m *Map) Set(k string, v any) {
	if i, ok := m.idx[k]; ok {
		m.vals[i] = v
		return
	}
	m.idx[k] = len(m.keys)
	m.keys = append(m.keys, k)
	m.vals = append(m.vals, v)
}

func (m *Map) Get(k string) (any, bool) {
	if i, ok := m.idx[k]; ok {
		return m.vals[i], true
	}
	return nil, false
}

func (m *Map) GetString(k string) string {
	if v, ok := m.Get(k); ok {
		if s, ok := v.(string); ok {
			return s
		}
		if n, ok := v.(json.Number); ok {
			return n.String()
		}
	}
	return ""
}

func (m *Map) GetInt(k string) int {
	if v, ok := m.Get(k); ok {
		switch n := v.(type) {
		case json.Number:
			i, _ := strconv.Atoi(n.String())
			return i
		case string:
			i, _ := strconv.Atoi(n)
			return i
		}
	}
	return 0
}

func (m *Map) Has(k string) bool {
	_, ok := m.idx[k]
	return ok
}

func (m *Map) Remove(k string) {
	i, ok := m.idx[k]
	if !ok {
		return
	}
	m.keys = append(m.keys[:i], m.keys[i+1:]...)
	m.vals = append(m.vals[:i], m.vals[i+1:]...)
	delete(m.idx, k)
	for j := i; j < len(m.keys); j++ {
		m.idx[m.keys[j]] = j
	}
}

func (m *Map) Len() int { return len(m.keys) }

// String 序列化为 JSON 字符串（日志与转发用）。
func (m *Map) String() string {
	b, err := m.MarshalJSON()
	if err != nil {
		return "{}"
	}
	return string(b)
}

// SetAll 按 src 的插入序并入（已存在覆盖）。
func (m *Map) SetAll(src *Map) {
	if src == nil {
		return
	}
	for i, k := range src.keys {
		m.Set(k, src.vals[i])
	}
}

// MarshalJSON 按插入序输出。
func (m *Map) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalValue(m.vals[i])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func marshalValue(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return []byte("null"), nil
	case json.Number:
		return []byte(t.String()), nil
	case string:
		return json.Marshal(t)
	case bool:
		return json.Marshal(t)
	case *Map:
		return t.MarshalJSON()
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			vb, err := marshalValue(e)
			if err != nil {
				return nil, err
			}
			buf.Write(vb)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return json.Marshal(v)
	}
}

// Parse 解析 JSON 对象，保持键插入序；数字用 json.Number 保留原始字面量。
func Parse(data []byte) (*Map, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	m, err := decodeObject(dec)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func decodeObject(dec *json.Decoder) (*Map, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return nil, errors.New("expected json object")
	}
	m := New()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("object key not string: %v", kt)
		}
		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		m.Set(key, v)
	}
	// consume '}'
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return m, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return decodeToken(dec, tok)
}

func decodeToken(dec *json.Decoder, tok json.Token) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			// 已消耗 '{'，继续读键值
			m := New()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key not string: %v", kt)
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				m.Set(key, v)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return m, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return arr, nil
		default:
			return nil, fmt.Errorf("unexpected delim %v", t)
		}
	default:
		return tok, nil // string / json.Number / bool / nil
	}
}

// FormParse 解析 x-www-form-urlencoded 到 Map（对齐 QueryStringDecoder(postBody,false)：值为 list 取首个）。
func FormParse(body []byte) (*Map, error) {
	m := New()
	for len(body) > 0 {
		i := bytes.IndexByte(body, '&')
		var pair []byte
		if i < 0 {
			pair, body = body, nil
		} else {
			pair, body = body[:i], body[i+1:]
		}
		if len(pair) == 0 {
			continue
		}
		k, v := pair, []byte("")
		if j := bytes.IndexByte(pair, '='); j >= 0 {
			k, v = pair[:j], pair[j+1:]
		}
		key, err := urlUnescape(string(k))
		if err != nil {
			continue
		}
		val, err := urlUnescape(string(v))
		if err != nil {
			continue
		}
		m.Set(key, val)
	}
	return m, nil
}

// urlUnescape 简化版 query unescape（+ → 空格，%XX 解码）。
func urlUnescape(s string) (string, error) {
	var buf bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+':
			buf.WriteByte(' ')
		case c == '%':
			if i+2 >= len(s) {
				return "", errors.New("bad escape")
			}
			hi := unhex(s[i+1])
			lo := unhex(s[i+2])
			if hi < 0 || lo < 0 {
				return "", errors.New("bad escape hex")
			}
			buf.WriteByte(byte(hi<<4 | lo))
			i += 2
		default:
			buf.WriteByte(c)
		}
	}
	return buf.String(), nil
}

func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// SortKeys 工具：返回排序键副本（groupList 用）。
func SortKeys(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
