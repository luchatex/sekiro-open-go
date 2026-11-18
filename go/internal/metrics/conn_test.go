package metrics

import "testing"

// 连接侧指标：注册成功/失败原因、断开原因与在线数都要进 /op/api/stats。
// 旧版只统计 invoke 侧，「客户端压根没连上」在监控里是空白。
func TestConnCountersInSnapshot(t *testing.T) {
	r := New()
	r.ConnRegisterOK()
	r.ConnRegisterOK()
	r.ConnRegisterFail("param-missing")
	r.ConnClosed("pong-timeout")
	r.ConnClosed("client-closed")
	r.ConnClosed("pong-timeout")

	snap := r.Snapshot(func(string) int { return 3 }, []string{"wenshuapp"})
	if snap.Conn == nil {
		t.Fatal("conn 字段缺失")
	}
	c := snap.Conn
	if c.OnlineNow != 3 {
		t.Fatalf("onlineNow = %d, want 3", c.OnlineNow)
	}
	if c.RegisterOK != 2 {
		t.Fatalf("registerOk = %d, want 2", c.RegisterOK)
	}
	if c.RegisterFail != 1 || c.FailReasons["param-missing"] != 1 {
		t.Fatalf("registerFail = %d %v, want 1 param-missing", c.RegisterFail, c.FailReasons)
	}
	if c.ClosedTotal != 3 {
		t.Fatalf("closedTotal = %d, want 3", c.ClosedTotal)
	}
	if c.CloseReasons["pong-timeout"] != 2 || c.CloseReasons["client-closed"] != 1 {
		t.Fatalf("closeReasons = %v", c.CloseReasons)
	}
}

// 空 reason 不应产生条目（防止调用方传空串把 map 打出无意义的键）。
func TestConnIgnoresEmptyReason(t *testing.T) {
	r := New()
	r.ConnRegisterFail("")
	r.ConnClosed("")
	snap := r.Snapshot(nil, nil)
	if snap.Conn.RegisterFail != 0 || snap.Conn.ClosedTotal != 0 {
		t.Fatalf("空 reason 被计数: %+v", snap.Conn)
	}
}

// nil 接收者安全（埋点侧无需判空，与 Recorder 其他方法约定一致）。
func TestConnNilSafety(t *testing.T) {
	var r *Recorder
	r.ConnRegisterOK()
	r.ConnRegisterFail("x")
	r.ConnClosed("y")
}
