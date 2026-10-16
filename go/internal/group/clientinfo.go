package group

import (
	"time"
)

// 监控页「客户端节点」信息采集。
//
// 设计约束：不修改 Transport 核心接口（SendInvoke/Active/Close/CloseAfter），
// IP/通道类型/最后活跃用可选接口断言采集——真实实现（nativeTransport/
// wsTransport）按需实现，测试 fake 与未来实现不受影响。

// ClientInfo 单个 provider 节点的监控信息（对齐 /op/api/clients 输出）。
type ClientInfo struct {
	Group       string     `json:"group"`
	ClientID    string     `json:"clientId"`              // 设备标识（js-web 场景即设备名）
	IP          string     `json:"ip,omitempty"`          // 连接对端地址（transport 未实现则为空）
	Transport   string     `json:"transport,omitempty"`   // 通道类型：ws / native
	Compression string     `json:"compression,omitempty"` // 压缩协商状态：deflate / off（仅 ws 通道）
	ConnectedAt time.Time  `json:"connectedAt"`           // 本次连接建立时刻（重注册自动刷新）
	LastActive  *time.Time `json:"lastActive,omitempty"`  // 最近一次活跃（响应到达 / 心跳 pong）
	Online      bool       `json:"online"`                // 连接存活且未被下线
}

// PeerAddrProvider transport 可选：连接对端地址。
type PeerAddrProvider interface{ PeerAddr() string }

// TransportKindProvider transport 可选：通道类型标识（"ws" / "native"）。
type TransportKindProvider interface{ TransportKind() string }

// LastActiveProvider transport 可选：最近一次活跃时刻。
type LastActiveProvider interface{ LastActive() time.Time }

// CompressionProvider transport 可选：压缩协商状态（"deflate" / "off"）。
// native 通道为逐包 gzip 头驱动、无需协商，不实现本接口。
type CompressionProvider interface{ Compression() string }

// buildClientInfo 从 Client 及其 transport（可选接口）聚合节点信息。
func buildClientInfo(c *Client) ClientInfo {
	info := ClientInfo{
		Group:       c.GroupName,
		ClientID:    c.ID,
		Online:      c.Active() && !c.offline.Load(),
		ConnectedAt: time.Unix(0, c.connectedAt.Load()),
	}
	if pa, ok := c.tr.(PeerAddrProvider); ok {
		info.IP = pa.PeerAddr()
	}
	if k, ok := c.tr.(TransportKindProvider); ok {
		info.Transport = k.TransportKind()
	}
	if cp, ok := c.tr.(CompressionProvider); ok {
		info.Compression = cp.Compression()
	}
	if la, ok := c.tr.(LastActiveProvider); ok {
		if t := la.LastActive(); !t.IsZero() {
			info.LastActive = &t
		}
	}
	return info
}
