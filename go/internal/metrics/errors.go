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
	"sync/atomic"
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

// 聚合视图的容量上限：错误维度理论上无界（action 名来自调用方），
// 用条目上限 + 惰性过期兜住内存。
const (
	maxErrAgg  = 200 // 聚合条目上限（group×action×reason）
	maxAggOut  = 50  // /op/api/errors 单次输出的聚合行数上限
	maxLateOut = 20  // 迟到响应输出行数上限
)

// SysErrEvent 单条系统错误事件。
type SysErrEvent struct {
	Time    int64  `json:"time"`  // unix 秒
	Group   string `json:"group"` // 可能为空（group 缺失类错误）
	Action  string `json:"action,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"` // 服务端固定文案，无业务参数
	// Params 请求参数摘要：仅 -op-err-params 开启时填充（默认关）。
	// 用于定位「反复失败的同一条请求」，默认不入事件（避免业务数据进监控）。
	Params   string `json:"params,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	Channel  string `json:"channel,omitempty"` // http / invoker（消费方通道）
}

// reasonStat 单原因聚合（全量计数 + 最近发生时刻）。
type reasonStat struct {
	count int64
	last  int64 // unix 秒
}

// aggKey 聚合视图键：同 group/action/原因归为一行。
type aggKey struct{ group, action, reason string }

// aggStat 聚合条目。
type aggStat struct {
	count int64
	last  int64 // unix 秒
}

// lateStat 迟到响应聚合（provider 在服务端判超时之后才回包）。
type lateStat struct {
	count    int64
	last     int64 // unix 秒
	maxDelay int64 // 毫秒（超时判定的时刻 → 回包到达的间隔）
}

// SysErrReasonJSON 原因分布（JSON 形态）。
type SysErrReasonJSON struct {
	Reason   string `json:"reason"`
	Count    int64  `json:"count"`
	LastTime int64  `json:"lastTime"` // unix 秒
}

// ErrAggJSON 系统错误聚合行（group/action/原因 → 次数）。
type ErrAggJSON struct {
	Group    string `json:"group"`
	Action   string `json:"action,omitempty"`
	Reason   string `json:"reason"`
	Count    int64  `json:"count"`
	LastTime int64  `json:"lastTime"`
}

// LateAggJSON 迟到响应聚合行。
type LateAggJSON struct {
	Group      string `json:"group"`
	Action     string `json:"action,omitempty"`
	Count      int64  `json:"count"`
	LastTime   int64  `json:"lastTime"`
	MaxDelayMs int64  `json:"maxDelayMs"`
}

// ErrorsJSON /op/api/errors 输出。
type ErrorsJSON struct {
	ServerTime int64              `json:"serverTime"`
	Total      int64              `json:"total"` // 系统错误总数（全量）
	Reasons    []SysErrReasonJSON `json:"reasons"`
	// ByAction 按 group/action/原因聚合（次数降序），让「每分钟固定几条
	// 同类错误」直接显形，不必翻事件流找规律。
	ByAction []ErrAggJSON `json:"byAction"`
	// LateTotal / Late 迟到响应（超时后才回包）：>0 说明 provider 是「慢」
	// 而不是「挂死」，是判断超时该调参还是该放弃的唯一客观依据。
	LateTotal     int64         `json:"lateTotal"`
	Late          []LateAggJSON `json:"late"`
	CaptureParams bool          `json:"captureParams"` // 事件是否带参数摘要（-op-err-params）
	Recent        []SysErrEvent `json:"recent"`        // 最新在前，最多 errRingCap 条
}

// RecordSysErr 记录一条系统错误：完成 SysErr 计数（复用 Finish 计数路径）、
// 原因聚合（entity 级 + 全局 + group/action 聚合）与最近事件环形写入。
// params 为请求参数摘要（未开启 -op-err-params 时为空）。nil 接收者安全。
func (r *Recorder) RecordSysErr(group, action, reason, message, clientID, channel, params string) {
	if r == nil {
		return
	}
	now := time.Now()
	h, d := now.Unix()/3600, dayKey(now)

	r.mu.Lock()
	defer r.mu.Unlock()

	if group != "" {
		// 计数路径与 Finish(SysErr) 完全一致（总量 + 小时桶 + 天桶）
		e := r.entityLocked(group, action)
		e.m.addResult(SysErr, 0, 0)
		e.touchLocked(h, d, func(b *Metrics) { b.addResult(SysErr, 0, 0) })
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

	// 聚合视图：group×action×原因归为一行（条目上限见 maxErrAgg）
	ak := aggKey{group, action, reason}
	if as := r.aggs[ak]; as != nil {
		as.count++
		as.last = now.Unix()
	} else if group != "" {
		if len(r.aggs) >= maxErrAgg {
			r.evictOldestAggLocked()
		}
		if len(r.aggs) < maxErrAgg {
			r.aggs[ak] = &aggStat{count: 1, last: now.Unix()}
		}
	}

	// 环形写入：ringN 为累计写入数，取模定位（覆盖最旧）
	r.ring[r.ringN%errRingCap] = SysErrEvent{
		Time: now.Unix(), Group: group, Action: action,
		Reason: reason, Message: message, Params: params,
		ClientID: clientID, Channel: channel,
	}
	r.ringN++
}

// evictOldestAggLocked 聚合表满时淘汰最近最少发生的一行（O(n)，仅满表时触发）。
func (r *Recorder) evictOldestAggLocked() {
	var oldestKey aggKey
	oldest := int64(1<<62 - 1)
	for k, as := range r.aggs {
		if as.last < oldest {
			oldest, oldestKey = as.last, k
		}
	}
	delete(r.aggs, oldestKey)
}

// RecordLate 记录一次「迟到响应」：服务端已按超时终结该调用，provider 之后
// 才回包（该回包被丢弃，调用方已拿到 timeout）。
//
// 语义价值：超时计数只说明「没按时回」，无从区分是上游真的挂了、还是只是
// 慢过阈值。迟到响应的延迟分布是唯一客观依据——若迟到普遍只差 2–5s，
// 说明该调大 invoke_timeout 或修客户端超时；若迟到出现在几十秒到几分钟后，
// 说明是挂死连接，应当断链与重试。nil 接收者安全。
func (r *Recorder) RecordLate(group, action string, delay time.Duration) {
	if r == nil || group == "" {
		return
	}
	ms := delay.Milliseconds()
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	ls := r.lates[key{group, action}]
	if ls == nil {
		ls = &lateStat{}
		r.lates[key{group, action}] = ls
	}
	ls.count++
	ls.last = now
	if ms > ls.maxDelay {
		ls.maxDelay = ms
	}
}

// Errors 生成系统错误快照：原因/聚合按次数降序、事件最新在前。nil 接收者安全。
func (r *Recorder) Errors() *ErrorsJSON {
	if r == nil {
		return &ErrorsJSON{ServerTime: time.Now().Unix(), Reasons: []SysErrReasonJSON{},
			ByAction: []ErrAggJSON{}, Late: []LateAggJSON{}, Recent: []SysErrEvent{},
			CaptureParams: CaptureParams()}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := &ErrorsJSON{ServerTime: time.Now().Unix(), Reasons: []SysErrReasonJSON{},
		ByAction: make([]ErrAggJSON, 0, len(r.aggs)), Late: make([]LateAggJSON, 0, len(r.lates)),
		Recent: []SysErrEvent{}, CaptureParams: CaptureParams()}
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

	for k, as := range r.aggs {
		out.ByAction = append(out.ByAction, ErrAggJSON{
			Group: k.group, Action: k.action, Reason: k.reason,
			Count: as.count, LastTime: as.last,
		})
	}
	sort.Slice(out.ByAction, func(i, j int) bool {
		a, b := out.ByAction[i], out.ByAction[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Action != b.Action {
			return a.Action < b.Action
		}
		return a.Reason < b.Reason
	})
	if len(out.ByAction) > maxAggOut {
		out.ByAction = out.ByAction[:maxAggOut]
	}

	for k, ls := range r.lates {
		out.LateTotal += ls.count
		out.Late = append(out.Late, LateAggJSON{
			Group: k.group, Action: k.action, Count: ls.count,
			LastTime: ls.last, MaxDelayMs: ls.maxDelay,
		})
	}
	sort.Slice(out.Late, func(i, j int) bool {
		a, b := out.Late[i], out.Late[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.MaxDelayMs != b.MaxDelayMs {
			return a.MaxDelayMs > b.MaxDelayMs
		}
		return a.Group < b.Group
	})
	if len(out.Late) > maxLateOut {
		out.Late = out.Late[:maxLateOut]
	}

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

// ---- 参数摘要开关（-op-err-params） ----

// captureParams 错误事件是否附带请求参数摘要。默认关：监控页因此不含业务
// 参数（爬取目标 URL / 文书 ID 等），需要定位「毒丸请求」时由运维显式开启。
var captureParams atomic.Bool

// SetCaptureParams 开启/关闭参数摘要（启动时由 config 决定）。
func SetCaptureParams(on bool) { captureParams.Store(on) }

// CaptureParams 当前是否采集参数摘要（nil 安全，供埋点侧判空）。
func CaptureParams() bool { return captureParams.Load() }

// pruneLocked 清理长期静默的聚合条目（与 entity 的 7 天窗口无关：聚合只服务
// 近期排障，24h 未再发生即无参考价值）。
func (r *Recorder) pruneLocked(now int64) {
	cutoff := now - int64(24*time.Hour/time.Second)
	for k, as := range r.aggs {
		if as.last < cutoff {
			delete(r.aggs, k)
		}
	}
	for k, ls := range r.lates {
		if ls.last < cutoff {
			delete(r.lates, k)
		}
	}
}
