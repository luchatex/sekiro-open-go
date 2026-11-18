package group

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// bind_client 定位失败的文案必须能区分三种成因，并指出设备的真实归属。
// 背景（2026-11-18 线上）：设备明明在线，却只报 "no device found"，
// 排查时只能人工比对 /op/api/clients 才看出它落在别的 group。

func TestBindMissPointsToOtherGroup(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	// 设备注册在 wenshuList，业务却在 wenshuapp 里定向调用
	g := m.GetOrCreate("wenshuList")
	g.register(NewClient(newFakeTransport(), g, "17623758743", slog.Default()))
	m.GetOrCreate("wenshuapp")
	waitActor()

	_, err := m.AllocateContext(context.Background(), "wenshuapp", "17623758743", "")
	if err == nil {
		t.Fatal("expected bind miss")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "no device found for clientId:17623758743") {
		t.Fatalf("前缀被破坏（调用方可能按前缀匹配）: %q", msg)
	}
	if !strings.Contains(msg, `device online in group "wenshuList"`) {
		t.Fatalf("未指出真实归属: %q", msg)
	}
	if strings.Contains(msg, "not found") {
		t.Fatalf("group 存在时不应报 group not found: %q", msg)
	}
}

func TestBindMissGroupMissing(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	g := m.GetOrCreate("ws_list")
	g.register(NewClient(newFakeTransport(), g, "client-x", slog.Default()))
	waitActor()

	_, err := m.AllocateContext(context.Background(), "no_such_group", "client-x", "")
	if err == nil {
		t.Fatal("expected bind miss")
	}
	msg := err.Error()
	if !strings.Contains(msg, `group "no_such_group" not found`) {
		t.Fatalf("未说明 group 不存在: %q", msg)
	}
	if !strings.Contains(msg, `device online in group "ws_list"`) {
		t.Fatalf("未指出真实归属: %q", msg)
	}
}

func TestBindMissDisconnected(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	g := m.GetOrCreate("g1")
	tr := newFakeTransport()
	c := NewClient(tr, g, "client-a", slog.Default())
	g.register(c)
	waitActor()

	// 连接已死但 unregister 尚未被 actor 处理：ring 里还留着一条 down 记录，
	// 这是线上最常见的「刚掉线就有人来调」窗口。
	tr.mu.Lock()
	tr.active = false
	tr.mu.Unlock()

	_, err := m.AllocateContext(context.Background(), "g1", "client-a", "")
	if err == nil {
		t.Fatal("expected bind miss")
	}
	if !strings.Contains(err.Error(), "device disconnected") {
		t.Fatalf("未说明设备已断线: %q", err.Error())
	}
	if strings.Contains(err.Error(), "(device disconnected)") {
		t.Fatalf("应带掉线时长: %q", err.Error())
	}
}

func TestBindMissAfterDisconnect(t *testing.T) {
	m := newTestManager()
	defer m.Close()

	g := m.GetOrCreate("g1")
	c := NewClient(newFakeTransport(), g, "client-d", slog.Default())
	g.register(c)
	waitActor()
	c.Disconnect() // 正常掉线：unregister 从 ring 删除
	waitActor()

	_, err := m.AllocateContext(context.Background(), "g1", "client-d", "")
	if err == nil {
		t.Fatal("expected bind miss")
	}
	if !strings.Contains(err.Error(), "device disconnected") {
		t.Fatalf("掉线后应提示刚断过线（旧实现此处只能报查无此设备）: %q", err.Error())
	}
}

// 掉线记录有 TTL：超过窗口后不应再提「刚掉线」，避免误导。
func TestBindMissDowntimeTTL(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	g := m.GetOrCreate("g1")

	g.post(func() {
		g.markDownLocked("stale")
		g.down["stale"] = time.Now().Add(-downTTL - time.Minute)
	})
	waitActor()

	_, err := m.AllocateContext(context.Background(), "g1", "stale", "")
	if err == nil {
		t.Fatal("expected bind miss")
	}
	if got, want := err.Error(), "no device found for clientId:stale"; got != want {
		t.Fatalf("超过 TTL 不应再提掉线: got %q want %q", got, want)
	}
}

func TestBindMissNoHintWhenNowhereOnline(t *testing.T) {
	m := newTestManager()
	defer m.Close()
	m.GetOrCreate("g1")

	_, err := m.AllocateContext(context.Background(), "g1", "ghost", "")
	if err == nil {
		t.Fatal("expected bind miss")
	}
	if got, want := err.Error(), "no device found for clientId:ghost"; got != want {
		t.Fatalf("无归属可提示时应保持原文案: got %q want %q", got, want)
	}
}
