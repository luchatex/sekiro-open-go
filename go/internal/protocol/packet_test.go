package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPacketRoundtrip(t *testing.T) {
	p := NewPacket(CTypeRegister)
	p.Seq = 42
	p.AddHeader(HeaderSekiroGroup, "demo-group")
	p.AddHeader(HeaderClientID, "测试客户端-01")
	p.Data = []byte(`{"action":"fetch","param":"参数校验"}`)

	b, err := p.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := ReadPacket(bufio.NewReader(bytes.NewReader(b)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != p.Type || got.Seq != p.Seq {
		t.Fatalf("type/seq mismatch: %v/%v vs %v/%v", got.Type, got.Seq, p.Type, p.Seq)
	}
	if got.GetHeader(HeaderSekiroGroup) != "demo-group" ||
		got.GetHeader(HeaderClientID) != "测试客户端-01" {
		t.Fatalf("headers mismatch: %v", got.Headers)
	}
	if !bytes.Equal(got.Data, p.Data) {
		t.Fatalf("data mismatch")
	}
}

func TestEncodeLimits(t *testing.T) {
	p := NewPacket(TypeHeartbeat)
	for i := 0; i < 128; i++ {
		p.AddHeader(string(rune('a'+i%26))+string(rune(i)), "v")
	}
	if _, err := p.Encode(); err == nil {
		t.Fatal("expected ErrTooManyHeaders for 128 headers")
	}

	p2 := NewPacket(TypeHeartbeat)
	p2.AddHeader(string(make([]byte, 256)), "v")
	if _, err := p2.Encode(); err == nil {
		t.Fatal("expected ErrHeaderTooLong for 256-byte key")
	}
}

func TestBadMagic(t *testing.T) {
	buf := bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 0, 0, 0, 0})
	if _, err := ReadPacket(bufio.NewReader(buf)); err != ErrBadMagic {
		t.Fatalf("expected ErrBadMagic, got %v", err)
	}
}

func TestFrameTooSmall(t *testing.T) {
	// 对端发 totalLen<6 的帧：必须返回错误而非索引越界 panic
	// （修复前 body[0] 越界 → readLoop panic → 整进程崩溃的 DoS 面）
	var magic [8]byte
	binary.BigEndian.PutUint64(magic[:], Magic)
	for _, n := range []uint32{0, 1, 5} {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], n)
		frame := append(append(magic[:], lenBuf[:]...), make([]byte, n)...)
		if _, err := ReadPacket(bufio.NewReader(bytes.NewReader(frame))); err != ErrFrameTooSmall {
			t.Fatalf("bodyLen=%d: expected ErrFrameTooSmall, got %v", n, err)
		}
	}
}

func TestPartialReadWaits(t *testing.T) {
	// 半包：只有 6 字节头部，ReadPacket 应阻塞等待而非报错
	p := NewPacket(TypeHeartbeat)
	p.Seq = 1
	b, _ := p.Encode()
	r := bufio.NewReader(bytes.NewReader(b[:6]))
	done := make(chan error, 1)
	go func() {
		_, err := ReadPacket(r)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("ReadPacket returned early on partial frame: %v", err)
	default:
	}
}

// ---- golden 对拍（Java 原实现产出） ----

type goldenFrame struct {
	Name       string            `json:"name"`
	Type       byte              `json:"type"`
	Seq        int32             `json:"seq"`
	Headers    map[string]string `json:"headers"`
	PayloadHex string            `json:"payloadHex"`
	FrameHex   string            `json:"frameHex"`
}

func loadGolden[T any](t *testing.T, name string) []T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", name))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	var wrapper struct {
		Cases []T `json:"cases"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		t.Fatalf("parse golden file: %v", err)
	}
	return wrapper.Cases
}

func TestGoldenFrames(t *testing.T) {
	cases := loadGolden[goldenFrame](t, "golden_frames.json")
	if len(cases) < 10 {
		t.Fatalf("too few golden cases: %d", len(cases))
	}
	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			frame, err := hex.DecodeString(c.FrameHex)
			if err != nil {
				t.Fatalf("bad frame hex: %v", err)
			}
			// 1) Go 解码 Java 帧 → 字段语义一致
			got, err := ReadPacket(bufio.NewReader(bytes.NewReader(frame)))
			if err != nil {
				t.Fatalf("decode java frame: %v", err)
			}
			if byte(got.Type) != c.Type || got.Seq != c.Seq {
				t.Fatalf("type/seq: %v/%v want %v/%v", got.Type, got.Seq, c.Type, c.Seq)
			}
			if len(c.Headers) != got.Headers.Len() {
				t.Fatalf("header count: %d want %d", got.Headers.Len(), len(c.Headers))
			}
			for k, v := range c.Headers {
				if got.GetHeader(k) != v {
					t.Fatalf("header %s: %q want %q", k, got.GetHeader(k), v)
				}
			}
			wantPayload, _ := hex.DecodeString(c.PayloadHex)
			if !bytes.Equal(got.Data, wantPayload) {
				t.Fatalf("payload mismatch: %d bytes vs %d bytes", len(got.Data), len(wantPayload))
			}

			// 2) Go 编码 → 字节级一致（headers ≤1 个时 Java HashMap 序唯一确定；
			//    多 header 时 Java HashMap 序不确定，转语义比较）
			enc := NewPacket(Type(c.Type))
			enc.Seq = c.Seq
			for k, v := range c.Headers {
				enc.AddHeader(k, v)
			}
			enc.Data = wantPayload
			out, err := enc.Encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if len(c.Headers) <= 1 {
				if !bytes.Equal(out, frame) {
					t.Fatalf("byte mismatch for single-header case:\n got %x\nwant %x", out, frame)
				}
			} else {
				back, err := ReadPacket(bufio.NewReader(bytes.NewReader(out)))
				if err != nil {
					t.Fatalf("re-decode go frame: %v", err)
				}
				if back.Type != got.Type || back.Seq != got.Seq || !bytes.Equal(back.Data, got.Data) ||
					back.Headers.Len() != got.Headers.Len() {
					t.Fatalf("semantic mismatch after re-encode")
				}
			}
		})
	}
}
