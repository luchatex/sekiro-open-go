package protocol

import "encoding/binary"

// MurmurHash2 64 位（与 Java 版 ConsistentHashUtil.murHash 一致）：
// seed 0x1234ABCD，m 0xc6a4a7935bd1e995，r 47，小端读取，尾部零填充。
// Java 侧 key.getBytes() 在 UTF-8 环境下取 UTF-8 字节，Go 侧 []byte(s) 等价。
func MurmurHash64(key []byte) uint64 {
	const (
		seed = 0x1234ABCD
		m    = 0xc6a4a7935bd1e995
		r    = 47
	)
	n := len(key)
	h := uint64(seed) ^ (uint64(n) * m)
	i := 0
	for ; i+8 <= n; i += 8 {
		k := binary.LittleEndian.Uint64(key[i:])
		k *= m
		k ^= k >> r
		k *= m
		h ^= k
		h *= m
	}
	if n-i > 0 {
		var tail [8]byte
		copy(tail[:], key[i:])
		k := binary.LittleEndian.Uint64(tail[:])
		h ^= k
		h *= m
	}
	h ^= h >> r
	h *= m
	h ^= h >> r
	return h
}

// MurmurHashString MurmurHash64 的字符串便捷入口。
func MurmurHashString(key string) uint64 {
	return MurmurHash64([]byte(key))
}
