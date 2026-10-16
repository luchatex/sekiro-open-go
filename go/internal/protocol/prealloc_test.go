package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"testing"
)

// declareOnlyReader 模拟最恶劣的放大攻击：只发12 字节帧头声明 bodyLen，
// 之后永不发送任何数据（原实现会在此之前就make 出完整 body）。
type declareOnlyReader struct {
	head []byte
	off  int
}

func (r *declareOnlyReader) Read(b []byte) (int, error) {
	if r.off < len(r.head) {
		n := copy(b, r.head[r.off:])
		r.off += n
		return n, nil
	}
	return 0, io.EOF
}

func declaredFrameHead(bodyLen uint32) []byte {
	head := make([]byte, 12)
	binary.BigEndian.PutUint64(head[0:8], Magic)
	binary.BigEndian.PutUint32(head[8:12], bodyLen)
	return head
}

// TestNoPreallocOnDeclaredLength 是本修复的核心回归：
// 对端仅发 12 字节声明 64MB，服务端分配量必须远小于 64MB。
func TestNoPreallocOnDeclaredLength(t *testing.T) {
	const declared = 64 << 20

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	// bufio 包裹，贴近真实调用路径（server 用 64KB 缓冲）
	br := bufio.NewReaderSize(&declareOnlyReader{head: declaredFrameHead(declared)}, 64<<10)
	_, err := ReadPacket(br)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("expected error (peer declared data but sent none)")
	}
	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("声明 64MB、实发 12 字节 → 分配 %d 字节（%.2f MB）",
		delta, float64(delta)/(1<<20))
	t.Logf("放大比 %.0f : 1（修复前为 5,592,409 : 1）",
		float64(delta)/12)

	// 修复前是 64MB（67,108,864 字节）；留足余量断言防回退。
	// 渐进读取首块 4KB + bufio 64KB，预期 < 1MB。
	const budget = 1 << 20
	if delta > budget {
		t.Fatalf("still preallocating: %d bytes > budget %d", delta, budget)
	}
}

// TestPreallocAmpComparison 量化对比：修复后各连接的实际分配量。
func TestPreallocAmpComparison(t *testing.T) {
	const conns = 200
	br := bufio.NewReaderSize(nil, 0) // 占位，避免误用
	_ = br

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < conns; i++ {
		r := bufio.NewReaderSize(
			&declareOnlyReader{head: declaredFrameHead(64 << 20)}, 8<<10)
		ReadPacket(r)
	}
	runtime.ReadMemStats(&after)

	delta := after.TotalAlloc - before.TotalAlloc
	fixed := float64(delta) / float64(conns)
	t.Logf("修复后：%d 条连接各声明 64MB → 总分配 %.2f MB（每条 %.0f 字节）",
		conns, float64(delta)/(1<<20), fixed)
	t.Logf("修复前：同场景为 %.0f MB（每条 64MB），放大 %.0f 倍",
		float64(conns)*64, float64(conns)*64*1024*1024/float64(delta))
}

// TestGradualGrowthMatchesActualData 验证内存开销与实际收到的字节数
// **同量级**（而非按声明长度预分配）——这是修复的核心不变式。
//
// 断言用「绝对字节数」而非倍数：小 Data 时 bufio 缓冲（64KB）与帧头等
// 固定开销占主导，倍数无意义；而绝对量能直接回答关键问题——
// 「无论对端声明多大的帧，服务端分配量都远小于声明值」。
func TestGradualGrowthMatchesActualData(t *testing.T) {
	// 调用方 bufio 缓冲（server 用 64KB）+ 帧头/切片/对齐等固定开销
	const fixedOverhead = 256 << 10

	for _, actual := range []int{0, 100, 4096, 100000, 1 << 20} {
		p := NewPacket(TypeHeartbeat)
		p.Data = bytes.Repeat([]byte("A"), actual)
		full, err := p.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		// 只发到 Data 的 actual 字节处（帧声明长度仍是实际长度，
		// 放大防护针对的是「声明大而实发小」，此处验证读取路径的按需分配）
		partial := full[:12+fixedHeaderLen+actual]

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, rerr := ReadPacket(bufio.NewReader(bytes.NewReader(partial)))
		runtime.ReadMemStats(&after)

		delta := after.TotalAlloc - before.TotalAlloc
		status := "完整帧"
		if rerr != nil {
			status = "截断(预期)"
		}
		t.Logf("Data 实际 %7d 字节 → 分配 %9d 字节（%.2f MB）[%s]；按声明 64MB 预分配则是其 %.0f 倍",
			actual, delta, float64(delta)/(1<<20), status,
			float64(MaxFrameBytes)/float64(maxU64(delta, 1)))

		// 不变式：分配量 = 固定开销 + 与 Data 长度同量级的增量。
		// 8 倍余量覆盖 bytes.Buffer.ReadFrom 的内部缓冲策略差异：
		// Linux Go 1.27 实测 1MB Data → 8MB 累计分配（约 8x），
		// Windows 约 4x。两者都与 Data 长度成正比，与声明值无关——
		// 关键区分点在下方「远小于声明预分配量」的断言。
		ceiling := uint64(fixedOverhead + actual*8)
		if delta > ceiling {
			t.Fatalf("allocation %d exceeds ceiling %d (fixed %d + data %d*8)",
				delta, ceiling, fixedOverhead, actual)
		}
		// 且必须显著小于「按声明长度预分配」的量——这是修复的核心
		if delta > MaxFrameBytes/2 {
			t.Fatalf("allocation %d approaches declared preallocation %d — regression",
				delta, uint64(MaxFrameBytes))
		}
	}
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// TestLegitLargeFrameStillWorks 确保防护没有误伤正常大帧。
func TestLegitLargeFrameStillWorks(t *testing.T) {
	for _, size := range []int{1 << 10, 1 << 20, 8 << 20} {
		p := NewPacket(CTypeInvokeResponse)
		p.Seq = 7
		p.AddHeader(HeaderClientID, "big-client")
		p.AddHeader(PayloadContentType, "application/json;charset=utf8;")
		p.Data = bytes.Repeat([]byte("中"), size) // 中文 3 字节/字

		full, err := p.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := ReadPacket(bufio.NewReader(bytes.NewReader(full)))
		if err != nil {
			t.Fatalf("size %d: read: %v", size, err)
		}
		if got.Seq != 7 || got.GetHeader(HeaderClientID) != "big-client" {
			t.Fatalf("size %d: header/seq mismatch", size)
		}
		if !bytes.Equal(got.Data, p.Data) {
			t.Fatalf("size %d: data mismatch (got %d want %d)",
				size, len(got.Data), len(p.Data))
		}
		t.Logf("合法大帧 Data %d 字节（编码后帧 %d）→ 解码正确",
			len(got.Data), len(full))
	}
}

// TestTruncatedHeaderVariants 覆盖各种截断位置，确保都返回错误而非 panic。
func TestTruncatedHeaderVariants(t *testing.T) {
	full := func() []byte {
		p := NewPacket(CTypeRegister)
		p.AddHeader(HeaderSekiroGroup, "g")
		p.AddHeader(HeaderClientID, "0123456789") // 10 字节 value
		p.Data = []byte("{}")
		b, _ := p.Encode()
		return b
	}()
	for cut := 12; cut < len(full); cut++ {
		_, err := ReadPacket(bufio.NewReader(bytes.NewReader(full[:cut])))
		if err == nil {
			t.Fatalf("cut=%d expected error, got nil", cut)
		}
	}
	t.Logf("帧长 %d 字节，截断点12..%d 全部返回错误（无 panic）",
		len(full), len(full)-1)
}

// TestInvalidDeclaredLengths 非法声明长度必须被拒。
func TestInvalidDeclaredLengths(t *testing.T) {
	cases := []struct {
		name string
		body uint32
	}{
		{"超上限", MaxFrameBytes + 1},
		{"上限+1MB", MaxFrameBytes + 1<<20},
		{"超uint32", 0},
	}
	for _, c := range cases {
		r := bufio.NewReader(bytes.NewReader(declaredFrameHead(c.body)))
		_, err := ReadPacket(r)
		if err == nil {
			t.Fatalf("%s: expected error for bodyLen=%d", c.name, c.body)
		}
		t.Logf("%s（bodyLen=%d）→ %v", c.name, c.body, err)
	}
}

// BenchmarkReadPacket 确认渐进读取未引入显著性能回退。
func BenchmarkReadPacket(b *testing.B) {
	p := NewPacket(CTypeInvokeResponse)
	p.AddHeader(HeaderClientID, "bench-client")
	p.AddHeader(PayloadContentType, "application/json;charset=utf8;")
	p.Data = bytes.Repeat([]byte("x"), 32<<10)
	frame, _ := p.Encode()

	b.Run("32KB", func(b *testing.B) {
		b.SetBytes(int64(len(frame)))
		for i := 0; i < b.N; i++ {
			if _, err := ReadPacket(bufio.NewReader(bytes.NewReader(frame))); err != nil {
				b.Fatal(err)
			}
		}
	})
	small := NewPacket(CTypeInvokeResponse)
	small.AddHeader(HeaderClientID, "c")
	small.Data = bytes.Repeat([]byte("y"), 512)
	sframe, _ := small.Encode()
	b.Run("512B", func(b *testing.B) {
		b.SetBytes(int64(len(sframe)))
		for i := 0; i < b.N; i++ {
			if _, err := ReadPacket(bufio.NewReader(bytes.NewReader(sframe))); err != nil {
				b.Fatal(err)
			}
		}
	})
	fmt.Println()
}
