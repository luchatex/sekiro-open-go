// 系统错误的原因维度与最近事件缓冲。
//
// 背景：SysErr 仅有计数（metrics.SysErr），监控页只能看到「N 条」，
// 无法定位是超时、分配失败还是调用方断连。本文件补齐两个维度：
//
//   - 原因聚合（全量常驻）：按 reason 累计，随 Snapshot 输出
//     （全局 + group 级 sysErrReasons）
//   - 最近事件环形缓冲（容量 100，滚动覆盖）：每条含时间/group/action/
//     原因/message 原文/clientId/消费通道，经 /op/api/errors 输出。
//     message 为服务端固定文案（如 "no device found for clientId:x"），
//     不含业务参数，展示无泄露风险。
package metrics

import (
	"sort"
	"time"
)

// 系统错误原因枚举（op.html 有对应说明文案）。
const (
	// ErrTimeout provider 未在 invoke_timeout 内回包。
	ErrTimeout = "timeout"
	// ErrDispatch 分配失败/上游丢失：空池、无设备、group 缺失、上游连接
	// 断开、写请求失败等（细分见事件 message 原文）。
	ErrDispatch = "dispatch-failed"
	// ErrAbort 调用方（HTTP/WS 消费方）提前断开连接。
	ErrAbort = "caller-abort"
)

// errRingCap 最近事件环形缓冲容量（约 100×250B ≈ 25KB 内存上限）。
const errRingCap = 100

// SysErrEvent 单条系统错误事件。
type SysErrEvent struct {
	Time     int64  `json:"time"`  // unix 秒
	Group    string `json:"group"` // 可能为空（group 缺失类错误）
	Action   string `json:"action,omitempty"`
	Reason   string `json:"reason"`
	Message  string `json:"message,omitempty"` // 服务端固定文案，无业务参数
	ClientID string `json:"clientId,omitempty"`
	Channel  string `json:"channel,omitempty"` // http / invoker（消费方通道）
}

// reasonStat 单原因聚合（全量计数 + 最近发生时刻）。
type reasonStat struct {
	count int64
	last  int64 // unix 秒
}

// SysErrReasonJSON 原因分布（JSON 形态）。
type SysErrReasonJSON struct {
	Reason   string `json:"reason"`
	Count    int64  `json:"count"`
	LastTime int64  `json:"lastTime"` // unix 秒
}

// ErrorsJSON /op/api/errors 输出。
type ErrorsJSON struct {
	ServerTime int64              `json:"serverTime"`
	Total      int64              `json:"total"` // 系统错误总数（全量）
	Reasons    []SysErrReasonJSON `json:"reasons"`
	Recent     []SysErrEvent      `json:"recent"` // 最新在前，最多 errRingCap 条
}

// RecordSysErr 记录一条系统错误：完成 SysErr 计数（复用 Finish 计数路径）、
// 原因聚合（entity 级 + 全局）与最近事件环形写入。nil 接收者安全。
func (r *Recorder) RecordSysErr(group, action, reason, message, clientID, channel string) {
	if r == nil {
		return
	}
	now := time.Now()
	h := now.Unix() / 3600

	r.mu.Lock()
	defer r.mu.Unlock()

	if group != "" {
		// 计数路径与 Finish(SysErr) 完全一致（总量/小时桶）
		e := r.entityLocked(group, action)
		e.m.addResult(SysErr, 0)
		e.hourLocked(h).addResult(SysErr, 0)
		if e.reasons == nil {
			e.reasons = make(map[string]int64)
		}
		e.reasons[reason]++
	}
	if r.reasons == nil {
		r.reasons = make(map[string]*reasonStat)
	}
	rs := r.reasons[reason]
	if rs == nil {
		rs = &reasonStat{}
		r.reasons[reason] = rs
	}
	rs.count++
	rs.last = now.Unix()

	// 环形写入：ringN 为累计写入数，取模定位（覆盖最旧）
	r.ring[r.ringN%errRingCap] = SysErrEvent{
		Time: now.Unix(), Group: group, Action: action,
		Reason: reason, Message: message, ClientID: clientID, Channel: channel,
	}
	r.ringN++
}

// Errors 生成系统错误快照：原因按次数降序、事件最新在前。nil 接收者安全。
func (r *Recorder) Errors() *ErrorsJSON {
	if r == nil {
		return &ErrorsJSON{ServerTime: time.Now().Unix(),
			Reasons: []SysErrReasonJSON{}, Recent: []SysErrEvent{}}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := &ErrorsJSON{ServerTime: time.Now().Unix(),
		Reasons: []SysErrReasonJSON{}, Recent: []SysErrEvent{}}
	for reason, rs := range r.reasons {
		out.Total += rs.count
		out.Reasons = append(out.Reasons, SysErrReasonJSON{
			Reason: reason, Count: rs.count, LastTime: rs.last,
		})
	}
	sort.Slice(out.Reasons, func(i, j int) bool {
		if out.Reasons[i].Count != out.Reasons[j].Count {
			return out.Reasons[i].Count > out.Reasons[j].Count
		}
		return out.Reasons[i].Reason < out.Reasons[j].Reason
	})

	// 环形展开：最新在前
	n := r.ringN
	if n > errRingCap {
		n = errRingCap
	}
	for i := int64(0); i < n; i++ {
		out.Recent = append(out.Recent, r.ring[(r.ringN-1-i)%errRingCap])
	}
	return out
}
