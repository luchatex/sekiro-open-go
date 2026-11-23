package compress

import (
	"bytes"
	"testing"
)

// TestGzipPoolReuse 验证池化后的 gzip.Writer/Reader 在连续调用中不串数据、
// 不丢失 Java 对齐所需的 OS=0 头字节。
func TestGzipPoolReuse(t *testing.T) {
	g := Gzip{}
	payloads := [][]byte{
		[]byte("first payload"),
		bytes.Repeat([]byte("A"), 300<<10), // 触发多 chunk 解压
		[]byte(""),
		{0x00, 0xff, 0x01, 0x80}, // 二进制载荷
	}
	var firstOut []byte
	for i, raw := range payloads {
		enc, err := g.Compress(raw)
		if err != nil {
			t.Fatalf("#%d compress: %v", i, err)
		}
		if len(enc) > 10 && enc[9] != 0 {
			t.Fatalf("#%d gzip header OS = %d, want 0 (Reset 后必须重设)", i, enc[9])
		}
		dec, err := g.Decompress(enc)
		if err != nil {
			t.Fatalf("#%d decompress: %v", i, err)
		}
		if !bytes.Equal(dec, raw) {
			t.Fatalf("#%d round-trip mismatch: got %d bytes, want %d", i, len(dec), len(raw))
		}
		if i == 0 {
			firstOut = enc
		}
	}
	// 前一次压缩结果不能被后续复用同一个 writer 覆盖（返回值不能与池共享）。
	dec, err := g.Decompress(firstOut)
	if err != nil || !bytes.Equal(dec, payloads[0]) {
		t.Fatalf("first compressed buffer was corrupted by later reuse: err=%v", err)
	}
}

// TestGzipReaderPoolAfterResetError Reset 失败的 reader 不归还池；下一次调用
// 仍能拿到可用的 reader（要么新建、要么健康的旧对象）。
func TestGzipReaderPoolAfterResetError(t *testing.T) {
	g := Gzip{}
	if _, err := g.Decompress([]byte("not a gzip stream")); err == nil {
		t.Fatal("invalid gzip must fail")
	}
	enc, err := g.Compress([]byte("still works"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.Decompress(enc)
	if err != nil || string(got) != "still works" {
		t.Fatalf("reader pool poisoned: got %q, err=%v", got, err)
	}
}
