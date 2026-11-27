// Package protocol 实现 sekiro 二进制线上协议（与 yint-tech/sekiro-open 私有 jar
// SekiroPackerEncoder/SekiroPacketDecoder 字节级兼容）。
//
// 帧格式（大端）：
//
//	magic(8B)=0x73656b69726f3031("sekiro01") | totalLen(4B) | type(1B) |
//	seq(4B) | headerCount(1B,<=127) | headers[kLen(1B)+key(UTF-8) |
//	vLen(1B)+value(UTF-8)] | payload(剩余字节)
package protocol

import (
	"sync/atomic"
	"time"
)

// Magic sekiro 协议魔数，字节序为大端的 "sekiro01"。
const Magic uint64 = 0x73656b69726f3031

// wireCommonHeaders 与 Constants$COMMON_HEADERS 对齐。
const (
	HeaderClientID    = "SEKIRO_CLIENT_ID"
	HeaderSekiroGroup = "SEKIRO_GROUP"

	HTTPContentType     = "Content-Type"
	HTTPContentTypeJSON = "application/json;charset=utf8;"
)

// wirePayloadHeaders 与 Constants$PAYLOAD_CONTENT_TYPE 对齐。
const (
	PayloadContentType        = "PAYLOAD_CONTENT_TYPE"
	ContentTypeSekiroFastJson = "CONTENT_TYPE_SEKIRO_FAST_JSON"
	SegmentStreamFlag         = "SEGMENT_STREAM_FLAG"
)

// 压缩协商头，与 Constants$COMPRESS 对齐。
const (
	CompressMethod = "COMPRESS_METHOD"
	CompressGzip   = "COMPRESS_METHOD_GZIP"
	CompressSnappy = "COMPRESS_METHOD_SNAPPY"
	CompressNone   = "COMPRESS_METHOD_NONE"
	// DoNotCompress 存在时跳过压缩（仅响应类包生效）。
	DoNotCompress = "__NOT_COMPRESS_FOR_SEKIRO_SEKIRO"
)

// 保留参数字，与 Constants$REVERSED_WORDS 对齐。
const (
	ParamGroup         = "group"
	ParamAction        = "action"
	ParamApiToken      = "sekiro_token"
	ParamBindClient    = "bind_client"
	ParamInvokeTimeout = "invoke_timeout"
	// ParamInvokeTimeoutAlias 兼容调用方历史拼写 `invoke_timeOut`（大写 O）。
	// 只作为回落：同时出现时以正确的 invoke_timeout 为准。
	ParamInvokeTimeoutAlias = "invoke_timeOut"
	ParamConstantInvoke     = "__sekiro_constant_key"
	ParamInvokeTraceID      = "__invoke_trace_id"
	ParamActionList         = "__action_list"
	ParamSystemTimeout      = "__sekiro_system_timeout"
	ParamResponse5xxJSON    = "__response_5xx_as_json"
	// WSSeqNumber WebSocket 通道的关联序号字段。
	WSSeqNumber = "__sekiro_seq__"
	// WSHeartbeatParam 注册时声明「支持应用层心跳」的 query 参数。
	//
	// 浏览器发不了 WS Ping（无此 API），provider 又是純静默端：链路被中间设备
	// 悄悄切断时，服务端 25s 内靠 Ping 判死，客户端却永远停在 readyState=1，
	// 业务侧只能看到设备"凭空消失"。声明该参数后服务端才下发能力通告并接受
	// 心跳帧——不声明则行为与旧版完全一致，老客户端零影响。
	WSHeartbeatParam = "hb"
	// WSServerHelloKey 能力通告帧的判别键。仅当注册带 hb=1 时下发一次。
	WSServerHelloKey = "__sekiro_hello__"
	// WSHeartbeatKey 应用层心跳帧（无 __sekiro_seq__）的判别键，服务端原样回显。
	WSHeartbeatKey = "__sekiro_hb__"
	// WSHeartbeatHintSeconds 能力通告里建议的心跳间隔（秒）。
	// 取值依据：服务端判死是「写空闲 20s + 等 pong 10s」= 30s，客户端按
	// 3 个周期判活，10s × 3 = 30s 与之对齐——避免出现「服务端已把设备移出
	// group、客户端还以为在线」的空窗。
	WSHeartbeatHintSeconds = 10
	// ResponseStatus 响应状态头（映射 HTTP status）。
	ResponseStatus = "__status__"
)

// 空闲与超时参数，与 Constants 对齐。
//
// 读/写空闲用原子存储而非 const/var：心跳与读空闲看护 goroutine 生命周期
// 超出单个测试（连接关闭后 goroutine 才退出），测试注入短周期时的恢复写入
// 会与遗留 goroutine 的读取并发——普通 var 在 -race 下报数据竞争。
// ticker 仅在 goroutine 启动时读一次；运行中的读取为对照值，读到注入值
// 或恢复值均无害。
var (
	serverWriteIdle atomic.Int64
	serverReadIdle  atomic.Int64
	serverPongWait  atomic.Int64
)

func init() {
	SetServerIdle(20*time.Second, 30*time.Second)
	SetServerPongTimeout(10 * time.Second)
}

// SetServerIdle 设置 WS/native 心跳的写空闲与读空闲周期（对齐 Java
// Constants 的 20s/30s）。供回归测试注入短周期；调用方需自行恢复。
func SetServerIdle(write, read time.Duration) {
	serverWriteIdle.Store(int64(write))
	serverReadIdle.Store(int64(read))
}

// SetServerPongTimeout 设置发出 Ping 后等待 Pong 的宽限。
//
// 取 10s 而非更短：Ping 的等待窗口里包含「等写锁 + 对端处理 + 网络往返」，
// 浏览器在页面繁忙（长任务、GC、后台节流）时回 Pong 可能延迟到秒级。
// 5s 窗口实测出现过对健康连接的误杀（判死后连接被强关，客户端只能重连），
// 而判死总时长由 写空闲 + 该值 决定：20s + 10s = 30s，仍是「分钟内发现」。
func SetServerPongTimeout(d time.Duration) { serverPongWait.Store(int64(d)) }

func ServerWriteIdle() time.Duration   { return time.Duration(serverWriteIdle.Load()) }
func ServerReadIdle() time.Duration    { return time.Duration(serverReadIdle.Load()) }
func ServerPongTimeout() time.Duration { return time.Duration(serverPongWait.Load()) }

const (
	DefaultInvokeTimeout = 30 * time.Second
)

// DefaultMaxFrameBytes 单帧/请求体默认上限 16MB。
//
// 业务侧大文本通常 <10MB，取 16MB 留约 60% 余量；原值 64MB（HTTP/WS 另设
// 32MB）是为未知峰值留的安全垫，实测只会抬高单帧可占用的内存峰值，收益有限。
// 若确实需要更大文本，用 SEKIRO_MAX_FRAME_MB 或 -max-frame-mb 调整，无需改代码。
// 用原子变量而非 const，使测试与启动配置都能注入；调用方一律走 MaxFrameBytes()。
const DefaultMaxFrameBytes = 16 << 20

var maxFrameBytes atomic.Int64

func init() { maxFrameBytes.Store(DefaultMaxFrameBytes) }

// SetMaxFrameBytes 设置单帧上限（字节）；非正值回退默认值。
func SetMaxFrameBytes(n int64) {
	if n <= 0 {
		n = DefaultMaxFrameBytes
	}
	maxFrameBytes.Store(n)
}

// MaxFrameBytes 当前单帧上限（字节）。
func MaxFrameBytes() int64 { return maxFrameBytes.Load() }
