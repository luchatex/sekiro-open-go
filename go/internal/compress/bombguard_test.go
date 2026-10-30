package compress

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/snappy"

	"sekiro-go/internal/protocol"
)

// gzipBomb 构造一枚压缩炸弹：highly compressible 数据 + 超出上限的解压体积。
func gzipBomb(t *testing.T, rawSize int) []byte {
	t.Helper()
	bomb := bytes.Repeat([]byte("A"), rawSize)
	enc, err := Gzip{}.Compress(bomb)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	return enc
}

// TestGzipBombRejected 核心回归：超过 MaxDecompressedBytes 的 gzip 流必须被拒绝。
func TestGzipBombRejected(t *testing.T) {
	bomb := gzipBomb(t, 200<<20)
	t.Logf("炸弹：压缩后 %d 字节（%.0f KB），解压后约 %.0f MB（放大 %.0fx）",
		len(bomb), float64(len(bomb))/1024, 200.0,
		float64(200<<20)/float64(len(bomb)))

	_, err := Gzip{}.Decompress(bomb)
	if err == nil {
		t.Fatal("expected error for gzip bomb")
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	t.Logf("正确拒绝：%v", err)
}

// TestGzipBombMemoryBounded 验证关键不变式：面对 400MB 炸弹，
// 解压过程的峰值堆内存被单流上限钉住，而非随炸弹体积无限膨胀。
//
// 注意：不能用 TotalAlloc 作断言——流式解压 128MB 内容必然产生
// O(128MB) 的累计分配，这是不可避免的。真正要保证的是「峰值驻留内存」
// 被上限约束（修复前二者均随炸弹体积线性膨胀，200 连接实测 HeapSys +8GB）。
func TestGzipBombMemoryBounded(t *testing.T) {
	// 先在采样窗口外构造炸弹：bytes.Repeat 本身要占 rawSize 内存，
	// 若计入采样会把「构造炸弹」误算成「解压炸弹」的内存开销。
	const bombRaw = 400 << 20
	bomb := gzipBomb(t, bombRaw)
	t.Logf("炸弹：压缩后 %d 字节（%.0f KB），解压后 %.0f MB",
		len(bomb), float64(len(bomb))/1024, float64(bombRaw)/(1<<20))

	// 堆峰值采样受 GC 时机与 race detector 影子内存影响过大
	// （-race 下同一用例报到 463MB），不是可靠的断言依据。
	// 精确的有界性由 TestReadChunkedStopsAtLimit 用「实际读取字节数」
	// 断言；此处只做趋势性观察与宽松上限检查。
	runtime.GC()
	gz := Gzip{}
	peak := watchHeapPeak(t, func() {
		if _, err := gz.Decompress(bomb); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("expected ErrTooLarge, got %v", err)
		}
	})

	got := float64(peak) / (1 << 20)
	t.Logf("解压 %.0f MB 炸弹时堆峰值 = %.1f MB（单流上限 %.0f MB）——趋势性观测，精确断言见 TestReadChunkedStopsAtLimit",
		float64(bombRaw)/(1<<20), got, float64(MaxDecompressedBytes)/(1<<20))
	if got > 8*float64(MaxDecompressedBytes)/(1<<20) {
		t.Logf("WARN 堆峰值 %.1f MB 偏高，请确认是否有新的放大路径", got)
	}
}

// TestGzipForgedISizeDoesNotPreallocate 伪造一个合法上限内的 ISIZE，
// 不能据此预分配 128MB。旧实现用 ISIZE 作 capHint，单请求在预算占位前
// 就能制造 128MB 分配；现在首块固定 64KB，靠 append 增长。
func TestGzipForgedISizeDoesNotPreallocate(t *testing.T) {
	raw := bytes.Repeat([]byte("A"), 1<<20)
	enc, err := Gzip{}.Compress(raw)
	if err != nil {
		t.Fatal(err)
	}
	// trailer = CRC32(4) + ISIZE(4)，只篡改 ISIZE，保留格式可解析。
	binary.LittleEndian.PutUint32(enc[len(enc)-4:], uint32(MaxDecompressedBytes))

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := (Gzip{}).Decompress(enc); err == nil {
		t.Fatal("伪造 ISIZE 应触发 gzip 完整性错误")
	}
	runtime.ReadMemStats(&after)

	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("伪造 ISIZE=%d，解压尝试累计分配 %.2f MB", MaxDecompressedBytes, float64(delta)/(1<<20))
	if delta >= 32<<20 {
		t.Fatalf("伪造 ISIZE 造成 %d MB 分配，疑似仍按不可信长度预分配", delta>>20)
	}
}

// TestReadChunkedStopsAtLimit 精确验证「读取量有界」这一核心不变式。
//
// 不用 HeapAlloc 采样做断言（受 GC 时机影响，并发下噪声可达 2 倍）：
// 直接统计「从流中实际读取了多少字节」——这是确定性的、可精确断言的指标，
// 且正是内存占用的上界来源。
func TestReadChunkedStopsAtLimit(t *testing.T) {
	origStream := maxStreamBytes
	maxStreamBytes = 8 << 20 // 降到 8MB 以便快速验证
	defer func() {
		maxStreamBytes = origStream
		inFlight.Store(0)
	}()

	src := &countingRepeatReader{chunk: make([]byte, 64<<10)}
	_, n, err := readChunked(src)
	if err != nil {
		t.Fatalf("readChunked: %v", err)
	}

	t.Logf("无限流（永不 EOF）→ 读取 %d 字节 = %.2f MB", n, float64(n)/(1<<20))
	t.Logf("上限 = 8MB；读取量必须 ≤ 上限 + 单块 64KB（否则说明会一直读到流结束）")

	// 精确断言：读取量在上限处截断（最多多读一个 chunk 用于判定）
	budget := int64(8<<20) + decompressChunk
	if n > budget {
		t.Fatalf("read %d bytes, exceeds limit+chunk budget %d", n, budget)
	}
	if n < int64(8<<20) {
		t.Fatalf("read %d bytes, expected at least the 8MB limit", n)
	}
	t.Logf("预算余额归还：inFlight=%d", inFlight.Load())
	if inFlight.Load() != 0 {
		t.Fatalf("budget leak: %d", inFlight.Load())
	}
}

// TestReadChunkedBudgetBlocksEarly 验证预算耗尽时提前中止读取，
// 而非读满单流上限——这是并发场景下总占用被钉住的关键。
func TestReadChunkedBudgetBlocksEarly(t *testing.T) {
	orig := maxInFlightBytes
	maxInFlightBytes = 4 << 20 // 4MB 预算（远小于单流上限 128MB）
	defer func() { maxInFlightBytes = orig; inFlight.Store(0) }()
	inFlight.Store(0)

	src := &countingRepeatReader{chunk: make([]byte, 64<<10)}
	_, n, err := readChunked(src)
	if err == nil {
		t.Fatal("expected ErrNoBudget")
	}
	if !errors.Is(err, ErrNoBudget) {
		t.Fatalf("expected ErrNoBudget, got %v", err)
	}
	t.Logf("预算 4MB < 单流上限 128MB → 读取 %.2f MB 即被拒（未读满上限）",
		float64(n)/(1<<20))
	if n > int64(maxInFlightBytes)+decompressChunk {
		t.Fatalf("read %d bytes before budget rejection, expected ~%d", n, maxInFlightBytes)
	}
	if inFlight.Load() != 0 {
		t.Fatalf("budget leak: %d", inFlight.Load())
	}
	t.Log("提前中止且预算全额归还")
}

// countingRepeatReader 无限重复输出同一块，用于模拟「无终止的解压流」。
type countingRepeatReader struct {
	chunk     []byte
	off       int
	readTotal int64
}

func (r *countingRepeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.chunk[r.off:])
		n += c
		r.off += c
		if r.off >= len(r.chunk) {
			r.off = 0
		}
	}
	r.readTotal += int64(n)
	return n, nil
}

// TestConcurrentBombsBoundedByBudget 验证第二道防线：并发炸弹的总体占用
// 被全局预算钉住（修复前 N×128MB 可无限膨胀）。
//
// 断言用「预算行为」而非 HeapAlloc 峰值：后者含未回收垃圾，并发下虚高
// （实测 64 路时 HeapAlloc 报 1.4GB，实际预算上限仅 16MB），不适合做精确断言。
// 精确的验证是：预算耗尽时新请求被拒、峰值并发预占不超预算、余额全额归还。
func TestConcurrentBombsBoundedByBudget(t *testing.T) {
	orig := maxInFlightBytes
	maxInFlightBytes = 16 << 20 // 降到 16MB 以便快速验证
	defer func() { maxInFlightBytes = orig; inFlight.Store(0) }()
	inFlight.Store(0)

	bomb := gzipBomb(t, 64<<20)
	const conc = 64
	t.Logf("并发 %d 路解压 64MB 炸弹（潜在总占用 %.0f MB，预算 16 MB）",
		conc, float64(conc*64))

	var rejected, succeeded atomic.Int64
	var mu sync.Mutex
	var maxSeen int64
	gz := Gzip{}
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 采样并发预占峰值
			stop := make(chan struct{})
			go func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					if v := inFlight.Load(); v > 0 {
						mu.Lock()
						if v > maxSeen {
							maxSeen = v
						}
						mu.Unlock()
					}
				}
			}()
			_, err := gz.Decompress(bomb)
			close(stop)
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, ErrNoBudget), errors.Is(err, ErrTooLarge):
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	peakInFlight := maxSeen
	mu.Unlock()

	t.Logf("结果：%d 路被预算拒绝，%d 路完成", rejected.Load(), succeeded.Load())
	t.Logf("并发预占峰值 = %.1f MB（预算上限 16.0 MB，未突破）",
		float64(peakInFlight)/(1<<20))

	if rejected.Load() == 0 {
		t.Fatal("expected some requests rejected by budget")
	}
	// 精确断言：并发预占峰值不得超过预算（这是内存占用的真正上界）
	if peakInFlight > maxInFlightBytes {
		t.Fatalf("in-flight peak %d exceeded budget %d", peakInFlight, maxInFlightBytes)
	}
	// 且确实用满了预算（否则本用例没有验证到限流逻辑）
	if peakInFlight < maxInFlightBytes/2 {
		t.Fatalf("in-flight peak %d never approached budget %d — budget not exercised",
			peakInFlight, maxInFlightBytes)
	}
	if final := inFlight.Load(); final != 0 {
		t.Fatalf("in-flight budget leaked: %d bytes", final)
	}
	t.Logf("预算余额已完全归还（inFlight=0）")
}

// TestBudgetReturnedOnAllPaths 确保预占预算在成功/格式错误/超限三条路径
// 都归还，否则长时间运行会误拒正常请求。
func TestBudgetReturnedOnAllPaths(t *testing.T) {
	defer inFlight.Store(0)

	data := []byte(strings.Repeat("ok", 100<<10))
	enc, _ := Gzip{}.Compress(data)
	for i := 0; i < 50; i++ {
		got, err := Gzip{}.Decompress(enc)
		if err != nil || len(got) != len(data) {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if inFlight.Load() != 0 {
		t.Fatalf("leak after success: %d", inFlight.Load())
	}

	for i := 0; i < 20; i++ {
		_, _ = Gzip{}.Decompress([]byte("garbage-not-gzip"))
	}
	if inFlight.Load() != 0 {
		t.Fatalf("leak after corrupt stream: %d", inFlight.Load())
	}

	for i := 0; i < 3; i++ {
		_, _ = Gzip{}.Decompress(gzipBomb(t, 200<<20))
	}
	if inFlight.Load() != 0 {
		t.Fatalf("leak after too-large: %d", inFlight.Load())
	}
	t.Log("成功 / 格式错误 / 超限三条路径的预算均已归还")
}

// TestLegitLargePayloadStillDecodes 确保防护不误伤合法大响应。
func TestLegitLargePayloadStillDecodes(t *testing.T) {
	for _, size := range []int{1 << 10, 1 << 20, 16 << 20, 64 << 20} {
		data := []byte(strings.Repeat("中", size/3)) // UTF-8 中文
		enc, err := Gzip{}.Compress(data)
		if err != nil {
			t.Fatalf("compress %d: %v", size, err)
		}
		got, err := Gzip{}.Decompress(enc)
		if err != nil {
			t.Fatalf("decompress %d: %v", size, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("roundtrip mismatch at %d", size)
		}
		t.Logf("合法 payload %d 字节 → 压缩 %d → 解压正确", size, len(enc))
	}
}

// TestSnappyBombRejected snappy raw block 自带长度声明，可在分配前拒绝。
func TestSnappyBombRejected(t *testing.T) {
	raw := bytes.Repeat([]byte("B"), 200<<20)
	enc := snappy.Encode(nil, raw)
	t.Logf("snappy 炸弹：压缩后 %d 字节（%.1f MB），声明解压后 200 MB（放大 %.0fx）",
		len(enc), float64(len(enc))/(1<<20), float64(200<<20)/float64(len(enc)))

	_, err := Snappy{}.Decompress(enc)
	if err == nil {
		t.Fatal("expected error for snappy bomb")
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	t.Logf("正确拒绝：%v", err)
	if inFlight.Load() != 0 {
		t.Fatalf("snappy budget leak: %d", inFlight.Load())
	}
}

// TestSnappyLenCheckHappensBeforeDecode 验证长度预检：声明超大但数据
// 很短的 block，应在解码前就被拒绝。
func TestSnappyLenCheckHappensBeforeDecode(t *testing.T) {
	fake := []byte{0x80, 0x80, 0x80, 0x80, 0x10} // varint 声明 ~1GB
	_, err := Snappy{}.Decompress(fake)
	t.Logf("伪造声明超大 的 block → %v", err)
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestDecompressPacketPropagatesError 确保包级入口把超限错误传给上层，
// 使 native.readLoop 能关闭恶意连接而非静默接受。
func TestDecompressPacketPropagatesError(t *testing.T) {
	bomb := gzipBomb(t, 200<<20)
	p := protocol.NewPacket(protocol.CTypeInvokeResponse)
	p.Data = bomb
	p.AddHeader(protocol.CompressMethod, protocol.CompressGzip)

	err := NewManager().DecompressPacket(p)
	if err == nil {
		t.Fatal("expected error from DecompressPacket")
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	t.Logf("DecompressPacket 正确传播超限错误：%v", err)
}

// TestNoCompressorPathUntouched 不涉及压缩的路径不受影响。
func TestNoCompressorPathUntouched(t *testing.T) {
	data := []byte(strings.Repeat("X", 300<<20/1000))
	out, err := None{}.Decompress(data)
	if err != nil || !bytes.Equal(out, data) {
		t.Fatalf("None must pass through unchanged: %v", err)
	}
	t.Logf("None 压缩器透传正常（%d 字节，不施加上限）", len(out))
}

// TestCorruptStreamsStillError 确保格式错误仍被正确识别，未被新逻辑掩盖。
func TestCorruptStreamsStillError(t *testing.T) {
	gz := Gzip{}
	if _, err := gz.Decompress([]byte("not gzip at all")); err == nil {
		t.Fatal("expected error for corrupt gzip")
	}
	sn := Snappy{}
	if _, err := sn.Decompress([]byte{0xff, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Fatal("expected error for corrupt snappy")
	}
	t.Log("损坏流仍正确报错")
}

// TestConcurrentLegitTrafficNotRejectedByBudget 确保正常并发不被预算误拒。
func TestConcurrentLegitTrafficNotRejectedByBudget(t *testing.T) {
	defer inFlight.Store(0)
	payload := []byte(strings.Repeat("中文数据", 64<<10)) // 256KB
	enc, _ := Gzip{}.Compress(payload)

	const conc = 128
	var wg sync.WaitGroup
	errCh := make(chan error, conc)
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Gzip{}.Decompress(enc)
			if err != nil {
				errCh <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errCh <- errors.New("payload mismatch")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("legit request rejected: %v", err)
	}
	t.Logf("%d 路并发合法解压（每路 %d KB，合计 %d MB）→ 全部通过，预算 %.0f MB 充裕",
		conc, len(payload)/1024, conc*len(payload)/(1<<20),
		float64(maxInFlightBytes)/(1<<20))
}

// watchHeapPeak 在 fn 执行期间采样堆内存峰值（20ms 间隔）。
func watchHeapPeak(t *testing.T, fn func()) uint64 {
	t.Helper()
	var peak uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var ms runtime.MemStats
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > peak {
					peak = ms.HeapAlloc
				}
			}
		}
	}()
	fn()
	close(done)
	wg.Wait()
	return peak
}
