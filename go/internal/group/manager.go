package group

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// Manager 全部 group 的注册表（对齐 NettySekiroGroup.sGroup 静态表）。
type Manager struct {
	mu     sync.Mutex
	groups map[string]*Group
	log    *slog.Logger
}

func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{groups: make(map[string]*Group), log: log}
}

// GetOrCreate 对齐 createOrGet（双重检查锁语义由 mutex 承担）。
func (m *Manager) GetOrCreate(name string) *Group {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.groups[name]; ok {
		return g
	}
	g := newGroup(name, m.log)
	m.groups[name] = g
	return g
}

// get 只读查找；调用/查询未知 group 不得创建 actor 与 janitor。
func (m *Manager) get(name string) *Group {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.groups[name]
}

// Names 已有 group 名（排序，对齐 groupList 的 TreeSet 语义）。
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.groups))
	for k := range m.groups {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Allocate 同步分配一个客户端：
// bindClient 定向 > constantKey 一致性哈希 > 轮询（对齐 accessAndAllocate）。
// 空池时按退避策略延时返回 ErrPoolEmpty（"pool queue empty"）。
//
// 未知 group 直接按空池失败，不创建永久 group/goroutine。
func (m *Manager) Allocate(groupName, bindClient, constantKey string) (*Client, error) {
	return m.AllocateContext(context.Background(), groupName, bindClient, constantKey)
}

// AllocateContext 与 Allocate 相同，但等待 actor、空池退避时响应 ctx 取消。
func (m *Manager) AllocateContext(ctx context.Context, groupName, bindClient, constantKey string) (*Client, error) {
	if groupName == "" {
		return nil, ErrGroupMissing
	}
	g := m.get(groupName)
	if g == nil {
		if bindClient != "" {
			return nil, fmt.Errorf("no device found for clientId:%s", bindClient)
		}
		return nil, ErrPoolEmpty
	}
	reply := make(chan allocResult, 1)
	g.allocate(ctx, bindClient, constantKey, reply)
	select {
	case r := <-reply:
		if r.err != nil {
			return nil, r.err
		}
		return r.client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// EnabledClientIDs group 内活跃客户端 ID 列表（clientQueue 接口）。
func (m *Manager) EnabledClientIDs(groupName string) []string {
	g := m.get(groupName)
	if g == nil {
		return []string{}
	}
	reply := make(chan []string, 1)
	g.enabledClientIDs(reply)
	return <-reply
}

// ClientDetails 全部 group 的 provider 节点监控信息（/op/api/clients）。
func (m *Manager) ClientDetails() []ClientInfo {
	infos := make([]ClientInfo, 0, 16)
	for _, name := range m.Names() {
		g := m.get(name)
		if g == nil {
			continue
		}
		reply := make(chan []ClientInfo, 1)
		g.clientDetails(reply)
		infos = append(infos, <-reply...)
	}
	return infos
}

// KickClient 断开指定 group/clientId 的活跃连接（监控页踢下线）。
// 返回是否踢到（false = 该 client 不在线）。
func (m *Manager) KickClient(groupName, clientID string) bool {
	g := m.get(groupName)
	if g == nil {
		return false
	}
	reply := make(chan *Client, 1)
	g.kick(clientID, reply)
	c := <-reply
	if c == nil {
		return false
	}
	// actor 外关闭：Close → Disconnect → group.post，避免 actor 自死锁。
	c.tr.Close()
	return true
}

// Close 停止所有 group actor（测试/关停用）。
func (m *Manager) Close() {
	m.mu.Lock()
	groups := m.groups
	m.groups = make(map[string]*Group)
	m.mu.Unlock()
	for _, g := range groups {
		g.close()
	}
}
