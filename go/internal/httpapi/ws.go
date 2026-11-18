package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"sekiro-go/internal/group"
	"sekiro-go/internal/metrics"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// wsCloseBadRequest 应用级 WS 关闭码（RFC 6455 应用段 3000–4999）。
// 原 Java 版以 HTTP 风格 400 表达帧/参数错误，但 400 不是合法 WS 关闭码：
// coder/websocket 发送前校验 validWireCloseCode，非法码直接报错、close
// 帧根本发不出去（连接只能等读空闲 30s 强关）。重写版统一映射到 4400。
const wsCloseBadRequest = 4400

// wsTransport WebSocket provider 传输通道。
type wsTransport struct {
	conn *websocket.Conn
	peer string // 握手请求的对端地址（websocket.Conn 不暴露 RemoteAddr）
	// compressed 握手时客户端是否声明 permessage-deflate。服务端 Accept
	// 固定启用 ContextTakeover，声明即协商成功；未声明 = 压缩静默关闭
	// （典型成因：前置 nginx 剥了 Sec-WebSocket-Extensions），大文本
	// 场景带宽直接放大 10 倍——必须可观测
	compressed bool

	writeMu    sync.Mutex
	dead       atomic.Bool
	deadCh     chan struct{}
	deadOne    sync.Once
	lastActive atomic.Int64           // 最近活跃（数据帧到达或心跳 pong），监控节点信息用
	closedBy   atomic.Pointer[string] // 服务端主动关闭原因（空 = 由读循环发现断开）
}

func (t *wsTransport) markDead() {
	t.dead.Store(true)
	t.deadOne.Do(func() { close(t.deadCh) })
}

func (t *wsTransport) SendInvoke(params *orderedjson.Map, seq int32) error {
	m := orderedjson.New()
	m.SetAll(params)
	m.Set(protocol.WSSeqNumber, json.Number(strconv.FormatInt(int64(seq), 10)))
	// MarshalJSONSorted：对齐原版 invoke 参数下发经 fastjson toJSONString
	// （默认 SortField → 字典序，__sekiro_seq__ 排在首位）
	body, err := m.MarshalJSONSorted()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.Write(ctx, websocket.MessageText, body)
}

func (t *wsTransport) Active() bool {
	select {
	case <-t.deadCh:
		return false
	default:
		return true
	}
}

func (t *wsTransport) Close() {
	t.markDead()
	t.conn.Close(websocket.StatusNormalClosure, "")
}

func (t *wsTransport) CloseAfter(d time.Duration) {
	time.AfterFunc(d, func() {
		t.markDead()
		t.conn.Close(wsCloseBadRequest, "duplicate client register")
	})
}

func (t *wsTransport) ping(ctx context.Context) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.Ping(ctx)
}

func (t *wsTransport) read(ctx context.Context) (websocket.MessageType, []byte, error) {
	mt, data, err := t.conn.Read(ctx)
	if err != nil {
		t.markDead()
	} else {
		t.lastActive.Store(time.Now().UnixNano())
	}
	return mt, data, err
}

// 监控节点信息采集（可选接口，见 group/clientinfo.go）。
func (t *wsTransport) PeerAddr() string      { return t.peer }
func (t *wsTransport) TransportKind() string { return "ws" }
func (t *wsTransport) LastActive() time.Time {
	return time.Unix(0, t.lastActive.Load())
}

// Compression 压缩协商状态："deflate"（协商成功）/ "off"（客户端未声明）。
func (t *wsTransport) Compression() string {
	if t.compressed {
		return "deflate"
	}
	return "off"
}

func (t *wsTransport) closeNow() {
	t.markDead()
	t.conn.CloseNow()
}

func (t *wsTransport) closeWith(code int, reason string) {
	t.markDead()
	t.conn.Close(websocket.StatusCode(code), reason)
}

// markClosedBy 记录「本连接是服务端主动关的」及其原因。
//
// 用途：读循环在连接被看护关闭后也会收到一次读错误，若不区分就会把同一条
// 连接重复计一次断线（实测 pong-timeout 会同时产生一条 read-error）。
// 只有「读循环先发现」的断开才真正代表对端/链路异常。
func (t *wsTransport) markClosedBy(reason string) { t.closedBy.Store(&reason) }

func (t *wsTransport) closedReason() string {
	if p := t.closedBy.Load(); p != nil {
		return *p
	}
	return ""
}

// wsRegister 处理 WebSocket provider 注册（对齐 ServiceHttp.doWsClientRegister +
// ServiceWsClient + WsHeartBeatHandler）。
//
// D12 粘包/乱码修复：使用 coder/websocket 的 conn.Read 高层 API，分片
// continuation 帧在字节层重组完成后才交付，多字节 UTF-8 字符跨分片不再产生
// U+FFFD 乱码（Java 版逐帧 text() 解码的缺陷在重写中被消除）。
func wsRegister(w http.ResponseWriter, r *http.Request, mgr *group.Manager, log *slog.Logger) {
	q := r.URL.Query()
	gName := q.Get("group")
	clientID := q.Get("clientId")
	if gName == "" || clientID == "" {
		// 埋点：参数缺失是最常见的一类「连不上」，旧版只在响应体里提示，
		// 监控页看不到——这里计入连接侧失败原因
		metrics.Default().ConnRegisterFail("param-missing")
		httpResponseText(w, http.StatusBadRequest,
			"{group} or {clientId} can not be empty!! demo url: ws://host:5612/register?group=ws-group&clientId=testClient")
		return
	}

	conn, err := wsAccept(w, r)
	if err != nil {
		metrics.Default().ConnRegisterFail("accept-error")
		return
	}
	conn.SetReadLimit(1 << 25) // 32MB，对齐聚合器上限

	g := mgr.GetOrCreate(gName)
	// 压缩协商可观测：客户端声明 permessage-deflate 即协商成功（Accept
	// 固定 ContextTakeover）；未声明通常是前置代理剥了扩展头
	compressed := strings.Contains(r.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate")
	tr := &wsTransport{conn: conn, peer: r.RemoteAddr, compressed: compressed, deadCh: make(chan struct{})}
	cl := group.NewClient(tr, g, clientID, log)
	g.Register(cl)
	metrics.Default().ConnRegisterOK()
	log.Info("ws client registered", "group", gName, "clientId", clientID,
		"compression", tr.Compression())
	wsServe(cl, tr, log)
}

func wsAccept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	// 启用 permessage-deflate（RFC 7692）压缩：
	//   - 仅在客户端同样声明支持时协商生效（标准扩展协商），Safari/Node undici
	//     等不支持的端自动降级为不压缩，零兼容性风险
	//   - ContextTakeover：跨消息复用压缩字典。sekiro 场景同一连接反复传输
	//     结构高度相似的 JSON（请求/响应骨架固定），实测比 NoContextTakeover
	//     再省约 80% 传输量；js-web 场景每浏览器连接数少，字典内存开销（每端
	//     约 32KB）可忽略
	//   - 中文 JSON 文本实测压缩比约 10x+（真实混合文本 gzip 11.7x 基线）
	// OriginPatterns "*"：对齐 Java 原版（ServiceWsClient 不做跨源校验）。
	// coder/websocket 默认拒绝一切非同源 Origin（回 403），会让 https 页面
	// 经 wss://127.0.0.1 注册 provider 时握手失败——sekiro 的 provider 本就
	// 设计为从任意业务页面注册，跨源是正常使用场景而非攻击面。
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
		OriginPatterns:  []string{"*"},
	})
}

// wsServe WS 通道运行：读循环 + 心跳 + 空闲看护。
func wsServe(cl *group.Client, tr *wsTransport, log *slog.Logger) {
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())
	start := time.Now()
	lifetime := func() string { return time.Since(start).Round(100 * time.Millisecond).String() }
	stop := make(chan struct{})
	defer close(stop)

	// 连接侧埋点：本连接终止的唯一一次计数（看护 goroutine / 读循环互斥，
	// 先 close 的一方既写日志也计数，另一方走「善后」分支不重复计）
	closed := func(reason string) { metrics.Default().ConnClosed(reason) }

	// 心跳：写空闲 20s 发 Ping（对齐 WsHeartBeatHandler WRITER_IDLE）。
	//
	// 关键：Ping 成功（收到对应 pong）必须刷新 lastRead。
	// Java 版靠 Netty IdleStateHandler 在 channelRead 时重置计时，而
	// PongWebSocketFrame 也走 channelRead，故静默 provider 靠 pong 保活；
	// Go 版 coder/websocket 在 Read 内部自动回 ping/消费 pong，但不会
	// 解除 Read 阻塞——若 lastRead 只按数据帧更新，静默连接（js-web
	// provider 注册后无业务）30s 后必被读空闲看护误杀，前端表现为
	// 频繁重连循环。Ping 失败（5s 无 pong）= 连接已死，立即关闭。
	go func() {
		tk := time.NewTicker(protocol.ServerWriteIdle())
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if !tr.Active() {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := tr.ping(ctx)
				cancel()
				if err != nil {
					// Info 级：线上排查「断开重连循环」的关键死因日志，
					// 重连周期 ≈ 写空闲+5s 即说明对端不回 pong（代理剥 ping 等）
					log.Info("ws provider closed",
						"reason", "pong-timeout", "clientId", cl.ID,
						"lifetime", lifetime(), "err", err)
					tr.markClosedBy("pong-timeout")
					closed("pong-timeout")
					tr.closeNow()
					return
				}
				lastRead.Store(time.Now().UnixNano())
				tr.lastActive.Store(time.Now().UnixNano()) // 心跳 pong 亦算活跃
			}
		}
	}()
	// 读空闲 30s 关闭（对齐 READER_IDLE）。
	// 检查间隔取 min(ReadIdle/4, 5s)：生产 30s → 5s（与原硬编码一致），
	// 测试注入短 ReadIdle 时按比例缩短，否则看护在测试窗口内检查不到。
	go func() {
		interval := protocol.ServerReadIdle() / 4
		if interval > 5*time.Second {
			interval = 5 * time.Second
		}
		if interval <= 0 {
			interval = time.Second
		}
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if time.Since(time.Unix(0, lastRead.Load())) > protocol.ServerReadIdle() {
					// Info 级：此前完全无日志，重连排查时看不到这条死因。
					// 新版心跳下静默 provider 靠 pong 保活，正常不应触发；
					// 若出现即 lastRead 从未刷新（如连接建立后 20s 内首个
					// ping 未及发出即死）或心跳 goroutine 未生效
					log.Info("ws provider closed",
						"reason", "read-idle", "clientId", cl.ID,
						"lifetime", lifetime())
					tr.markClosedBy("read-idle")
					closed("read-idle")
					tr.closeNow()
					return
				}
			}
		}
	}()

	ctx := context.Background()
	for {
		msgType, data, err := tr.read(ctx)
		if err != nil {
			if by := tr.closedReason(); by != "" {
				// 看护已主动关闭并计过数（pong-timeout/read-idle/bad-*），
				// 这里的读错误只是善后，不能重复计入断线统计
				log.Debug("ws client closed (reclaimed)", "reason", by, "err", err)
			} else {
				// 读循环先发现的断开：客户端关闭/网络中断/中间设备切断。
				// Info 级 + 存活时长，供重连周期分析
				reason := classifyReadErr(err)
				log.Info("ws client disconnected",
					"reason", reason, "clientId", cl.ID,
					"lifetime", lifetime(), "err", err)
				closed(reason)
			}
			cl.Disconnect()
			return
		}
		lastRead.Store(time.Now().UnixNano())
		if msgType != websocket.MessageText {
			log.Warn("error ws request: non-text frame")
			closed("bad-frame")
			tr.closeWith(wsCloseBadRequest, "error ws request: non-text frame")
			cl.Disconnect()
			return
		}
		if !wsHandleMessage(cl, tr, data, log) {
			return
		}
	}
}

// classifyReadErr 把读错误归成稳定短标签，供日志与 /op 连接侧统计使用。
// 区分「对端正常关闭」与「链路异常」：前者不该计入错误率，后者要能一眼看出。
func classifyReadErr(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		// 1000 正常关闭；1001 对端离开；1005 收到空 close 帧（浏览器
		// socket.close() 不带码、undici 等实现也走这条）——都算客户端主动关闭，
		// 不该计入链路异常。
		case websocket.StatusNormalClosure, websocket.StatusGoingAway,
			websocket.StatusNoStatusRcvd:
			return "client-closed"
		}
		return "close-" + strconv.Itoa(int(ce.Code))
	}
	if errors.Is(err, context.Canceled) {
		return "client-closed"
	}
	return "read-error"
}

// wsHandleMessage 对齐 ServiceWsClient.channelRead0：
// 解析完整 JSON（分片已在字节层重组，D12）→ 取 __sekiro_seq__ → 构造响应包路由。
// 返回 false 表示连接已终止。
func wsHandleMessage(cl *group.Client, tr *wsTransport, data []byte, log *slog.Logger) bool {
	msg, err := orderedjson.Parse(data)
	if err != nil {
		log.Warn("ws frame json parse failed", "err", err)
		metrics.Default().ConnClosed("bad-json")
		tr.closeWith(wsCloseBadRequest, "error ws request: invalid json")
		cl.Disconnect()
		return false
	}
	seq := msg.GetInt(protocol.WSSeqNumber)
	if seq <= 0 {
		log.Warn("serial number not set for client!!")
		metrics.Default().ConnClosed("bad-seq")
		tr.closeWith(wsCloseBadRequest, "serial number not set for client!!")
		cl.Disconnect()
		return false
	}
	msg.Remove(protocol.WSSeqNumber)

	// 序号已不在 pending（超时/断连已终结该调用）：跳过重新序列化直接丢弃。
	// 大响应（详情页 MB 级）在迟到场景下重新 marshal 一遍毫无意义。
	if !cl.Pending(int32(seq)) {
		log.Debug("ws response dropped before marshal (invoke already finished)", "seq", seq)
		cl.DropLateResponse(int32(seq))
		return true
	}

	packet := protocol.NewPacket(protocol.STypeInvokeResponse)
	packet.Seq = int32(seq)
	packet.AddHeader(protocol.PayloadContentType, "application/json;charset=utf8")
	packet.AddHeader(protocol.ResponseStatus, strconv.Itoa(msg.GetInt("status")))
	body, _ := msg.MarshalJSON()
	packet.Data = body

	cl.HandleResponse(packet)
	return true
}
