package group

import (
	"encoding/binary"
	"strings"
	"sync"
	"time"

	"sekiro-go/internal/metrics"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// Sink 消费方响应出口（HTTP 连接 / 二进制 invoker 连接）。
type Sink interface {
	// RespondCommon 失败类响应（pool queue empty / no device found / timeout 等），
	// 对齐 CommonRes + CommonRes2HttpEncoder / CommonRes2SekiroEncoder。
	RespondCommon(status int, message, clientID string, seq int32)
	// RespondPacket 转发 provider 响应包（已解压、已注入 clientId）。
	RespondPacket(p *protocol.Packet)
	// Closed 响应终止信号（HTTP handler 等待用）。
	Closed() <-chan struct{}
}

// Record 一次调用的关联记录（对齐 InvokeRecord）。
type Record struct {
	Params *orderedjson.Map
	sink   Sink

	client     *Client
	seq        int32
	invokerSeq int32 // >=0 表示消费方为二进制 invoker；-1 表示 HTTP
	timeout    time.Duration

	segment     bool // 已进入分段流模式
	extendedTil time.Time
	timer       *time.Timer

	// terminal 终结原因（metrics.ErrTimeout / ErrAbort / ErrDispatch），
	// 空 = 未终结。用于区分「超时后的迟到回包」与其他终结（迟到响应只对
	// 超时才有诊断意义：调用方断连/分配失败时 provider 本就不该回包）。
	terminal string
	endedAt  time.Time // 终结时刻（迟到延迟的起算点）
	// lateCounted 分段流的迟到包可能连续到达多条，只按首包计一次迟到，
	// 否则一个挂死的大响应会把迟到计数刷成几十条。
	lateCounted bool

	// Meter 可选指标记录器（nil 安全，测试默认不设）：
	// 终结时按 success/fail/sysErr 分类计数，并累加响应流量。
	Meter *metrics.Recorder

	mu   sync.Mutex
	done bool
}

func NewRecord(params *orderedjson.Map, sink Sink, invokerSeq int32, timeout time.Duration) *Record {
	if timeout < 500*time.Millisecond {
		timeout = protocol.DefaultInvokeTimeout // 对齐：小于 500ms 按默认 15s
	}
	return &Record{
		Params:     params,
		sink:       sink,
		invokerSeq: invokerSeq,
		timeout:    timeout,
	}
}

// metricKeys 统计维度（终结时解析一次）。
func (r *Record) metricKeys() (group, action string) {
	return r.Params.GetString(protocol.ParamGroup), r.Params.GetString(protocol.ParamAction)
}

// finishMetric 终结埋点。bytesOut 为协议 payload 逻辑字节；
// 实际出站字节（压缩后）从 sink 采集（HTTP 消费方的 compressWriter
// 计数，WS/native 消费方 sink 无该能力记 0）。
// 必须在 sink.RespondPacket 写完全部响应之后调用，否则计数漏末包。
func (r *Record) finishMetric(k metrics.Kind, bytesOut int64) {
	if r.Meter == nil {
		return
	}
	var wire int64
	if wb, ok := r.sink.(interface{ WireBytes() int64 }); ok {
		wire = wb.WireBytes()
	}
	g, a := r.metricKeys()
	r.Meter.Finish(g, a, k, bytesOut, wire)
}

// finishSysErr 系统错误终结埋点：除计数外携带原因/message/clientId/通道，
// 供监控页「系统错误分析」聚合与事件列表使用（message 为服务端固定文案）。
//
// 参数摘要只在 -op-err-params 开启时采集：超时/分配失败大多源于「某几条
// 具体请求反复失败」，只有参数能把它指认出来；默认关闭使监控页不含业务数据。
func (r *Record) finishSysErr(reason, message string) {
	if r.Meter == nil {
		return
	}
	g, a := r.metricKeys()
	channel := "http"
	if r.invokerSeq >= 0 {
		channel = "invoker"
	}
	var params string
	if metrics.CaptureParams() {
		params = paramDigest(r.Params)
	}
	r.Meter.RecordSysErr(g, a, reason, message, r.clientID(), channel, params)
}

// digestSkipKeys 框架键：属协议层（分组/动作/超时/序号），对定位业务请求
// 无价值且稳定占预算，摘要里剔除。
var digestSkipKeys = map[string]bool{
	protocol.ParamGroup:          true,
	protocol.ParamAction:         true,
	protocol.ParamBindClient:     true,
	protocol.ParamConstantInvoke: true,
	protocol.ParamInvokeTimeout:  true,
	protocol.WSSeqNumber:         true,
	protocol.ParamApiToken:       true,
}

// digestMaxBytes 参数摘要长度上限：够放 URL + 业务 ID，又不至于把大 body
// 灌进监控页与事件环形缓冲（100 条 × 256B ≈ 25KB）。
const digestMaxBytes = 256

func paramDigest(p *orderedjson.Map) string {
	if p == nil {
		return ""
	}
	return p.Digest(func(k string) bool { return digestSkipKeys[k] }, digestMaxBytes)
}

// attached 写成功后回调（对齐 afterForward）：启动超时定时器。
func (r *Record) attached(c *Client, seq int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return // Disconnect 与 attached 竞态：已终结就不再创建长定时器
	}
	r.client = c
	r.seq = seq
	r.timer = time.AfterFunc(r.timeout, r.onTimeout)
}

func (r *Record) isDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// commonSeq 响应 seq：WS 消费方优先回写原始 __sekiro_seq__（对齐 response(CommonRes)）。
func (r *Record) commonSeq() int32 {
	r.mu.Lock()
	seq := r.seq
	r.mu.Unlock()
	if s := r.Params.GetInt(protocol.WSSeqNumber); s > 0 {
		return int32(s)
	}
	return seq
}

// Fail CommonRes.failed 路径（分配失败/上游丢失等错误响应）。
func (r *Record) Fail(message, clientID string) { r.fail(message, clientID) }

// fail CommonRes.failed 路径（分配失败/上游丢失等错误响应）。
func (r *Record) fail(message, clientID string) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	r.terminal = metrics.ErrDispatch
	r.endedAt = time.Now()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	r.sink.RespondCommon(-1, message, clientID, r.commonSeq())
	r.finishSysErr(metrics.ErrDispatch, message) // 分配失败/上游丢失（细分见 message）
}

// Abort 消费方已断开：终结 Record 但不写回任何响应。
//
// 与 fail 的区别是「不碰sink」——消费方连接都没了，写响应只会往
// 一个已关闭的 socket 写（HTTP 场景还会触发 compressWriter 对已
// 终结流的写入）。仍需完成三件事，与 fail/onTimeout 一致：
//  1. 置done，保证后续 provider 响应不会重复终结
//  2. 停超时定时器，避免空转
//  3. 从 client.pending 摘除，否则 pending 会一直挂到超时才被清理
//
// 由HTTP handler 在 r.Context().Done() 时调用：客户端断连后不再
// 驻留到15s 超时。
// 返回 true 表示本次 Abort 赢得终结；false 表示已有响应/超时先完成，
// HTTP handler 应等待该写回结束再关闭压缩流。
func (r *Record) Abort() bool {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return false
	}
	r.done = true
	r.terminal = metrics.ErrAbort
	r.endedAt = time.Now()
	if r.timer != nil {
		r.timer.Stop()
	}
	c, seq := r.client, r.seq
	r.mu.Unlock()

	if c != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
	}
	r.finishSysErr(metrics.ErrAbort, "") // 调用方提前断开
	return true
}

// onTimeout 对齐 scheduleTimeout（含分段流的滚动续期）。
func (r *Record) onTimeout() {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	if r.segment && time.Now().Before(r.extendedTil) {
		// 只等到最后活跃时刻 + timeout，不能固定再加一个完整 timeout
		r.timer.Reset(time.Until(r.extendedTil))
		r.mu.Unlock()
		return
	}
	r.done = true
	r.terminal = metrics.ErrTimeout
	r.endedAt = time.Now()
	c, seq := r.client, r.seq
	r.mu.Unlock()
	if c != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
	}
	r.sink.RespondCommon(-1, "timeout", r.clientID(), r.commonSeq())
	r.finishSysErr(metrics.ErrTimeout, "timeout") // provider 未在超时内回包
}

// resultKind 从 provider 响应包判定调用结果：
// SekiroFastJson 信封前 4 字节为 status（0=成功，非 0=provider 业务失败）；
// 非 fastjson（JSON/分段流）响应即视为成功投递。
func (r *Record) resultKind(p *protocol.Packet) metrics.Kind {
	if p.GetHeader(protocol.PayloadContentType) == protocol.ContentTypeSekiroFastJson &&
		len(p.Data) >= 4 {
		if st := int32(binary.BigEndian.Uint32(p.Data[:4])); st != 0 {
			return metrics.Fail
		}
	}
	return metrics.Success
}

func (r *Record) clientID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return r.client.ID
	}
	return ""
}

// deliver provider 响应到达（对齐 InvokeRecord.response(SekiroPacket)）。
func (r *Record) deliver(p *protocol.Packet) {
	r.mu.Lock()
	if r.done {
		// 迟到回包：只在「已按超时终结」时计入迟到统计——此时调用方早已
		// 拿到 timeout，provider 的回包只能丢弃。这条计数是区分「上游只是
		// 慢」与「上游挂死」的唯一客观依据（见 metrics.RecordLate）。
		late := r.terminal == metrics.ErrTimeout && !r.lateCounted
		delay := time.Duration(0)
		if late {
			r.lateCounted = true
			if !r.endedAt.IsZero() {
				delay = time.Since(r.endedAt)
			}
		}
		r.mu.Unlock()
		if late && r.Meter != nil {
			g, a := r.metricKeys()
			r.Meter.RecordLate(g, a, delay)
		}
		return
	}
	// 分段流：响应头携带 SEGMENT_STREAM_FLAG=true
	// 大小写口径与 Client.HandleResponse 保持一致（Java 端存在 True/TRUE 写法）
	isSegment := strings.EqualFold("true", p.GetHeader(protocol.SegmentStreamFlag))
	if isSegment {
		r.segment = true
		r.extendedTil = time.Now().Add(r.timeout)
	}
	if p.Type == protocol.CTypeInvokeResponse {
		// sekiro v2 协议兼容：C→S 类型降级
		p.Type = protocol.STypeInvokeResponse
	}
	if r.invokerSeq >= 0 {
		p.Seq = r.invokerSeq
	}
	// 无分段标记的包：要么是一次性响应（fast-json / v2 兼容），
	// 要么是分段流的终结包（onceCall）；带标记的空包也是 HTTP sink
	// 的终结信号。三者必须使用同一判定，否则 sink 已返回而 Record
	// 仍挂 pending，最终多记一次 timeout 并向已关闭响应二次写入。
	finish := !isSegment || len(p.Data) == 0
	c, seq := r.client, r.seq
	if finish {
		r.done = true
		if r.timer != nil {
			r.timer.Stop()
		}
	}
	r.mu.Unlock()

	if finish && c != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
	}

	clientID := ""
	if c != nil {
		clientID = c.ID
	}
	p.AddHeader(protocol.HeaderClientID, clientID)
	// 指标先取逻辑值：invokerSink 会原地压缩 p.Data，写入后再读会把
	// 压缩后字节当逻辑出流量，并可能把压缩流前 4 字节误判为 status。
	kind := r.resultKind(p)
	logicalBytes := int64(len(p.Data))
	r.sink.RespondPacket(p)

	// 指标埋点：终结包按 status 分类计数；分段流中间包只累加响应流量。
	// 实际出站字节由 finishMetric 从 sink 的 WireBytes 单独采集。
	if r.Meter != nil {
		if finish {
			r.finishMetric(kind, logicalBytes)
		} else if logicalBytes > 0 {
			g, a := r.metricKeys()
			r.Meter.AddOut(g, a, logicalBytes)
		}
	}
}
