// Package httpapi 实现 sekiro HTTP API 与 WebSocket 注册通道。
//
// 路由（大小写不敏感，对齐 ServiceHttp/HttpFirstPacketHandler）：
//   - /business/invoke、/business-demo/invoke
//   - /business/groupList、/business-demo/groupList
//   - /business/clientQueue、/business-demo/clientQueue
//   - WebSocket 注册：/register、/business/register、/business-demo/register
//   - 其他路径：404（原 Java 版反向代理 iinti.cn 文档站的能力已按需求剔除）
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/metrics"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

const (
	contentTypeJSON = "application/json"
	contentTypeForm = "application/x-www-form-urlencoded"
	maxBodyBytes    = 1 << 25 // 32MB，对齐 HttpObjectAggregator
)

// Handler 返回 HTTP API 处理器。
//
// opEnabled 控制 /op 监控页面与 /op/api/stats 是否可用（config.OpDisabled）。
func Handler(mgr *group.Manager, comp *compress.Manager, log *slog.Logger, opEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if opEnabled {
			if isOpStatsPath(path) {
				handleOpStats(w, mgr)
				return
			}
			if isOpClientsPath(path) {
				handleOpClients(w, mgr)
				return
			}
			if isOpErrorsPath(path) {
				handleOpErrors(w)
				return
			}
			if isOpKickPath(path) {
				handleOpKick(w, r, mgr, log)
				return
			}
			if isOpPath(path) {
				handleOpPage(w)
				return
			}
		}
		if isWebSocketUpgrade(r) {
			switch {
			case strings.EqualFold(path, "/register"),
				strings.EqualFold(path, "/business/register"),
				strings.EqualFold(path, "/business-demo/register"):
				wsRegister(w, r, mgr, log)
			default:
				httpResponseText(w, http.StatusBadRequest, "error websocket url:"+path)
			}
			return
		}
		switch {
		case strings.EqualFold(path, "/business/invoke"),
			strings.EqualFold(path, "/business-demo/invoke"),
			strings.EqualFold(path, "/business/groupList"),
			strings.EqualFold(path, "/business-demo/groupList"),
			strings.EqualFold(path, "/business/clientQueue"),
			strings.EqualFold(path, "/business-demo/clientQueue"):
			// sekiro API 路径：挂内容压缩（对齐 HttpFirstPacketHandler 中的
			// HttpContentCompressor）；WS 升级与 404 路径不压缩
			cw := newCompressWriter(w, negotiateEncoding(r.Header.Get("Accept-Encoding")))
			defer cw.Close()
			w = cw
		}
		switch {
		case strings.EqualFold(path, "/business/invoke"),
			strings.EqualFold(path, "/business-demo/invoke"):
			handleInvoke(w, r, mgr, log)
		case strings.EqualFold(path, "/business/groupList"),
			strings.EqualFold(path, "/business-demo/groupList"):
			handleGroupList(w, mgr)
		case strings.EqualFold(path, "/business/clientQueue"),
			strings.EqualFold(path, "/business-demo/clientQueue"):
			handleClientQueue(w, r, mgr)
		default:
			// 原 Java 版此处反向代理到 iinti.cn 文档站（已按需求剔除）
			httpResponseText(w, http.StatusNotFound,
				"sekiro framework(go): not found. docs: https://sekiro.iinti.cn/sekiro-doc/")
		}
	})
}

// ---- invoke ----

func handleInvoke(w http.ResponseWriter, r *http.Request, mgr *group.Manager, log *slog.Logger) {
	// Content-Type 校验（对齐 ServiceHttp.handleHttpRequest）
	ctHeader := r.Header.Get("Content-Type")
	if ctHeader == "" && r.Method != http.MethodGet {
		httpResponseText(w, http.StatusBadRequest, "sekiro framework only support contentType:application/x-www-form-urlencoded | application/json")
		return
	}
	mimeType := ""
	if ctHeader != "" {
		mimeType = strings.ToLower(strings.TrimSpace(strings.SplitN(ctHeader, ";", 2)[0]))
		if mimeType != contentTypeForm && mimeType != contentTypeJSON {
			httpResponseText(w, http.StatusBadRequest,
				"sekiro framework only support contentType:application/x-www-form-urlencoded | application/json, now is: "+mimeType)
			return
		}
	}

	params, bodyLen, err := buildParams(w, r, mimeType)
	if err != nil {
		// body 读取超时单独告警：这是「客户端发了首部却没发完 body」，
		// 属挂死连接而非普通请求错误，是最需要被发现的故障模式。
		if isTimeout(err) {
			log.Warn("read request body timeout (incomplete request)",
				"timeout", bodyReadTimeout, "declaredLength", r.ContentLength, "peer", r.RemoteAddr)
			httpResponseText(w, http.StatusRequestTimeout,
				"read request body timeout after "+bodyReadTimeout.String())
			return
		}
		httpResponseText(w, http.StatusBadRequest, "read request body failed: "+err.Error())
		return
	}
	logInvokeParams(log, params)

	// 监控埋点：入口记总量与请求流量（query + body 字节）
	mtr := metrics.Default()
	sink := &httpSink{w: w, closed: make(chan struct{})}
	timeout := timeFromMillis(params.GetInt(protocol.ParamInvokeTimeout))
	rec := group.NewRecord(params, sink, -1, timeout)
	rec.Meter = mtr
	mtr.Invoke(
		params.GetString(protocol.ParamGroup),
		params.GetString(protocol.ParamAction),
		int64(len(r.URL.RawQuery))+int64(bodyLen),
	)

	client, err := mgr.Allocate(
		params.GetString(protocol.ParamGroup),
		params.GetString(protocol.ParamBindClient),
		params.GetString(protocol.ParamConstantInvoke),
	)
	if err != nil {
		rec.Fail(err.Error(), "")
	} else {
		client.ForwardInvoke(rec)
	}
	// 等待响应终结：正常响应 / 分段结束 / 错误 / 超时，
	// 以及「消费方主动断开」——后者的信号是 r.Context().Done()。
	//
	// 不监听它的话，客户端断连后本goroutine 会驻留到 Record 超时
	// （默认 15s），pending 也同步挂到超时。调用方超时设得较长
	// （30s/60s）或批量超时时，堆积量可观。
	select {
	case <-sink.Closed():
	case <-r.Context().Done():
		rec.Abort()
		log.Debug("invoke consumer disconnected",
			"group", params.GetString(protocol.ParamGroup),
			"action", params.GetString(protocol.ParamAction),
			"peer", r.RemoteAddr)
	}
}

// maxLogParamsLen 单条日志中 params 字段的字节上限（防止超大 body 放大日志）。
const maxLogParamsLen = 2048

// logInvokeParams 调用参数日志：
//   - Debug 级（--debug / SEKIRO_LOCAL_DEBUG 打开）：完整参数（超长截断）
//   - Info 级（默认生产）：仅 group/action，业务参数不入日志——
//     避免 secret/token 等敏感参数落盘，也避免高 QPS 下每条请求放大日志
func logInvokeParams(log *slog.Logger, params *orderedjson.Map) {
	if log.Enabled(context.Background(), slog.LevelDebug) {
		log.Debug("sekiro invoke request", "params", truncate(params.String(), maxLogParamsLen))
		return
	}
	log.Info("sekiro invoke request",
		"group", params.GetString(protocol.ParamGroup),
		"action", params.GetString(protocol.ParamAction))
}

// truncate 限制字符串长度，尾部标注省略字节数。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "...(omitted " + strconv.Itoa(len(s)-limit) + " bytes)"
}

// bodyReadTimeout 读取请求 body 的上限。
//
// 为何需要：http.Server 只设了 ReadHeaderTimeout（仅覆盖请求首部），
// ReadTimeout 未设——若全局设置会误杀 15s 长轮询的响应期。body 读取发生在
// NewRecord 之前，客户端的 invoke_timeout 参数此刻尚未解析，对它无约束。
// 于是「发完首部就静默」的连接会永久阻塞在 io.ReadAll，占住 1 个 goroutine
// 与 1 个 fd直到客户端断开或服务重启，且日志无任何痕迹（故障放大器）。
//
// 取 60s：正常客户端毫秒级发完，此值只拦「挂死」不拦「慢」；
// 远大于业务侧常见的请求超时，也大于任何合理的 body 上传耗时。
var bodyReadTimeout = 60 * time.Second

// readBody 读取请求 body，期间施加 bodyReadTimeout 读截止时间。
//
// 用 http.NewResponseController 而非直接改 r.Body：deadline 必须设在
// 底层连接上。ResponseController 会逐层穿透 ResponseWriter 包装
// （compressWriter 实现了 Unwrap），因此即便响应启用了 gzip 压缩
// 也能生效。
//
// 读完立即清除 deadline：其后的响应期是 15s+ 长轮询，必须无读限制。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(bodyReadTimeout)); err != nil {
		// 不支持 deadline 的 ResponseWriter（如测试用httptest 包装）
		// 不应导致请求失败，降级为无超时读取。
		return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	}
	// defer 而非读后立即清除：MaxBytesReader 之外的任何提前返回
	// （含 panic）都不应把 deadline 泄漏给后续长轮询。
	defer rc.SetReadDeadline(time.Time{})
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
}

// buildParams 查询参数在前、body 覆盖在后（对齐 buildRequestJson：POST body
// 先试 JSON，失败回落 form-urlencoded；OrderedField 保序）。
// 返回的 bodyLen 为请求 body 字节数（监控流量统计用）。
func buildParams(w http.ResponseWriter, r *http.Request, mimeType string) (*orderedjson.Map, int, error) {
	m := orderedjson.New()
	// RawQuery 手工解析以保持 URL 顺序
	if qm, err := orderedjson.FormParse([]byte(r.URL.RawQuery)); err == nil {
		m.SetAll(qm)
	}
	bodyLen := 0
	if r.Method == http.MethodPost {
		body, err := readBody(w, r)
		if err != nil {
			return nil, 0, err
		}
		bodyLen = len(body)
		if len(body) > 0 {
			if mimeType == contentTypeJSON {
				if jm, perr := orderedjson.Parse(body); perr == nil {
					m.SetAll(jm)
				} else if fm, ferr := orderedjson.FormParse(body); ferr == nil {
					m.SetAll(fm)
				}
			} else {
				if fm, ferr := orderedjson.FormParse(body); ferr == nil {
					m.SetAll(fm)
				} else if jm, perr := orderedjson.Parse(body); perr == nil {
					m.SetAll(jm)
				}
			}
		}
	}
	return m, bodyLen, nil
}

// timeFromMillis 解析 invoke_timeout（毫秒）；小于 500ms 按默认 15s。
func timeFromMillis(ms int) time.Duration {
	if ms < 500 {
		return 0 // Record 内提升为默认 15s
	}
	return time.Duration(ms) * time.Millisecond
}

// isTimeout 判断错误是否为 deadline 超时（os.ErrDeadlineExceeded 被
// net.Error.IsTimeout 覆盖；此处两者都认，以适配 httptest 与真实连接的差异）。
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---- groupList / clientQueue ----

func handleGroupList(w http.ResponseWriter, mgr *group.Manager) {
	writeCommonRes(w, commonRes{Status: 0, Data: mgr.Names()}, true)
}

func handleClientQueue(w http.ResponseWriter, r *http.Request, mgr *group.Manager) {
	params, _, err := buildParams(w, r, "application/x-www-form-urlencoded")
	if err != nil {
		writeCommonRes(w, commonRes{Status: -1, Message: "read params failed"}, true)
		return
	}
	g := params.GetString(protocol.ParamGroup)
	if g == "" {
		writeCommonRes(w, commonRes{Status: -1, Message: "the param {group} not presented"}, true)
		return
	}
	writeCommonRes(w, commonRes{Status: 0, Data: mgr.EnabledClientIDs(g)}, true)
}

// ---- CommonRes 信封（对齐 fastjson 序列化：省略 null 字段） ----

type commonRes struct {
	Status   int
	Message  string
	Data     any
	ClientID string
	Seq      *int32
}

func writeCommonRes(w http.ResponseWriter, res commonRes, closeConn bool) {
	body := commonResJSON(res.Status, res.Message, res.ClientID, res.Data, res.Seq)
	w.Header().Set("Content-Type", protocol.HTTPContentTypeJSON)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if closeConn {
		w.Header().Set("Connection", "close")
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func commonResJSON(status int, message, clientID string, data any, seq *int32) []byte {
	var b []byte = []byte{'{'}
	first := true
	put := func(k string, v []byte) {
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, "\""+k+"\":"...)
		b = append(b, v...)
	}
	if clientID != "" {
		q, _ := json.Marshal(clientID)
		put("clientId", q)
	}
	if data != nil {
		d, err := json.Marshal(data)
		if err == nil {
			put("data", d)
		}
	}
	if message != "" {
		q, _ := json.Marshal(message)
		put("message", q)
	}
	// ok 布尔字段：Java 版 open.core.CommonRes.isOk() 经 Lombok/fastjson
	// 序列化恒输出（对拍 Netty 服务端实证），字典序位于 message 与 seq 之间
	if status == 0 {
		put("ok", []byte("true"))
	} else {
		put("ok", []byte("false"))
	}
	if seq != nil {
		put("seq", []byte(strconv.FormatInt(int64(*seq), 10)))
	}
	put("status", []byte(strconv.Itoa(status)))
	return append(b, '}')
}

// ---- HTTP 响应工具 ----

func httpResponseText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// ---- httpSink：consumer 为 HTTP 时的响应出口（对齐 buildHttpResponse）----

type httpSink struct {
	w      http.ResponseWriter
	closed chan struct{}
	once   sync.Once

	mu         sync.Mutex
	headerSent bool
	segment    bool
}

func (s *httpSink) Closed() <-chan struct{} { return s.closed }

func (s *httpSink) finish() { s.once.Do(func() { close(s.closed) }) }

func (s *httpSink) RespondCommon(status int, message, clientID string, seq int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body := commonResJSON(status, message, clientID, nil, &seq)
	s.w.Header().Set("Content-Type", protocol.HTTPContentTypeJSON)
	s.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	s.w.WriteHeader(http.StatusOK)
	s.w.Write(body)
	s.finish()
}

// RespondPacket 对齐 NettyUtils.buildHttpResponse：
// HTTP 状态恒 200；业务状态经 JSON 内 status 传递。
func (s *httpSink) RespondPacket(p *protocol.Packet) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p.GetHeader(protocol.PayloadContentType) == protocol.ContentTypeSekiroFastJson {
		// 内部快速反序列化分支（2021+ 客户端）
		status, errMsg, dataJSON, err := decodeFastJSON(p.Data)
		if err != nil {
			httpResponseTextLocked(s.w, http.StatusBadRequest, "error to decode sekiro fast json: "+err.Error())
			s.finish()
			return
		}
		var cidBuf bytes.Buffer
		orderedjson.AppendJSONString(&cidBuf, s.clientIDOf(p))
		msg := "null"
		if errMsg != "" {
			var mb bytes.Buffer
			orderedjson.AppendJSONString(&mb, errMsg)
			msg = mb.String()
		}
		body := `{"clientId":` + cidBuf.String() + `,"message":` + msg + `,"status":` +
			strconv.Itoa(status) + `,"data":` + string(dataJSON) + `}`
		s.writeOnce(protocol.HTTPContentTypeJSON, []byte(body))
		return
	}

	// 分段流分支（SEGMENT_STREAM_FLAG=true）
	flag := strings.EqualFold(p.GetHeader(protocol.SegmentStreamFlag), "true")
	if flag {
		s.segment = true
	}
	if s.segment {
		s.respondSegment(p, flag)
		return
	}

	// 一次性响应分支（兼容 sekiro V1/V2）
	//
	// 对齐原版 NettyUtils.buildHttpResponse once-call 非速 JSON 分支：
	// JSON.parse → put clientId → toJSONString（默认 SortField → 递归字典序，
	// FjProbe/FjProbe3 实证）。因此用 MarshalJSONSorted 而非插入序 MarshalJSON，
	// clientId 经排序合并进字典序位置而非尾随。
	ct := p.GetHeader(protocol.PayloadContentType)
	data := p.Data
	if mainSub(ct) == "application/json" {
		if jm, err := orderedjson.Parse(data); err == nil {
			jm.Set("clientId", s.clientIDOf(p))
			if nb, err := jm.MarshalJSONSorted(); err == nil {
				data = nb
			}
		}
	}
	s.writeOnce(ct, data)
}

// respondSegment 分段流：首包发头，后续 flush 数据，非分段包/空数据终止。
func (s *httpSink) respondSegment(p *protocol.Packet, flag bool) {
	ct := p.GetHeader(protocol.PayloadContentType)
	if !s.headerSent {
		s.headerSent = true
		s.w.Header().Set("Content-Type", ct)
		s.w.Header().Set("Transfer-Encoding", "chunked")
		s.w.WriteHeader(http.StatusOK)
	}
	// onceCall（无分段标记的终结包）或空数据 → 终止 chunked
	if !flag || len(p.Data) == 0 {
		if f, ok := s.w.(http.Flusher); ok {
			f.Flush()
		}
		s.finish()
		return
	}
	s.w.Write(p.Data)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *httpSink) writeOnce(contentType string, body []byte) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	s.headerSent = true
	s.w.Header().Set("Content-Type", contentType)
	s.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	s.w.WriteHeader(http.StatusOK)
	s.w.Write(body)
	s.finish()
}

func httpResponseTextLocked(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func (s *httpSink) clientIDOf(p *protocol.Packet) string {
	return p.GetHeader(protocol.HeaderClientID)
}

// mainSub 提取 mime 主/子类型（对齐 ContentType.getMainType/subType 判断）。
func mainSub(ct string) string {
	if ct == "" {
		return ""
	}
	mime := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	return mime
}

// ---- SekiroFastJson 信封（对齐 SekiroFastJson.quickJsonDecode 逆向格式）----
// payload = [int status][int len][errorMessage UTF-8][int len][dataJson UTF-8]
// len==0 → null

func decodeFastJSON(data []byte) (status int, errMsg string, dataJSON []byte, err error) {
	be := func(b []byte) (int, []byte) {
		if len(b) < 4 {
			return 0, nil
		}
		// 信封字段为 Java int32（有符号）：status=-2 的位模式 0xFFFFFFFE
		// 必须输出 -2 而非 4294967294
		return int(int32(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]))), b[4:]
	}
	status, data = be(data)
	rdStr := func(b []byte) (string, []byte, error) {
		n, rest := be(b)
		if rest == nil {
			return "", nil, io.ErrUnexpectedEOF
		}
		if n == 0 {
			return "", rest, nil
		}
		if n < 0 || n > len(rest) {
			return "", nil, io.ErrUnexpectedEOF
		}
		return string(rest[:n]), rest[n:], nil
	}
	errMsg, data, err = rdStr(data)
	if err != nil {
		return 0, "", nil, err
	}
	var dj string
	dj, data, err = rdStr(data)
	if err != nil {
		return 0, "", nil, err
	}
	if dj == "" {
		// len==0 → Java readString 返回 null → finalJson 中输出字面量 null
		return status, errMsg, []byte("null"), nil
	}
	return status, errMsg, []byte(dj), nil
}
