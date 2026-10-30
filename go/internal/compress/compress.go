// Package compress 实现 sekiro 线上协议的压缩协商层。
//
// 语义（对齐 CompressorManager 字节码逆向结论）：
//   - 仅对响应类包（C_TYPE_INVOKE_RESPONSE / S_TYPE_INVOKE_RESPONSE）生效；
//   - 由包 header COMPRESS_METHOD 驱动：空 → 不压缩；
//     未知方法 → 报错（Java 抛 IllegalStateException）；
//   - header __NOT_COMPRESS_FOR_SEKIRO_SEKIRO 存在时跳过（由调用方判断，见 NettyCompressHandler 对齐逻辑）；
//   - 解压后 COMPRESS_METHOD 头保留（转发时按头重压缩，Java 行为一致）。
package compress

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/golang/snappy"

	"sekiro-go/internal/protocol"
)

// Compressor 与 Java Compressor 接口对齐。
type Compressor interface {
	Method() string
	Compress([]byte) ([]byte, error)
	Decompress([]byte) ([]byte, error)
}

// MaxDecompressedBytes 单次解压的字节上限（zip bomb 防护，第一道）。
//
// 为什么需要：解压是压缩的逆运算，放大比可达数百倍（实测 gzip 510x、
// snappy 21x）。帧上限 protocol.MaxFrameBytes 为 64MB，一个 64MB 的
// 压缩响应包理论可膨胀到数十 GB，把服务端 OOM 掉。原实现用 io.ReadAll
// 无上限读取，provider 客户端（注册即被信任）返回一枚压缩炸弹即可
// 远程触发。
//
// 取 128MB 的考量：常规业务响应远小于此（HTTP 聚合器上限 32MB），
// 留 4倍余量避免误伤合法大响应；超限则返回明确错误而非静默截断
// （静默截断产生数据不一致，比报错更难排查）。
const MaxDecompressedBytes = 128 << 20

// maxInFlight 全进程并发解压的内存预算（zip bomb 防护，第二道）。
//
// 为什么单流上限不够：MaxDecompressedBytes 只约束「一次」解压。N 个
// provider 连接同时返回压缩包时，总占用可达 N × 128MB——单流合规，
// 总体仍可打挂进程（实测 50 并发 × 64MB 数据落地即 HeapSys +2047MB）。
// 预算耗尽时立即拒绝新解压请求，把总体占用钉在常数级。
//
// 512MB 的取值：显著高于任何真实业务峰值（单实例并发解压通常 <64MB），
// 又远低于常见的 2~4GB 容器 limit，不会误伤正常负载。
var maxInFlightBytes int64 = 512 << 20

// maxStreamBytes 单流上限的运行时副本（var 以便测试注入小值；
// 生产恒为 MaxDecompressedBytes）。
var maxStreamBytes int64 = MaxDecompressedBytes

// inFlight 当前并发解压占用的字节数（原子增减）。
var inFlight atomic.Int64

// ErrNoBudget 并发解压预算耗尽（zip bomb 防护第二道触发）。
var ErrNoBudget = errors.New("concurrent decompression budget exhausted")

// ErrTooLarge 解压后超出 MaxDecompressedBytes。
var ErrTooLarge = errors.New("decompressed payload too large")

// reserveDecompress 占用解压预算；超限返回 ErrNoBudget。
func reserveDecompress(n int64) error {
	if n > maxInFlightBytes {
		return fmt.Errorf("%w: single request needs %d bytes", ErrTooLarge, n)
	}
	for {
		cur := inFlight.Load()
		if cur+n > maxInFlightBytes {
			return fmt.Errorf("%w: in-flight %d MB + %d MB > %d MB",
				ErrNoBudget, cur>>20, n>>20, maxInFlightBytes>>20)
		}
		if inFlight.CompareAndSwap(cur, cur+n) {
			return nil
		}
	}
}

func releaseDecompress(n int64) { inFlight.Add(-n) }

// Gzip 对齐 GzipCompressor。写入 OS=0 字节以对齐 Java GZIPOutputStream 头
// （MTIME=0/XFL=0/OS=0）；解压方向与任何标准 gzip 流互通。
type Gzip struct{}

func (Gzip) Method() string { return protocol.CompressGzip }

func (Gzip) Compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	if err != nil {
		return nil, err
	}
	w.Header.OS = 0 // Java GZIPOutputStream 写 OS=0
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decompress 解压并同时施加单流上限与全局预算。
//
// 逐chunk 读取而非 io.ReadAll / io.LimitReader：后者在到达上限前仍要
// 把内容实际读进内存（实测拒绝一枚 200MB 炸弹仍先分配 512MB），
// 恰恰是本防护要消除的放大路径。这里每读入一块就检查上限并预占预算，
// 超限立即返回，已读部分随之回收。
func (Gzip) Decompress(data []byte) ([]byte, error) {
	// 第一道：ISIZE 预检（O(1)，不解压任何字节）。
	// gzip trailer 末 4 字节即解压后长度，而压缩数据此时已完整在内存中，
	// 据此可在解压前判定炸弹。实测一枚 402KB 炸弹：只靠流式检查时服务端
	// 要先读出 128MB（上限）才拒绝，累计分配高达 783MB；加预检后为 0。
	//
	// mod 2^32 回绕无害：真实值 ≥4GB 时 ISIZE 回绕后仍远大于上限，照样被拒；
	// 回绕到上限以下的极端情形由下面的流式检查兜底。
	// ISIZE 只能用于「拒绝」，不能用于预分配——它可被伪造。
	if n := len(data); n >= 4 {
		isize := int64(binary.LittleEndian.Uint32(data[n-4:]))
		if isize > MaxDecompressedBytes {
			return nil, tooLargeErr("gzip", len(data), MaxDecompressedBytes)
		}
	}

	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// 第二道：流式兜底。ISIZE 可伪造（且可回绕），真实解压量仍须逐块校验。
	out, n, err := readChunked(r)
	if err != nil {
		return nil, err
	}
	if n > maxStreamBytes {
		return nil, tooLargeErr("gzip", len(data), maxStreamBytes)
	}
	return out, nil
}

// decompressChunk 单次读取块大小。取 64KB 与 server 的 bufio 缓冲一致。
const decompressChunk = 64 << 10

// readChunked 逐块读取解压流。每块到达即：
//  1. 检查单流上限（超过立即返回，已读内存随函数退出被 GC 回收）
//  2. 向全局预算预占（CAS 循环），超预算立即返回
//
// 首块固定按 decompressChunk 预分配，绝不使用 gzip ISIZE 作为 capHint：
// ISIZE 来自不可信输入，伪造“ISIZE=128MB、实际只有几字节”即可在预算
// 占位前制造巨额分配。合法大响应依靠 append 倍增扩容，开销可接受。
func readChunked(r io.Reader) ([]byte, int64, error) {
	buf := make([]byte, 0, decompressChunk)
	chunk := make([]byte, decompressChunk)
	var reserved int64
	defer func() { releaseDecompress(reserved) }()

	for {
		n, err := r.Read(chunk)
		if n > 0 {
			total := int64(len(buf)) + int64(n)
			if total > maxStreamBytes {
				return nil, total, nil // 由调用方生成错误信息
			}
			if rerr := reserveDecompress(int64(n)); rerr != nil {
				return nil, total, rerr
			}
			reserved += int64(n)
			buf = append(buf, chunk[:n]...)
		}
		if err == io.EOF {
			return buf, int64(len(buf)), nil
		}
		if err != nil {
			return nil, int64(len(buf)), err
		}
	}
}

func tooLargeErr(method string, compressed int, limit int64) error {
	return fmt.Errorf("%w: %s stream of %d compressed bytes expands beyond %d bytes",
		ErrTooLarge, method, compressed, limit)
}

// Snappy 对齐 SnappyCompressor：snappy-java 单包 raw block 格式，
// 与 github.com/golang/snappy Encode/Decode 兼容。
type Snappy struct{}

func (Snappy) Method() string { return protocol.CompressSnappy }

func (Snappy) Compress(data []byte) ([]byte, error) { return snappy.Encode(nil, data), nil }

// Decompress 先校验 raw block 头声明的长度，再在预算内解码。
//
// snappy 的 block 格式前若干字节即为解压后长度（varint），比 gzip 更早
// 暴露意图：可在分配任何内存之前就判定炸弹。实测 3.1MB 压缩块声明
// 64MB（21x）。校验后仍走全局预算——单流合规不代表并发安全。
func (Snappy) Decompress(data []byte) ([]byte, error) {
	if dLen, ok := snappyDecodedLen(data); ok {
		if dLen > MaxDecompressedBytes {
			return nil, tooLargeErr("snappy", len(data), MaxDecompressedBytes)
		}
		// 长度已知且合规：按声明值预占预算，避免解码中途才发现超限
		if err := reserveDecompress(int64(dLen)); err != nil {
			return nil, err
		}
		defer releaseDecompress(int64(dLen))
	}
	out, err := snappy.Decode(nil, data)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxDecompressedBytes {
		return nil, tooLargeErr("snappy", len(data), MaxDecompressedBytes)
	}
	return out, nil
}

// snappyDecodedLen 读取 raw block 头声明的解压后长度，不做解码。
func snappyDecodedLen(data []byte) (uint64, bool) {
	v, _, ok := snappyDecodedLenPartial(data)
	return v, ok
}

func snappyDecodedLenPartial(data []byte) (uint64, int, bool) {
	var v uint64
	for i := 0; i < 4 && i < len(data); i++ {
		b := data[i]
		v |= uint64(b&0x7f) << (7 * uint(i))
		if b < 0x80 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

// None 对齐 NoneCompressor：原样透传。
type None struct{}

func (None) Method() string                         { return protocol.CompressNone }
func (None) Compress(data []byte) ([]byte, error)   { return data, nil }
func (None) Decompress(data []byte) ([]byte, error) { return data, nil }

// Manager 压缩器注册表，对齐 CompressorManager。
type Manager struct {
	compressors map[string]Compressor
}

func NewManager() *Manager {
	m := &Manager{compressors: map[string]Compressor{}}
	for _, c := range []Compressor{Gzip{}, None{}, Snappy{}} {
		m.Register(c)
	}
	return m
}

func (m *Manager) Register(c Compressor) {
	m.compressors[c.Method()] = c
}

// fetch 对齐 fetchCompressor：
//   - 非响应类包 → nil, nil（跳过）
//   - COMPRESS_METHOD 头为空 → nil, nil（跳过）
//   - 未注册的方法 → 错误（Java IllegalStateException）
func (m *Manager) fetch(p *protocol.Packet) (Compressor, error) {
	if !p.Type.IsInvokeResponse() {
		return nil, nil
	}
	method := p.GetHeader(protocol.CompressMethod)
	if method == "" {
		return nil, nil
	}
	c, ok := m.compressors[method]
	if !ok {
		return nil, fmt.Errorf("can not decompress sekiro package with compress method: %s", method)
	}
	return c, nil
}

// DecompressPacket 对齐 handleDecompress：原地解压 payload，头保留。
func (m *Manager) DecompressPacket(p *protocol.Packet) error {
	c, err := m.fetch(p)
	if err != nil || c == nil {
		return err
	}
	out, err := c.Decompress(p.Data)
	if err != nil {
		return fmt.Errorf("sekiro decompress with %s: %w", c.Method(), err)
	}
	p.Data = out
	return nil
}

// CompressPacket 对齐 doCompress：原地压缩 payload（仅在头驱动命中时）。
func (m *Manager) CompressPacket(p *protocol.Packet) error {
	c, err := m.fetch(p)
	if err != nil || c == nil {
		return err
	}
	out, err := c.Compress(p.Data)
	if err != nil {
		return fmt.Errorf("sekiro compress with %s: %w", c.Method(), err)
	}
	p.Data = out
	return nil
}

// ShouldSkip 对齐 NettyCompressHandler 的跳过头判断。
func ShouldSkip(p *protocol.Packet) bool {
	return p.Headers.Has(protocol.DoNotCompress)
}
