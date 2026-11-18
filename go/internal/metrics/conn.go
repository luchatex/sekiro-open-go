package metrics

// 连接侧指标（invoke 指标之外的另一半事实）。
//
// 动机：/op 此前只统计 invoke 侧——「客户端压根没连上」在监控里是空白。
// 线上遇到 js-client 报 404/401、或 provider 被心跳判死后频繁重连时，监控页
// 一条记录都没有，只能人工 curl 试探。这里补上注册成功/失败与断开原因。
//
// 口径：进程启动以来累计（与 invoke 指标一致，重启清零）；不做时间桶——
// 连接事件量级远小于调用（典型 <1/min），存环形桶只会放大内存且无人看趋势。

// ConnJSON /op/api/stats 的 conn 字段。
type ConnJSON struct {
	OnlineNow    int              `json:"onlineNow"`    // 当前在线 provider 数（各 group 求和）
	RegisterOK   int64            `json:"registerOk"`   // 注册成功次数（含重连）
	RegisterFail int64            `json:"registerFail"` // 注册失败次数
	FailReasons  map[string]int64 `json:"failReasons,omitempty"`
	ClosedTotal  int64            `json:"closedTotal"`            // 连接终止次数
	CloseReasons map[string]int64 `json:"closeReasons,omitempty"` // 断开原因分布
}

// ConnRegisterOK 记一次注册成功（WS 握手通过并完成入组）。
func (r *Recorder) ConnRegisterOK() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.connOK++
	r.mu.Unlock()
}

// ConnRegisterFail 记一次注册失败。reason 取稳定短标签（param-missing /
// accept-error 等），不做自由文本，避免 map 基数失控。
func (r *Recorder) ConnRegisterFail(reason string) {
	if r == nil || reason == "" {
		return
	}
	r.mu.Lock()
	if r.connFailBy == nil {
		r.connFailBy = make(map[string]int64)
	}
	r.connFail++
	r.connFailBy[reason]++
	r.mu.Unlock()
}

// ConnClosed 记一次连接终止。reason 为断开原因（pong-timeout / read-idle /
// read-error / client-closed / bad-frame / bad-json / bad-seq）。
func (r *Recorder) ConnClosed(reason string) {
	if r == nil || reason == "" {
		return
	}
	r.mu.Lock()
	if r.connCloseBy == nil {
		r.connCloseBy = make(map[string]int64)
	}
	r.connClosed++
	r.connCloseBy[reason]++
	r.mu.Unlock()
}

// connLocked 复制连接侧计数（调用方须持有 r.mu）。
func (r *Recorder) connLocked() ConnJSON {
	out := ConnJSON{
		RegisterOK:   r.connOK,
		RegisterFail: r.connFail,
		ClosedTotal:  r.connClosed,
	}
	if len(r.connFailBy) > 0 {
		out.FailReasons = make(map[string]int64, len(r.connFailBy))
		for k, v := range r.connFailBy {
			out.FailReasons[k] = v
		}
	}
	if len(r.connCloseBy) > 0 {
		out.CloseReasons = make(map[string]int64, len(r.connCloseBy))
		for k, v := range r.connCloseBy {
			out.CloseReasons[k] = v
		}
	}
	return out
}
