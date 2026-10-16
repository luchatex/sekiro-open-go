package native

import (
	"time"

	"sekiro-go/internal/protocol"
)

// connActive 连接是否仍然可用。
func (c *Channel) connActive() bool {
	// net.Conn 无活性查询接口；以最近读时间 + 读空闲窗口近似判断，
	// 真正的下线由读循环错误路径触发 Disconnect 完成。
	deadline := time.Now().Add(-2 * protocol.ServerReadIdle())
	return time.Unix(0, c.lastRead.Load()).After(deadline)
}
