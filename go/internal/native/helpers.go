package native

import (
	"time"

	"sekiro-go/internal/protocol"
)

// connActive 连接是否仍然可用。
func (c *Channel) connActive() bool {
	if c.dead.Load() {
		return false
	}
	// net.Conn 无活性查询接口；以最近读时间 + 读空闲窗口辅助判断，
	// 但关闭/写失败后的 dead 才是不可逆权威状态。
	deadline := time.Now().Add(-2 * protocol.ServerReadIdle())
	return time.Unix(0, c.lastRead.Load()).After(deadline)
}
