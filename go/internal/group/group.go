package group

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"sekiro-go/internal/protocol"
)

var (
	// ErrPoolEmpty 空池失败（对齐 NettySekiroGroup.wrapWithEmptyDelay 文案）。
	ErrPoolEmpty = errors.New("pool queue empty")
	// ErrGroupMissing group 参数缺失。
	ErrGroupMissing = errors.New("the param {group} not presented")
)

// Group 单个 group 的调度核心（actor 模型）。
//
// Java 版注释：由于多线程操作客户端容器（rpc 入口、admin、netty 客户端线程），
// 容器状态容易不同步，因此用单线程事件循环避免资源状态问题。
type Group struct {
	name string
	mail chan func()
	log  *slog.Logger

	// 以下状态仅在 actor goroutine 内访问
	ring        map[uint64]*Client
	keys        []uint64 // 升序
	rotate      []*Client
	offlineMap  map[string]*Client
	emptyCount  int
	firstEmpty  time.Time
	janitorStop chan struct{}
	janitorOnce sync.Once
	actorStop   chan struct{}
	actorDone   chan struct{}
	actorOnce   sync.Once
}

func newGroup(name string, log *slog.Logger) *Group {
	g := &Group{
		name:        name,
		mail:        make(chan func(), 1024),
		log:         log.With("group", name),
		ring:        make(map[uint64]*Client),
		offlineMap:  make(map[string]*Client),
		janitorStop: make(chan struct{}),
		actorStop:   make(chan struct{}),
		actorDone:   make(chan struct{}),
	}
	go g.loop()
	// 对齐 scheduleWithRate(clientList.removeIf(isDown), 60min)
	go g.janitor()
	return g
}

func (g *Group) loop() {
	defer close(g.actorDone)
	for {
		select {
		case fn := <-g.mail:
			func() {
				defer func() {
					if r := recover(); r != nil {
						g.log.Error("group event loop panic", "panic", r)
					}
				}()
				fn()
			}()
		case <-g.actorStop:
			return
		}
	}
}

func (g *Group) post(fn func()) {
	g.postContext(context.Background(), fn)
}

// postContext 投递 actor 任务；调用方取消或 group 关闭后立即返回，
// 避免故障期邮箱满时无限堆积等待 goroutine。
func (g *Group) postContext(ctx context.Context, fn func()) {
	select {
	case g.mail <- fn:
	case <-ctx.Done():
	case <-g.actorStop:
	}
}

// janitor 每小时清理（对齐 scheduleWithRate(clientList.removeIf(isDown), 60min)）。
func (g *Group) janitor() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-g.janitorStop:
			return
		case <-t.C:
			g.post(func() {
				kept := g.rotate[:0]
				for _, c := range g.rotate {
					if !c.IsDown() {
						kept = append(kept, c)
					}
				}
				clear(g.rotate[len(kept):])
				if len(kept) == 0 {
					g.rotate = nil
				} else {
					g.rotate = kept
				}
			})
		}
	}
}

// removeRotate 从轮询切片移除指定 Client，并清空尾部指针槽。
// 仅清理 len 不够：残余 backing array 仍可能被 GC 视为存活引用。
func (g *Group) removeRotate(target *Client) {
	kept := g.rotate[:0]
	for _, c := range g.rotate {
		if c != target {
			kept = append(kept, c)
		}
	}
	clear(g.rotate[len(kept):])
	if len(kept) == 0 {
		g.rotate = nil
		return
	}
	g.rotate = kept
}

// ---- 注册/注销（对齐 registerNettyClient / unregisterNettyClient / offlineClient）----

// Register 注册客户端到该 group（actor 内执行）。
func (g *Group) Register(c *Client) {
	g.register(c)
}

func (g *Group) register(c *Client) {
	g.post(func() {
		c.connectedAt.Store(time.Now().UnixNano()) // 重注册=新连接顶替，时间随之刷新
		if old, ok := g.ring[c.Key]; ok && old != c {
			old.offline.Store(true)
			g.removeRotate(old) // 被顶替连接不再参与轮询，也不能被 rotate 永久引用
			if old.Active() {
				// oldLifetime：旧连接刚活多久就被顶替。重连风暴的特征值——
				// 周期性出现 ~30s 说明旧连接被服务端读空闲回收（旧版二进制
				// 指纹），~25s 说明对端不回 pong；正常的页面刷新则无规律
				g.log.Warn("duplicate client register",
					"old", old.ID, "new", c.ID,
					"oldLifetime", time.Since(time.Unix(0, old.connectedAt.Load())).Round(100*time.Millisecond).String())
				// BugFix（Java 注释记载）：旧连接延迟 30s 关闭，
				// 避免网络抖动重复注册时误杀新连接导致假死
				old.tr.CloseAfter(30 * time.Second)
			}
		}
		if _, exists := g.ring[c.Key]; !exists {
			g.addKey(c.Key)
		}
		g.ring[c.Key] = c
		g.removeRotate(c) // 同一实例重复注册也不产生重复轮询项
		if off, ok := g.offlineMap[c.ID]; ok && off != c {
			delete(g.offlineMap, c.ID) // 新连接已上线，清除旧代际离线记录
		}
		g.rotate = append(g.rotate, nil)
		copy(g.rotate[1:], g.rotate[:len(g.rotate)-1])
		g.rotate[0] = c // addFirst
	})
}

func (g *Group) unregister(c *Client) {
	g.post(func() {
		c.offline.Store(true)
		g.removeRotate(c)
		// offlineClient 会先从 ring 删除，旧实现因此在 !ok 处提前返回，
		// offlineMap 永远无法清理。离线记录按 Client 代际精确删除。
		if off, ok := g.offlineMap[c.ID]; ok && off == c {
			delete(g.offlineMap, c.ID)
		}
		old, ok := g.ring[c.Key]
		if !ok {
			g.log.Warn("unregister none exist client", "clientId", c.ID)
			return
		}
		// BugFix（Java 注释记载）：只有 channel 同一性成立才删除，
		// 防止重复注册场景把新连接误删导致假死
		if old == c {
			delete(g.ring, c.Key)
			g.removeKey(c.Key)
		}
	})
}

func (g *Group) offlineClient(c *Client) {
	g.post(func() {
		old, ok := g.ring[c.Key]
		if !ok {
			return
		}
		if old != c {
			return
		}
		if c.IsDown() {
			return
		}
		c.offline.Store(true)
		delete(g.ring, c.Key)
		g.removeKey(c.Key)
		g.removeRotate(c)
		// offlineMap 保留"已下线但未断开"的连接：bind_client 定向调用时
		// fetchByClientID 查不到会直接报错，保留可让业务侧拿到明确归属。
		// 注意：该表只在 unregister（连接断开）时移除，客户端下线后长期不
		// 断线会缓慢累积（与原 Java 版行为一致，非本重写引入）；
		// 历史经验值下每客户端单条记录，量级可控，暂不做额外 GC。
		g.offlineMap[c.ID] = c
	})
}

func (g *Group) removeKey(k uint64) {
	i := sort.Search(len(g.keys), func(i int) bool { return g.keys[i] >= k })
	if i < len(g.keys) && g.keys[i] == k {
		g.keys = append(g.keys[:i], g.keys[i+1:]...)
	}
}

func (g *Group) addKey(k uint64) {
	i := sort.Search(len(g.keys), func(i int) bool { return g.keys[i] >= k })
	if i < len(g.keys) && g.keys[i] == k {
		return
	}
	g.keys = append(g.keys, 0)
	copy(g.keys[i+1:], g.keys[i:])
	g.keys[i] = k
}

// ---- 分配（对齐 queueRotateAllocate / consistentAllocate / fetchByClientId）----

// allocate 在 actor 内计算并回填结果；空池走退避延时回填。
//
// 对齐 Java：rotate/consistent 分配失败经 wrapWithEmptyDelay（文案
// "pool queue empty"，含退避延时）；bind_client 查找失败不经退避，
// 立即返回 "no device found for clientId:x"（NettySekiroGroup.fetchByClientId）。
func (g *Group) allocate(ctx context.Context, bindClient, constantKey string, reply chan<- allocResult) {
	g.postContext(ctx, func() {
		switch {
		case bindClient != "":
			c := g.fetchByClientID(bindClient)
			if c == nil {
				replyAlloc(ctx, reply, allocResult{err: fmt.Errorf("no device found for clientId:%s", bindClient)})
				return
			}
			replyAlloc(ctx, reply, allocResult{client: c})
		case constantKey != "":
			c := g.consistentAllocate(protocol.MurmurHashString(constantKey))
			if c == nil {
				g.backoffFail(ctx, reply)
				return
			}
			g.emptyCount = 0
			replyAlloc(ctx, reply, allocResult{client: c})
		default:
			c := g.rotateAllocate()
			if c == nil {
				g.backoffFail(ctx, reply)
				return
			}
			g.emptyCount = 0
			replyAlloc(ctx, reply, allocResult{client: c})
		}
	})
}

// replyAlloc 回填分配结果；调用方已取消时不得再阻塞 actor。
func replyAlloc(ctx context.Context, reply chan<- allocResult, res allocResult) {
	select {
	case reply <- res:
	case <-ctx.Done():
	}
}

func (g *Group) fetchByClientID(id string) *Client {
	k := protocol.MurmurHashString(id)
	c, ok := g.ring[k]
	if !ok {
		g.log.Info("no device found for clientId", "clientId", id)
		return nil
	}
	if c.ID != id {
		g.log.Warn("hash conflict!! ？？", "clientId1", id, "clientId2", c.ID, "murHash", k)
		return nil
	}
	if !c.Active() || c.offline.Load() {
		delete(g.ring, k)
		g.removeKey(k)
		return nil
	}
	return c
}

// consistentAllocate 一致性哈希：≥hash 的第一个活节点，否则回卷到头部。
func (g *Group) consistentAllocate(hash uint64) *Client {
	idx := sort.Search(len(g.keys), func(i int) bool { return g.keys[i] >= hash })
	if c := g.scanFrom(idx); c != nil {
		return c
	}
	if idx > 0 {
		return g.scanFrom(0)
	}
	return nil
}

// scanFrom 从 keys[start] 起找第一个活节点；惰性删除挂死节点后原地重试。
func (g *Group) scanFrom(start int) *Client {
	for start < len(g.keys) {
		k := g.keys[start]
		c := g.ring[k]
		if c == nil || c.IsDown() {
			delete(g.ring, k)
			g.removeKey(k)
			continue
		}
		return c
	}
	return nil
}

func (g *Group) rotateAllocate() *Client {
	for len(g.rotate) > 0 {
		c := g.rotate[0]
		g.rotate[0] = nil // 清空已弹出的指针槽
		g.rotate = g.rotate[1:]
		if c.IsDown() {
			continue
		}
		if c.offline.Load() {
			g.offlineMap[c.ID] = c
			continue
		}
		g.rotate = append(g.rotate, c) // addLast（轮询回填）
		return c
	}
	g.rotate = nil
	return nil
}

// backoffFail 对齐 wrapWithEmptyDelay（含死代码分支修复）：
//   - 空池次数 ≤5：立即失败
//   - 6–10 次：延时 (n-4)s
//   - >10 次：封顶 10s（次数 <100）
//   - 空池持续 >10min 且次数 >100：15s；>30min：20s
//     （原代码 20s 分支与 15s 条件重复永不触发，此处按注释意图修复）
//
// 机理：下游宕机时若快速返回空池，上游数千线程伪死循环会打满 CPU
// （Java 注释：2000 并发 CPU 占用 70%），用延时把 QPS 压回合理水位。
func (g *Group) backoffFail(ctx context.Context, reply chan<- allocResult) {
	if g.emptyCount == 0 {
		g.firstEmpty = time.Now()
	}
	g.emptyCount++
	n := g.emptyCount
	g.log.Info("pool queue empty", "count", n)
	if n <= 5 {
		replyAlloc(ctx, reply, allocResult{err: ErrPoolEmpty})
		return
	}
	delay := time.Duration(n-4) * time.Second
	if delay > 10*time.Second && n < 100 {
		delay = 10 * time.Second
	}
	empty := time.Since(g.firstEmpty)
	if n > 100 && empty > 30*time.Minute {
		delay = 20 * time.Second
	} else if n > 100 && empty > 10*time.Minute {
		delay = 15 * time.Second
	} else if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	timer := time.AfterFunc(delay, func() {
		replyAlloc(ctx, reply, allocResult{err: ErrPoolEmpty})
	})
	if ctx.Done() != nil {
		// 调用方断开后立即释放退避定时器，而不是继续空等到 2–20s。
		context.AfterFunc(ctx, func() { timer.Stop() })
	}
}

type allocResult struct {
	client *Client
	err    error
}

// ---- 查询 ----

func (g *Group) enabledClientIDs(reply chan<- []string) {
	g.post(func() {
		ids := make([]string, 0, len(g.rotate))
		for _, c := range g.rotate {
			if c.Active() && !c.offline.Load() {
				ids = append(ids, c.ID)
			}
		}
		reply <- ids
	})
}

// clientDetails 节点监控信息（actor 内遍历，保证与注册/注销串行化）。
func (g *Group) clientDetails(reply chan<- []ClientInfo) {
	g.post(func() {
		infos := make([]ClientInfo, 0, len(g.ring))
		for _, c := range g.ring {
			infos = append(infos, buildClientInfo(c))
		}
		reply <- infos
	})
}

// kick 断开指定 clientId 的活跃连接（监控页踢下线）。
// 与 MarkOffline（业务下线，连接保留）不同：直接关闭 transport，
// 连接关闭触发 Disconnect → 彻底注销 + pending 失败。
func (g *Group) kick(clientID string, reply chan<- *Client) {
	g.post(func() {
		c := g.fetchByClientID(clientID)
		if c == nil {
			reply <- nil
			return
		}
		g.log.Warn("client kicked via monitor", "clientId", clientID)
		// 实际 Close 由 actor 外的 Manager 执行：native Close 会同步回调
		// Disconnect 并再次 post 当前 group，actor 内执行可能自死锁。
		reply <- c
	})
}

func (g *Group) close() {
	g.actorOnce.Do(func() {
		g.janitorOnce.Do(func() { close(g.janitorStop) })
		close(g.actorStop)
	})
	<-g.actorDone
}
