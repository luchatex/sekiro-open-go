// Package group 实现 sekiro 的 group 调度核心：
// 每 group 一个 actor（单 goroutine 串行化状态，对齐 Java Looper 单线程事件循环），
// 客户端注册表、轮询/一致性哈希/bind_client 三种分配、空池退避、重复注册处理。
package group

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// Transport provider 客户端传输通道（native 二进制连接 / WebSocket 连接）。
type Transport interface {
	// SendInvoke 下发调用请求（native: S_TYPE_INVOKE 帧；ws: JSON 文本帧 + __sekiro_seq__）。
	SendInvoke(params *orderedjson.Map, seq int32) error
	Active() bool
	Close()
	// CloseAfter 延迟关闭（对齐重复注册时旧连接 30s 后关闭）。
	CloseAfter(d time.Duration)
}

// Client 一个已注册的 provider 客户端。
type Client struct {
	ID        string
	GroupName string
	Key       uint64 // murmur(ID)

	tr  Transport
	grp *Group

	offline     atomic.Bool
	seqNum      atomic.Int32
	connectedAt atomic.Int64 // 本次连接建立时刻（register 时写入，纳秒）

	mu      sync.Mutex
	pending map[int32]*Record

	log *slog.Logger
}

// NewClient 创建一个 provider 客户端并注册到 group（对齐 NettyClient 构造）。
func NewClient(tr Transport, grp *Group, id string, log *slog.Logger) *Client {
	c := &Client{
		ID:        id,
		GroupName: grp.name,
		Key:       protocol.MurmurHashString(id),
		tr:        tr,
		grp:       grp,
		pending:   make(map[int32]*Record),
		log:       log,
	}
	c.seqNum.Store(1)
	return c
}

func (c *Client) Active() bool { return c.tr.Active() }

func (c *Client) IsDown() bool { return c.offline.Load() || !c.tr.Active() }

// ForwardInvoke 下发调用（对齐 NettyClient.forwardInvoke）。
//
// 顺序约束（两段都不能交换，provider 响应可达毫秒级）：
//  1. 先登记 pending 再 SendInvoke：若先发送后登记，高并发下本 goroutine
//     被调度延迟时响应会先于登记到达 HandleResponse，pending 查空导致
//     响应被丢弃、请求空等 15s 超时（压测实证：20000 请求约 11 例，0.05%）。
//  2. attached 也必须在 SendInvoke 之前：pending 登记后响应即可到达
//     deliver，若此时尚未 attached，r.client 为 nil → 响应头
//     SEKIRO_CLIENT_ID 为空（-cover 下 e2e 偶发 "clientId header
//     missing" 实证）。超时定时器随之略微提前启动，误差可忽略。
func (c *Client) ForwardInvoke(rec *Record) {
	seq := c.newSeq()
	c.mu.Lock()
	c.pending[seq] = rec
	c.mu.Unlock()
	rec.attached(c, seq)
	if err := c.tr.SendInvoke(rec.Params, seq); err != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		rec.fail("write request to broken upstream: "+err.Error(), c.ID)
		return
	}
}

// newSeq 对齐 NettyClient.newSeq：超过 0.75*MaxInt32 时 CAS 重置。
func (c *Client) newSeq() int32 {
	seq := c.seqNum.Add(1)
	const maxI32 = int32(2147483647)
	if seq > maxI32/4*3 { // 0.75 * MaxInt32
		now := c.seqNum.Load()
		if now > maxI32/2 { // 0.5 * MaxInt32
			c.seqNum.CompareAndSwap(now, 1)
		}
		seq = c.seqNum.Add(1)
	}
	return seq
}

// HandleResponse 路由 provider 响应（对齐 NettyClient.response）。
func (c *Client) HandleResponse(p *protocol.Packet) {
	// 分段流模式（SEGMENT_STREAM_FLAG=true）不移除 record
	segment := strings.EqualFold(p.GetHeader(protocol.SegmentStreamFlag), "true")
	var rec *Record
	c.mu.Lock()
	if segment {
		rec = c.pending[p.Seq]
	} else {
		rec = c.pending[p.Seq]
		delete(c.pending, p.Seq)
	}
	c.mu.Unlock()
	if rec == nil {
		c.log.Debug("receive invoke response after user timeout", "seq", p.Seq)
		return
	}
	rec.deliver(p)
}

// Disconnect 连接断开：注销并失败所有pending（对齐 onClientDisconnected）。
//
// 锁内只做「摘走pending」，逐个 rec.fail 必须在锁外：
// fail 会经sink.RespondCommon 写回消费方（HTTP 即写TCP 响应），
// 是阻塞 IO。若持锁执行，一个读端滞后的消费方会把整个 client 的
// 锁按住——期间新到的 provider 响应（HandleResponse 需c.mu）
// 与新下发调用（ForwardInvoke 需 c.mu）全部阻塞，
// 于是「一个慢消费方」被放大成「整个 provider 不可用」。
func (c *Client) Disconnect() {
	c.grp.unregister(c)
	c.grp.post(func() {
		c.mu.Lock()
		recs := make([]*Record, 0, len(c.pending))
		for seq, rec := range c.pending {
			delete(c.pending, seq)
			recs = append(recs, rec)
		}
		c.mu.Unlock()

		for _, rec := range recs {
			rec.fail("sekiro upstream lose connection", c.ID)
		}
	})
}

// MarkOffline 主动下线（C_TYPE_OFFLINE，对齐 doOffline）。
func (c *Client) MarkOffline() {
	if c.ID == "" {
		c.tr.Close()
		return
	}
	c.grp.offlineClient(c)
}
