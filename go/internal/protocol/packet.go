package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var (
	// ErrBadMagic 帧首不是 sekiro 魔数。
	ErrBadMagic = errors.New("pkg not start with legal magic")
	// ErrFrameTooLarge 帧超过 MaxFrameBytes。
	ErrFrameTooLarge = errors.New("sekiro frame too large")
	// ErrFrameTooSmall 帧小于最小合法长度（type+seq+count 共 6 字节）。
	// 缺失该校验时，对端发 totalLen<6 的帧会使 body 索引越界 panic，
	// readLoop 无 recover，整进程被远程打挂（DoS 面）。
	ErrFrameTooSmall = errors.New("sekiro frame too small")
	// ErrTooManyHeaders header 数量超过 127。
	ErrTooManyHeaders = errors.New("too many headers")
	// ErrHeaderTooLong 单个 key/value 字节长度超过 255。
	ErrHeaderTooLong = errors.New("sekiro header key/value too long")
)

// Packet sekiro 线路包。
type Packet struct {
	Type    Type
	Seq     int32
	Headers *Headers
	Data    []byte
}

func NewPacket(t Type) *Packet {
	return &Packet{Type: t, Headers: NewHeaders()}
}

// AddHeader 链式添加 header。
func (p *Packet) AddHeader(k, v string) *Packet {
	p.Headers.Set(k, v)
	return p
}

func (p *Packet) GetHeader(k string) string { return p.Headers.Get(k) }

// bodyLen 计算除 magic+totalLen 外的字节数（type+seq+count+headers+data）。
func (p *Packet) bodyLen() (int, error) {
	n := 1 + 4 + 1
	var err error
	p.Headers.Each(func(k, v string) {
		n += 1 + len(k) + 1 + len(v)
		if len(k) > 255 || len(v) > 255 {
			err = fmt.Errorf("%w: key %q", ErrHeaderTooLong, k)
		}
	})
	if err != nil {
		return 0, err
	}
	n += len(p.Data)
	if p.Headers.Len() > 127 {
		return 0, fmt.Errorf("%w: %d", ErrTooManyHeaders, p.Headers.Len())
	}
	return n, nil
}

// Encode 编码为完整帧字节。
func (p *Packet) Encode() ([]byte, error) {
	body, err := p.bodyLen()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 12+body)
	binary.BigEndian.PutUint64(buf[0:8], Magic)
	binary.BigEndian.PutUint32(buf[8:12], uint32(body))
	pos := 12
	buf[pos] = byte(p.Type)
	pos++
	binary.BigEndian.PutUint32(buf[pos:pos+4], uint32(p.Seq))
	pos += 4
	buf[pos] = byte(p.Headers.Len())
	pos++
	p.Headers.Each(func(k, v string) {
		buf[pos] = byte(len(k))
		pos++
		pos += copy(buf[pos:], k)
		buf[pos] = byte(len(v))
		pos++
		pos += copy(buf[pos:], v)
	})
	pos += copy(buf[pos:], p.Data)
	return buf, nil
}

// 帧固定头部：type(1)+seq(4)+headerCount(1)。
const fixedHeaderLen = 1 + 4 + 1

// maxHeaderEntry 单个 header 的最大字节数：kLen(1)+key(255)+vLen(1)+value(255)。
const maxHeaderEntry = 1 + 255 + 1 + 255

// dataInitialBuf Data 渐进读取的首块大小：按实际到达量增长，
// 不按声明长度预分配（见 ReadPacket 的放大防护说明）。
const dataInitialBuf = 4 << 10

// ReadPacket 从 r 阻塞读取一个完整帧并解码。
// 半包时持续等待（等价 Java 版 markReaderIndex/resetReaderIndex 的组包语义）。
// r 应为 *bufio.Reader 以避免小读放大（见 server 包）。
//
// 放大防护（DoS）：不可按对端声明的 bodyLen 预分配缓冲区。
// 早期实现直接 make([]byte, bodyLen)，对端仅发 12 字节帧头声明
// bodyLen=64MB 即可让服务端瞬时分配 64MB——实测 200 条连接各发 12 字节
// （合计 2.4KB）即造成 12,800MB 分配，放大约 560 万:1，且连接可被
// readLoop 的 30s 读空闲长期吊住。改为「先解析头部、拿到真实 Data 长度，
// 再按实际到达量渐进读取」，使内存开销与实际收到的字节数成正比。
func ReadPacket(r io.Reader) (*Packet, error) {
	var head [12]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint64(head[0:8]) != Magic {
		return nil, ErrBadMagic
	}
	bodyLen := int64(binary.BigEndian.Uint32(head[8:12]))
	if bodyLen > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	// 下限校验：type(1)+seq(4)+headerCount(1) 共 6 字节
	if bodyLen < fixedHeaderLen {
		return nil, ErrFrameTooSmall
	}

	// 先读固定头部（精确 6 字节，不多读——多读会吞掉属于 Data 的字节）
	var fixed [fixedHeaderLen]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return nil, err
	}
	p := &Packet{Headers: NewHeaders(), Type: Type(fixed[0]),
		Seq: int32(binary.BigEndian.Uint32(fixed[1:5]))}
	count := int(fixed[5])

	// 逐个读取 header：长度是前缀编码的，必须逐段精确消费，
	// 不能一次读满（会越过 header 边界吞掉 Data 的开头字节）。
	// 实测代价可忽略：每header 4 次 ReadFull，命中 bufio 缓冲时约 20ns/次，
	// 而每个 header 至多 255 字节，网络 IO 是主要成本。
	var scratch [maxHeaderEntry]byte
	consumed := int64(fixedHeaderLen)
	for i := 0; i < count; i++ {
		if consumed+2 > bodyLen {
			return nil, fmt.Errorf("sekiro frame truncated in header %d", i)
		}
		if _, err := io.ReadFull(r, scratch[:1]); err != nil {
			return nil, truncatedErr(i, err)
		}
		consumed++
		kLen := int(scratch[0])
		if consumed+int64(kLen)+1 > bodyLen {
			return nil, fmt.Errorf("sekiro frame truncated in header key %d", i)
		}
		var key string
		if kLen > 0 {
			if _, err := io.ReadFull(r, scratch[:kLen]); err != nil {
				return nil, truncatedErr(i, err)
			}
			key = string(scratch[:kLen])
			consumed += int64(kLen)
		}
		if _, err := io.ReadFull(r, scratch[:1]); err != nil {
			return nil, truncatedErr(i, err)
		}
		consumed++
		vLen := int(scratch[0])
		if consumed+int64(vLen) > bodyLen {
			return nil, fmt.Errorf("sekiro frame truncated in header value %d", i)
		}
		var val string
		if vLen > 0 {
			if _, err := io.ReadFull(r, scratch[:vLen]); err != nil {
				return nil, truncatedErr(i, err)
			}
			val = string(scratch[:vLen])
			consumed += int64(vLen)
		}
		p.Headers.Set(key, val)
	}

	// Data 长度此时才是可信的（bodyLen - 已消费的头部）。
	// 渐进读取：按实际到达量增长，不做bodyLen 大小的预分配。
	dataLen := bodyLen - consumed
	data, err := readData(r, dataLen)
	if err != nil {
		return nil, err
	}
	p.Data = data
	return p, nil
}

// readData 渐进读取 n 字节：首块 dataInitialBuf，之后由 bytes.Buffer
// 按实际写入量增长（避免按 n 预分配）。n==0 时返回 nil。
func readData(r io.Reader, n int64) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if n < dataInitialBuf {
		buf.Grow(int(n))
	} else {
		buf.Grow(dataInitialBuf)
	}
	got, err := buf.ReadFrom(io.LimitReader(r, n))
	if err != nil {
		return nil, err
	}
	if got < n {
		// 对端声明 bodyLen 但提前断开：与 io.ReadFull 语义对齐
		return nil, io.ErrUnexpectedEOF
	}
	return buf.Bytes(), nil
}

// truncatedErr 头部区域读取中断：统一归一为 ErrUnexpectedEOF，
// 便于上层区分「连接断」与「帧格式非法」。
func truncatedErr(i int, err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return fmt.Errorf("sekiro frame truncated in header %d: %w", i, io.ErrUnexpectedEOF)
	}
	return err
}

// WritePacket 将包编码并写入 w。
func WritePacket(w io.Writer, p *Packet) error {
	b, err := p.Encode()
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
