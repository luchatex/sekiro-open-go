// Package native 实现 sekiro 二进制协议通道：
// provider client 注册（C_TYPE_REGISTER）与 invoker 连接（I_TYPE_CONNECT）。
// 对齐 RouterSekiro / ServiceSekiroClient / ServiceSekiroInvoker /
// ServerHeartBeatHandler（读空闲 30s 关闭、写空闲 20s 发心跳）。
package native

import (
	"bufio"
	"context"
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
	ctx       context.Context
	cancel    context.CancelFunc
	dead      atomic.Bool
	lastRead  atomic.Int64
	lastWrite atomic.Int64
	writeMu   sync.Mutex // 并发写保护：多个 HTTP handler 同时向同一
	// provider 下发 S_TYPE_INVOKE，与心跳线程并发——TCP 流式帧必须串行化，
	// 否则帧交错损坏（压测实证：偶发响应丢失 → 15s 超时）
	closeOnce sync.Once
	stopHB    chan struct{}

	invMu      sync.Mutex
	invPending map[int32]*group.Record // invoker 通道未完成调用
}

// ServeChannel 服务一条二进制协议连接（阻塞直到连接结束）。
func ServeChannel(conn net.Conn, br *bufio.Reader, mgr *group.Manager, comp *compress.Manager, log *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := &Channel{
		conn:   conn,
		br:     br,
		mgr:    mgr,
		comp:   comp,
		log:    log.With("peer", conn.RemoteAddr().String()),
		ctx:    ctx,
		cancel: cancel,
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
		c.dead.Store(true)
		c.cancel()
		close(c.stopHB)
		// 先释放 socket 与所有等待中的分配，再做会写回消费方的失败通知，
		// 避免关闭路径被慢消费方或 actor 邮箱反向阻塞。
		_ = c.conn.Close()
		c.abortPendingInvokes()
		if c.client != nil {
			c.client.Disconnect()
		}
	})
}

// trackInvoke 登记 invoker 未完成调用；重复 seq 返回 false。
func (c *Channel) trackInvoke(seq int32, rec *group.Record) bool {
	c.invMu.Lock()
	defer c.invMu.Unlock()
	if c.invPending == nil {
		c.invPending = make(map[int32]*group.Record)
	}
	if _, exists := c.invPending[seq]; exists {
		return false
	}
	c.invPending[seq] = rec
	return true
}

func (c *Channel) finishInvoke(seq int32) {
	c.invMu.Lock()
	delete(c.invPending, seq)
	c.invMu.Unlock()
}

// abortPendingInvokes invoker 断开时立即取消所有未完成调用，释放 provider
// pending、定时器和请求参数，而不是等各自的 15s 超时。
func (c *Channel) abortPendingInvokes() {
	c.invMu.Lock()
	recs := c.invPending
	c.invPending = nil
	c.invMu.Unlock()
	for _, rec := range recs {
		rec.Abort()
	}
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
				c.log.Info("binary provider closed", "reason", "heartbeat-write-error", "err", err)
				c.close()
				return
			}
			c.log.Debug("server write idle, send heartbeat")
		}
	}
}

func (c *Channel) writePacket(p *protocol.Packet) error {
	c.writeMu.Lock()
	c.lastWrite.Store(time.Now().UnixNano())
	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err := protocol.WritePacket(c.conn, p)
	_ = c.conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()
	if err != nil {
		// 写失败是连接死亡的权威信号；立即转 dead 并关闭，不能等下一次读超时。
		c.dead.Store(true)
		_ = c.conn.Close()
	}
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
	sink := &invokerSink{ch: c, seq: p.Seq, closed: make(chan struct{})}
	timeout := time.Duration(params.GetInt(protocol.ParamInvokeTimeout)) * time.Millisecond
	rec := group.NewRecord(params, sink, p.Seq, timeout) // <500ms 由 NewRecord 提升为默认值
	if !c.trackInvoke(p.Seq, rec) {
		c.log.Warn("duplicate invoker seq", "seq", p.Seq)
		c.closeConn()
		return
	}
	// 监控埋点：二进制 invoker 入口记总量与请求流量
	rec.Meter = metrics.Default()
	rec.Meter.Invoke(
		params.GetString(protocol.ParamGroup),
		params.GetString(protocol.ParamAction),
		int64(len(data)),
	)
	client, aerr := c.mgr.AllocateContext(
		c.ctx,
		params.GetString(protocol.ParamGroup),
		params.GetString(protocol.ParamBindClient),
		params.GetString(protocol.ParamConstantInvoke),
	)
	if aerr != nil {
		if c.ctx.Err() != nil {
			rec.Abort()
			c.finishInvoke(p.Seq)
			return
		}
		rec.Fail(aerr.Error(), "") // sink 写入后自行 finishInvoke
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
	seq    int32
	closed chan struct{}
	once   sync.Once
}

func (s *invokerSink) Closed() <-chan struct{} { return s.closed }

func (s *invokerSink) finish() {
	s.once.Do(func() {
		s.ch.finishInvoke(s.seq)
		close(s.closed)
	})
}

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
