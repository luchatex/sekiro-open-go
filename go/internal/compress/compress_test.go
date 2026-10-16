package compress

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"sekiro-go/internal/protocol"
)

// 黄金对拍（Java 原实现产出）：
//   - 硬性：Go 解压 Java 压缩结果 == 原文（Java→Go 互通）
//   - 硬性：Go 压缩→Go 解压 roundtrip
//   - 信息性：Go 压缩与 Java 压缩逐字节一致（deflate/snappy 实现差异允许不一致）
func TestGoldenCompress(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "golden_compress.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var wrapper struct {
		Cases []struct {
			Name      string `json:"name"`
			PlainHex  string `json:"plainHex"`
			GzipHex   string `json:"gzipHex"`
			SnappyHex string `json:"snappyHex"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(wrapper.Cases) < 3 {
		t.Fatalf("too few cases: %d", len(wrapper.Cases))
	}
	for _, c := range wrapper.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			plain, _ := hex.DecodeString(c.PlainHex)
			javaGz, _ := hex.DecodeString(c.GzipHex)
			javaSn, _ := hex.DecodeString(c.SnappyHex)

			gzBack, err := (Gzip{}).Decompress(javaGz)
			if err != nil || !bytes.Equal(gzBack, plain) {
				t.Fatalf("gzip java->go interop failed: %v", err)
			}
			snBack, err := (Snappy{}).Decompress(javaSn)
			if err != nil || !bytes.Equal(snBack, plain) {
				t.Fatalf("snappy java->go interop failed: %v", err)
			}

			goGz, err := Gzip{}.Compress(plain)
			if err != nil {
				t.Fatalf("gzip compress: %v", err)
			}
			if back, err := (Gzip{}).Decompress(goGz); err != nil || !bytes.Equal(back, plain) {
				t.Fatalf("gzip go roundtrip failed: %v", err)
			}
			if bytes.Equal(goGz, javaGz) {
				t.Logf("gzip byte-identical with Java")
			} else {
				t.Logf("gzip bytes differ (acceptable): go=%d java=%d", len(goGz), len(javaGz))
			}

			goSn, err := Snappy{}.Compress(plain)
			if err != nil {
				t.Fatalf("snappy compress: %v", err)
			}
			if back, err := (Snappy{}).Decompress(goSn); err != nil || !bytes.Equal(back, plain) {
				t.Fatalf("snappy go roundtrip failed: %v", err)
			}
			if !bytes.Equal(goSn, javaSn) {
				t.Logf("snappy bytes differ (acceptable): go=%d java=%d", len(goSn), len(javaSn))
			}
		})
	}
}

func TestManagerGating(t *testing.T) {
	m := NewManager()

	// 非响应类包：即使带头也不压缩/解压
	p := protocol.NewPacket(protocol.STypeInvoke)
	p.AddHeader(protocol.CompressMethod, protocol.CompressGzip)
	p.Data = []byte("x")
	if err := m.CompressPacket(p); err != nil {
		t.Fatalf("non-response should skip: %v", err)
	}
	if string(p.Data) != "x" {
		t.Fatal("non-response data must be untouched")
	}

	// 响应类 + gzip 头：压缩后可解压回
	r := protocol.NewPacket(protocol.STypeInvokeResponse)
	r.AddHeader(protocol.CompressMethod, protocol.CompressGzip)
	r.Data = []byte("调用失败：参数校验未通过，请检查签名")
	if err := m.CompressPacket(r); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if bytes.Equal(r.Data, []byte("调用失败：参数校验未通过，请检查签名")) {
		t.Fatal("expected compressed bytes")
	}
	if r.GetHeader(protocol.CompressMethod) != protocol.CompressGzip {
		t.Fatal("COMPRESS_METHOD header must be preserved")
	}
	if err := m.DecompressPacket(r); err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(r.Data) != "调用失败：参数校验未通过，请检查签名" {
		t.Fatal("roundtrip mismatch")
	}

	// 未知方法：报错
	u := protocol.NewPacket(protocol.STypeInvokeResponse)
	u.AddHeader(protocol.CompressMethod, "COMPRESS_METHOD_BOGUS")
	if err := m.DecompressPacket(u); err == nil {
		t.Fatal("expected error for unknown compress method")
	}

	// 无头：跳过
	n := protocol.NewPacket(protocol.STypeInvokeResponse)
	n.Data = []byte("raw")
	if err := m.DecompressPacket(n); err != nil {
		t.Fatalf("blank header should skip: %v", err)
	}
	if string(n.Data) != "raw" {
		t.Fatal("data untouched")
	}
}
