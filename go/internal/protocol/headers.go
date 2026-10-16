package protocol

// Headers 有序 header 集合。
//
// Java 版 SekiroPacket.headers 是 HashMap（迭代序不确定但解码方不依赖顺序）；
// Go 版保持插入序以获得确定性的编码输出。线上语义：解码方按 headerCount
// 顺序读取、构建 map，不依赖顺序，故插入序编码与 Java 输出语义等价。
type Headers struct {
	keys []string
	vals []string
	idx  map[string]int
}

func NewHeaders() *Headers {
	return &Headers{idx: make(map[string]int)}
}

// Set 添加或更新（更新保持原位置）。
func (h *Headers) Set(k, v string) {
	if i, ok := h.idx[k]; ok {
		h.vals[i] = v
		return
	}
	h.idx[k] = len(h.keys)
	h.keys = append(h.keys, k)
	h.vals = append(h.vals, v)
}

func (h *Headers) Get(k string) string {
	if i, ok := h.idx[k]; ok {
		return h.vals[i]
	}
	return ""
}

func (h *Headers) Has(k string) bool {
	_, ok := h.idx[k]
	return ok
}

func (h *Headers) Remove(k string) {
	i, ok := h.idx[k]
	if !ok {
		return
	}
	h.keys = append(h.keys[:i], h.keys[i+1:]...)
	h.vals = append(h.vals[:i], h.vals[i+1:]...)
	delete(h.idx, k)
	for j := i; j < len(h.keys); j++ {
		h.idx[h.keys[j]] = j
	}
}

func (h *Headers) Len() int { return len(h.keys) }

// Each 按插入序遍历。
func (h *Headers) Each(fn func(k, v string)) {
	for i := range h.keys {
		fn(h.keys[i], h.vals[i])
	}
}

func (h *Headers) Clone() *Headers {
	n := NewHeaders()
	n.keys = append(n.keys, h.keys...)
	n.vals = append(n.vals, h.vals...)
	for k, v := range h.idx {
		n.idx[k] = v
	}
	return n
}

// Merge 将 src 的键值并入（已存在则覆盖）。
func (h *Headers) Merge(src *Headers) {
	src.Each(func(k, v string) { h.Set(k, v) })
}
