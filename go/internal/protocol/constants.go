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
	ParamGroup           = "group"
	ParamAction          = "action"
	ParamApiToken        = "sekiro_token"
	ParamBindClient      = "bind_client"
	ParamInvokeTimeout   = "invoke_timeout"
	ParamConstantInvoke  = "__sekiro_constant_key"
	ParamInvokeTraceID   = "__invoke_trace_id"
	ParamActionList      = "__action_list"
	ParamSystemTimeout   = "__sekiro_system_timeout"
	ParamResponse5xxJSON = "__response_5xx_as_json"
	// WSSeqNumber WebSocket 通道的关联序号字段。
	WSSeqNumber = "__sekiro_seq__"
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
)

func init() {
	SetServerIdle(20*time.Second, 30*time.Second)
}

// SetServerIdle 设置 WS/native 心跳的写空闲与读空闲周期（对齐 Java
// Constants 的 20s/30s）。供回归测试注入短周期；调用方需自行恢复。
func SetServerIdle(write, read time.Duration) {
	serverWriteIdle.Store(int64(write))
	serverReadIdle.Store(int64(read))
}

func ServerWriteIdle() time.Duration { return time.Duration(serverWriteIdle.Load()) }
func ServerReadIdle() time.Duration  { return time.Duration(serverReadIdle.Load()) }

const (
	DefaultInvokeTimeout = 15 * time.Second
)

// MaxFrameBytes 单帧上限 64MB（防护内存滥用；常规业务 <32MB）。
const MaxFrameBytes = 64 << 20
