package protocol

import "sekiro-go/internal/orderedjson"

// InvokeTimeoutMillis 读取调用方期望的 invoke 超时（毫秒）。
//
// 历史上部分业务脚本把参数写成 `invoke_timeOut`（大写 O），服务端此前只认
// `invoke_timeout`，导致 100s 的意图被静默忽略、回退到默认 15s 并频繁超时。
// 这里做兼容：优先取正确拼写，缺失/为 0 时再取历史拼写；两者都有时正确拼写
// 优先，避免别名意外覆盖显式配置。
func InvokeTimeoutMillis(params *orderedjson.Map) int {
	if params == nil {
		return 0
	}
	if ms := params.GetInt(ParamInvokeTimeout); ms != 0 {
		return ms
	}
	return params.GetInt(ParamInvokeTimeoutAlias)
}
