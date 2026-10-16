// Package native 实现 sekiro 二进制协议通道：
// provider client 注册（C_TYPE_REGISTER）与 invoker 连接（I_TYPE_CONNECT）。
// 对齐 RouterSekiro / ServiceSekiroClient / ServiceSekiroInvoker /
// ServerHeartBeatHandler（读空闲 30s 关闭、写空闲 20s 发心跳）。
package native

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/metrics"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

type Channel struct {
	conn      net.Conn
	br        *bufio.Reader
	mgr       *group.Manager
	comp      *compress.Manager
	log       *slog.Logger
	client    *group.Client // 注册后非 nil
	invoker   bool
	lastRead  atomic.Int64
	lastWrite atomic.Int64
	writeMu   sync.Mutex // 并发写保护：多个 HTTP handler 同时向同一
	// provider 下发 S_TYPE_INVOKE，与心跳线程并发——TCP 流式帧必须串行化，
	// 否则帧交错损坏（压测实证：偶发响应丢失 → 15s 超时）
	closeOnce sync.Once
	stopHB    chan struct{}
}

// ServeChannel 服务一条二进制协议连接（阻塞直到连接结束）。
func ServeChannel(conn net.Conn, br *bufio.Reader, mgr *group.Manager, comp *compress.Manager, log *slog.Logger) {
	ch := &Channel{
		conn:   conn,
		br:     br,
		mgr:    mgr,
		comp:   comp,
		log:    log.With("peer", conn.RemoteAddr().String()),
		stopHB: make(chan struct{}),
	}
	defer ch.close()
	ch.lastRead.Store(time.Now().UnixNano())
	ch.lastWrite.Store(time.Now().UnixNano())
	go ch.heartbeatLoop()
	ch.readLoop()
}

func (c *Channel) close() {
	c.closeOnce.Do(func() {
		close(c.stopHB)
		if c.client != nil {
			c.client.Disconnect()
		}
		c.conn.Close()
	})
}

// heartbeatLoop 对齐 IdleStateHandler：写空闲 20s 发 TYPE_HEARTBEAT。
func (c *Channel) heartbeatLoop() {
	t := time.NewTicker(protocol.ServerWriteIdle())
	defer t.Stop()
	for {
		select {
		case <-c.stopHB:
			return
		case <-t.C:
			if time.Since(time.Unix(0, c.lastWrite.Load())) < protocol.ServerWriteIdle() {
				continue
			}
			hb := protocol.NewPacket(protocol.TypeHeartbeat)
			if err := c.writePacket(hb); err != nil {
				return
			}
			c.log.Debug("server write idle, send heartbeat")
		}
	}
}

func (c *Channel) writePacket(p *protocol.Packet) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.lastWrite.Store(time.Now().UnixNano())
	c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err := protocol.WritePacket(c.conn, p)
	c.conn.SetWriteDeadline(time.Time{})
	return err
}

func (c *Channel) readLoop() {
	registered := false
	for {
		// 读空闲 30s：超时关闭（对齐 READER_IDLE）
		c.conn.SetReadDeadline(time.Now().Add(protocol.ServerReadIdle()))
		p, err := protocol.ReadPacket(c.br)
		if err != nil {
			c.log.Debug("binary channel closed", "err", err)
			return
		}
		c.lastRead.Store(time.Now().UnixNano())

		switch p.Type {
		case protocol.TypeHeartbeat:
			continue
		case protocol.CTypeRegister:
			if registered {
				c.log.Warn("duplicate register on same channel")
				return
			}
			if !c.handleRegister(p) {
				return
			}
			registered = true
		case protocol.ITypeConnect:
			if registered {
				return
			}
			if err := c.writePacket(protocol.NewPacket(protocol.STypeInvokeConnected)); err != nil {
				return
			}
			c.invoker = true
			registered = true
		default:
			if !registered {
				c.log.Warn("packet before register", "type", p.Type.String())
				return
			}
			if c.invoker {
				if p.Type != protocol.ITypeInvoke {
					c.log.Warn("invoker receive error packet", "type", p.Type.String())
					return
				}
				c.handleInvokerInvoke(p)
				continue
			}
			switch p.Type {
			case protocol.CTypeInvokeResponse:
				// 解压（响应类包按 COMPRESS_METHOD 头，对齐 NettyDecompressHandler）
				if err := c.comp.DecompressPacket(p); err != nil {
					c.log.Warn("decompress failed", "err", err)
					return
				}
				c.client.HandleResponse(p)
			case protocol.CTypeOffline:
				c.client.MarkOffline()
			default:
				c.log.Warn("client receive unknown packet", "type", p.Type.String())
			}
		}
	}
}

// handleRegister 对齐 RouterSekiro.handleClientRegister。
func (c *Channel) handleRegister(p *protocol.Packet) bool {
	gName := p.GetHeader(protocol.HeaderSekiroGroup)
	clientID := p.GetHeader(protocol.HeaderClientID)
	if gName == "" || clientID == "" {
		c.log.Warn("group or clientId header is empty when register sekiro client")
		return false
	}
	tr := &nativeTransport{ch: c}
	g := c.mgr.GetOrCreate(gName)
	c.client = group.NewClient(tr, g, clientID, c.log)
	g.Register(c.client)
	c.log.Info("client registered", "group", gName, "clientId", clientID)
	return true
}

// handleInvokerInvoke 对齐 ServiceSekiroInvoker：
// 解析 JSON 参数 → 打标不压缩 → 分配 client → 转发（seq 映射回 invokerSeq）。
func (c *Channel) handleInvokerInvoke(p *protocol.Packet) {
	data := p.Data
	if len(data) == 0 {
		c.log.Info("empty invoker request")
		c.closeConn()
		return
	}
	if p.Seq < 0 {
		c.log.Info("empty invoker seq number")
		c.closeConn()
		return
	}
	params, err := orderedjson.Parse(data)
	if err != nil {
		c.log.Warn("error parse request json", "err", err)
		// CommonRes.failed + setSeq(invokerSeq)
		res := commonResJSON(-1, "error to parse request", "", p.Seq)
		pkt := protocol.NewPacket(protocol.STypeInvokeResponse)
		pkt.Seq = p.Seq
		pkt.Data = res
		pkt.AddHeader(protocol.HTTPContentType, protocol.HTTPContentTypeJSON)
		c.writePacket(pkt)
		return
	}
	params.Set(protocol.DoNotCompress, "true")
	sink := &invokerSink{ch: c, closed: make(chan struct{})}
	rec := group.NewRecord(params, sink, p.Seq, protocol.DefaultInvokeTimeout)
	// 监控埋点：二进制 invoker 入口记总量与请求流量
	rec.Meter = metrics.Default()
	rec.Meter.Invoke(
		params.GetString(protocol.ParamGroup),
		params.GetString(protocol.ParamAction),
		int64(len(data)),
	)
	client, aerr := c.mgr.Allocate(
		params.GetString(protocol.ParamGroup),
		params.GetString(protocol.ParamBindClient),
		params.GetString(protocol.ParamConstantInvoke),
	)
	if aerr != nil {
		rec.Fail(aerr.Error(), "")
		return
	}
	client.ForwardInvoke(rec)
}

func (c *Channel) closeConn() { c.close() }

// commonResJSON 生成 CommonRes JSON（fastjson 默认省略 null、字段名字典序）。
func commonResJSON(status int, message, clientID string, seq int32) []byte {
	b := []byte{'{'}
	if clientID != "" {
		b = append(b, `"clientId":`...)
		b = appendJSONString(b, clientID)
		b = append(b, ',')
	}
	if message != "" {
		b = append(b, `"message":`...)
		b = appendJSONString(b, message)
		b = append(b, ',')
	}
	b = append(b, `"seq":`...)
	b = appendInt(b, int64(seq))
	b = append(b, `,"status":`...)
	b = appendInt(b, int64(status))
	return append(b, '}')
}

func appendJSONString(b []byte, s string) []byte {
	q, _ := json.Marshal(s)
	return append(b, q...)
}

func appendInt(b []byte, n int64) []byte {
	return strconv.AppendInt(b, n, 10)
}

// nativeTransport native 二进制通道的 provider 侧传输。
type nativeTransport struct{ ch *Channel }

func (t *nativeTransport) SendInvoke(params *orderedjson.Map, seq int32) error {
	pkt := protocol.NewPacket(protocol.STypeInvoke)
	pkt.Seq = seq
	data, err := params.MarshalJSONSorted()
	if err != nil {
		return err
	}
	pkt.Data = data
	return t.ch.writePacket(pkt)
}

func (t *nativeTransport) Active() bool { return t.ch.connActive() }

func (t *nativeTransport) Close() { t.ch.closeConn() }

func (t *nativeTransport) CloseAfter(d time.Duration) {
	time.AfterFunc(d, t.ch.closeConn)
}

// 监控节点信息采集（可选接口，见 group/clientinfo.go）。
func (t *nativeTransport) PeerAddr() string      { return t.ch.conn.RemoteAddr().String() }
func (t *nativeTransport) TransportKind() string { return "native" }
func (t *nativeTransport) LastActive() time.Time {
	return time.Unix(0, t.ch.lastRead.Load())
}

// invokerSink 二进制 invoker 的响应出口。
type invokerSink struct {
	ch     *Channel
	closed chan struct{}
	once   sync.Once
}

func (s *invokerSink) Closed() <-chan struct{} { return s.closed }

func (s *invokerSink) finish() { s.once.Do(func() { close(s.closed) }) }

func (s *invokerSink) RespondCommon(status int, message, clientID string, seq int32) {
	pkt := protocol.NewPacket(protocol.STypeInvokeResponse)
	pkt.Seq = seq
	pkt.Data = commonResJSON(status, message, clientID, seq)
	pkt.AddHeader(protocol.HTTPContentType, protocol.HTTPContentTypeJSON)
	pkt.AddHeader(protocol.HeaderClientID, clientID)
	pkt.AddHeader(protocol.ResponseStatus, strconv.Itoa(status))
	s.ch.writePacket(pkt)
	s.finish()
}

func (s *invokerSink) RespondPacket(p *protocol.Packet) {
	// 出站压缩门（对齐 invoker 通道保留的 NettyCompressHandler）：
	// 头驱动 + 未带跳过标记时重压缩
	if !compress.ShouldSkip(p) {
		if err := s.ch.comp.CompressPacket(p); err != nil {
			s.ch.log.Warn("compress forward failed", "err", err)
		}
	}
	s.ch.writePacket(p)
	s.finish()
}
