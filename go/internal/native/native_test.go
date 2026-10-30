package native

import (
	"net"
	"testing"
	"time"

	"sekiro-go/internal/group"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

type testSink struct{ closed chan struct{} }

func (s *testSink) RespondCommon(int, string, string, int32) {}
func (s *testSink) RespondPacket(*protocol.Packet)           {}
func (s *testSink) Closed() <-chan struct{}                  { return s.closed }

func TestAbortPendingInvokesClearsRecords(t *testing.T) {
	ch := &Channel{}
	rec := group.NewRecord(orderedjson.New(), &testSink{closed: make(chan struct{})}, 7, time.Hour)
	if !ch.trackInvoke(7, rec) {
		t.Fatal("首次登记 invoker 调用失败")
	}
	if ch.trackInvoke(7, rec) {
		t.Fatal("重复 seq 不应登记成功")
	}

	ch.abortPendingInvokes()
	if got := len(ch.invPending); got != 0 {
		t.Fatalf("断开后 invoker pending = %d, want 0", got)
	}
	if rec.Abort() {
		t.Fatal("invoker 断开后未取消未完成调用")
	}
}

func TestWriteFailureImmediatelyMarksChannelDead(t *testing.T) {
	server, peer := net.Pipe()
	peer.Close()
	ch := &Channel{conn: server, stopHB: make(chan struct{})}
	ch.lastRead.Store(time.Now().UnixNano())

	if err := ch.writePacket(protocol.NewPacket(protocol.TypeHeartbeat)); err == nil {
		t.Fatal("向已关闭连接写入应返回错误")
	}
	if ch.connActive() {
		t.Fatal("写失败后连接仍被判定为 Active")
	}
}
