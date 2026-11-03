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

	"sekiro-go/internal/metrics"
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

	// lateSeqs 已按超时清理的序号：provider 迟到的回包要靠它才知道「这条曾经
	// 是什么调用」。record 到超时就被摘出 pending（防止挂死请求永久驻留），
	// 迟到包到达时已无从反查——只留 seq→(action, 时刻) 这一个极小条目，
	// 既能算出迟到延迟，也不必把 Record（连带 sink 引用）继续挂着。
	lateMu   sync.Mutex
	lateSeqs map[int32]lateSeq
	lateRing []int32 // 插入序，容量满时按序淘汰（seq 单调递增，无重复）

	log *slog.Logger
}

// lateTrackCap 迟到序号表容量上限（每 client）。
// 256 条 × 约 40B ≈ 10KB；线上典型错误率（每分钟几条）足够覆盖十几分钟，
// 既不会无界增长，也不会因为异常流量把内存顶起来。
const lateTrackCap = 256

// lateSeq 迟到序号条目。
type lateSeq struct {
	action string
	at     time.Time
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
	if c.IsDown() {
		rec.fail("sekiro upstream lose connection", c.ID)
		return
	}
	seq := c.newSeq()
	c.mu.Lock()
	// Disconnect 在 c.mu 外先置 offline，再持锁摘 pending。此处复核可堵住
	// “检查通过 → Disconnect 完成清理 → 又登记新 pending”的窗口。
	if c.offline.Load() {
		c.mu.Unlock()
		rec.fail("sekiro upstream lose connection", c.ID)
		return
	}
	c.pending[seq] = rec
	c.mu.Unlock()
	rec.attached(c, seq)
	if rec.isDone() {
		// Disconnect/fail 已终结本条调用，不再向死连接写。
		return
	}
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
		// 正常路径已在解压/解析前由 Pending + DropLateResponse 拦掉，
		// 这里只剩「判定与响应并发」的窄窗口。
		c.DropLateResponse(p.Seq)
		return
	}
	rec.deliver(p)
}

// Pending 该序号是否仍在等响应（供通道层在解压/解析**之前**判定丢弃）。
//
// 迟到回包的解压与 JSON 重组成本与响应体成正比：线上毒丸请求（5MB 详情页
// 在超时 20s 后才返回）此前会先完整解压 / 重新序列化，再因为 pending 查空被
// 丢掉——纯属白烧 CPU 与临时内存。加这一次无竞争锁（纳秒级）换掉整包处理。
func (c *Client) Pending(seq int32) bool {
	c.mu.Lock()
	_, ok := c.pending[seq]
	c.mu.Unlock()
	return ok
}

// trackLateTimeout 登记一个「已按超时终结、provider 可能仍会回包」的序号。
//
// 由 Record.onTimeout 调用：此刻 record 已从 pending 摘除，只能靠这张小表
// 在迟到包到达时反查 action 与超时时刻。
func (c *Client) trackLateTimeout(seq int32, action string) {
	c.lateMu.Lock()
	defer c.lateMu.Unlock()
	if c.lateSeqs == nil {
		c.lateSeqs = make(map[int32]lateSeq, 8)
	}
	if len(c.lateRing) >= lateTrackCap {
		delete(c.lateSeqs, c.lateRing[0]) // 淘汰最旧，环形切片容量已满则自然回收
		c.lateRing = c.lateRing[1:]
	}
	c.lateSeqs[seq] = lateSeq{action: action, at: time.Now()}
	c.lateRing = append(c.lateRing, seq)
}

// takeLate 取出并删除迟到条目（同一序号的多包只算首包）。
func (c *Client) takeLate(seq int32) (action string, delay time.Duration, ok bool) {
	c.lateMu.Lock()
	defer c.lateMu.Unlock()
	ls, ok := c.lateSeqs[seq]
	if !ok {
		return "", 0, false
	}
	delete(c.lateSeqs, seq)
	return ls.action, time.Since(ls.at), true
}

// DropLateResponse 处理「序号不在 pending」的响应：命中迟到表则计入迟到统计。
// 返回 true 表示该序号曾因超时被清理（迟到），false 表示完全未知（如客户端
// 自造序号、或服务端重启前的旧响应）。
//
// 统计走进程级默认记录器：此刻 Record 已终结回收，没有可用的 per-record
// Meter；生产上 Record.Meter 本就是 metrics.Default()，口径一致。
func (c *Client) DropLateResponse(seq int32) bool {
	action, delay, ok := c.takeLate(seq)
	if !ok {
		c.log.Debug("receive invoke response with unknown seq", "seq", seq)
		return false
	}
	c.log.Debug("late response after timeout", "seq", seq, "action", action, "delay", delay)
	metrics.Default().RecordLate(c.GroupName, action, delay)
	return true
}

// Disconnect 连接断开：注销并失败所有pending（对齐 onClientDisconnected）。
//
// 先同步置 offline 并摘走 pending，再投递索引清理，最后在锁外写失败响应：
//   - 置 offline 必须在投递 actor 之前，堵住断线后又登记 pending 的窗口；
//   - sink 写是阻塞 IO，不能在 client 锁或 group actor 中执行，否则一个
//     慢消费方会冻结整个 group 的注册、分配和监控。
func (c *Client) Disconnect() {
	c.offline.Store(true)
	c.mu.Lock()
	recs := make([]*Record, 0, len(c.pending))
	for seq, rec := range c.pending {
		delete(c.pending, seq)
		recs = append(recs, rec)
	}
	if len(recs) > 0 {
		c.pending = make(map[int32]*Record) // 释放 map 高水位 bucket
	}
	c.mu.Unlock()

	c.grp.unregister(c)
	for _, rec := range recs {
		rec.fail("sekiro upstream lose connection", c.ID)
	}
}

// MarkOffline 主动下线（C_TYPE_OFFLINE，对齐 doOffline）。
func (c *Client) MarkOffline() {
	if c.ID == "" {
		c.tr.Close()
		return
	}
	c.grp.offlineClient(c)
}
