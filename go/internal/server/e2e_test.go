package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"sekiro-go/internal/config"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

var configForTest = &config.Config{Port: 0}

// ---- 测试基础设施：线上协议级测试客户端 ----

func startTestServer(t *testing.T) (*Server, string) {
	return startTestServerWithSniff(t, defaultSniffTimeout)
}

// startTestServerWithSniff 启动测试服务端并可指定首包探测超时。
// 必须在 accept 协程创建前写字段：否则与 handleConn 的读取缺
// happens-before，-race 会报 data race。
func startTestServerWithSniff(t *testing.T, sniff time.Duration) (*Server, string) {
	t.Helper()
	s, err := New(configForTest)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.sniffTimeout = sniff
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.setListener(ln)
	go func() {
		go s.httpSrv.Serve(s.memLn)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handleConn(conn)
		}
	}()
	t.Cleanup(s.Close)
	return s, ln.Addr().String()
}

type wireClient struct {
	conn net.Conn
	br   *bufio.Reader
	t    *testing.T
	addr string
}

func dialWire(t *testing.T, addr string) *wireClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &wireClient{conn: conn, br: bufio.NewReader(conn), t: t, addr: addr}
}

func (c *wireClient) send(p *protocol.Packet) {
	c.t.Helper()
	c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := protocol.WritePacket(c.conn, p); err != nil {
		c.t.Fatalf("send packet: %v", err)
	}
}

func (c *wireClient) recv() *protocol.Packet {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	p, err := protocol.ReadPacket(c.br)
	if err != nil {
		c.t.Fatalf("recv packet: %v", err)
	}
	return p
}

func (c *wireClient) register(group, id string) {
	c.t.Helper()
	p := protocol.NewPacket(protocol.CTypeRegister)
	p.AddHeader(protocol.HeaderSekiroGroup, group)
	p.AddHeader(protocol.HeaderClientID, id)
	c.send(p)
}

// waitRegistered 轮询 clientQueue 直到 id 上线。register 是 fire-and-forget，
// CI 高负载下注册处理可能落后于紧随其后的 invoke——invoke 命中空池直接
// dispatch-fail，provider 侧 waitInvoke 就会超时（v1.0.0 CI 偶发实锤），
// 因此凡 register 后立即发起 invoke 的用例必须先等注册生效。
func (c *wireClient) waitRegistered(group, id string) {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/business/clientQueue?group=%s", c.addr, group))
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(b), `"`+id+`"`) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("client %s not registered in group %s within 5s", id, group)
}

// waitInvoke 等待一条下发的调用，返回 (paramsJSON, seq)
func (c *wireClient) waitInvoke() ([]byte, int32) {
	c.t.Helper()
	for {
		p := c.recv()
		switch p.Type {
		case protocol.TypeHeartbeat:
			continue
		case protocol.STypeInvoke:
			return p.Data, p.Seq
		default:
			c.t.Fatalf("unexpected packet while waiting invoke: %v", p.Type)
		}
	}
}

func (c *wireClient) respondInvoke(seq int32, body []byte, headers map[string]string) {
	c.t.Helper()
	p := protocol.NewPacket(protocol.CTypeInvokeResponse)
	p.Seq = seq
	for k, v := range headers {
		p.AddHeader(k, v)
	}
	p.Data = body
	c.send(p)
}

// ---- 用例 ----

func TestE2E_NativeProvider_HTTPConsumer(t *testing.T) {
	_, addr := startTestServer(t)
	cl := dialWire(t, addr)
	cl.register("e2e", "dev1")
	cl.waitRegistered("e2e", "dev1")

	// HTTP consumer 调用（含中文参数）
	url := fmt.Sprintf("http://%s/business/invoke?group=e2e&action=fetchData&param=%s&invoke_timeout=8000",
		addr, "%E8%B0%83%E7%94%A8%E6%B5%8B%E8%AF%95")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)

	type httpResult struct {
		status int
		body   []byte
		ct     string
	}
	resCh := make(chan httpResult, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			resCh <- httpResult{status: -1}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		resCh <- httpResult{status: resp.StatusCode, body: b, ct: resp.Header.Get("Content-Type")}
	}()

	data, seq := cl.waitInvoke()
	var params map[string]any
	if err := json.Unmarshal(data, &params); err != nil {
		t.Fatalf("invoke params json: %v", err)
	}
	if params["group"] != "e2e" || params["action"] != "fetchData" {
		t.Fatalf("params: %v", params)
	}
	if params["param"] != "调用测试" {
		t.Fatalf("chinese param broken: %v", params["param"])
	}

	cl.respondInvoke(seq, []byte(`{"status":0,"data":"成功：数据获取完毕"}`), map[string]string{
		protocol.PayloadContentType: "application/json;charset=utf8",
	})

	select {
	case r := <-resCh:
		if r.status != 200 {
			t.Fatalf("http status: %d", r.status)
		}
		var body map[string]any
		if err := json.Unmarshal(r.body, &body); err != nil {
			t.Fatalf("response json: %v (%s)", err, r.body)
		}
		if body["status"] != float64(0) {
			t.Fatalf("body status: %v", body["status"])
		}
		if body["data"] != "成功：数据获取完毕" {
			t.Fatalf("chinese data broken: %v", body["data"])
		}
		if body["clientId"] != "dev1" {
			t.Fatalf("clientId injected: %v", body["clientId"])
		}
	case <-time.After(8 * time.Second):
		t.Fatal("http invoke timeout")
	}
}

func TestE2E_WSFragments_ChineseAllSplitPoints(t *testing.T) {
	// D12 验收：多字节 UTF-8 字符跨 WS 分片边界的全部切分点无乱码
	_, addr := startTestServer(t)
	cl := dialWire(t, addr)
	cl.register("e2ews", "wsdev1")
	cl.waitRegistered("e2ews", "wsdev1")

	ws := dialWS(t, addr, "e2ews", "wsdev1")
	defer ws.close()

	// 响应消息含中文（多字节字符），遍历所有切分点
	msg := []byte(`{"status":0,"data":"调用失败：参数校验未通过，请检查签名"}`)
	for split := 1; split < len(msg); split++ {
		split := split
		t.Run(fmt.Sprintf("split_%d", split), func(t *testing.T) {
			// 发起调用
			done := make(chan []byte, 1)
			errCh := make(chan error, 1)
			go func() {
				url := fmt.Sprintf("http://%s/business/invoke?group=e2ews&action=a", addr)
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errCh <- err
					return
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				done <- b
			}()

			data, seq := ws.waitWSInvoke()
			_ = data

			// 分片发送响应：在 split 字节处切断（可能切断中文字符的 UTF-8 序列）
			ws.sendFragmentedResponse(msg, split, seq)

			select {
			case body := <-done:
				if !bytes.Contains(body, []byte("调用失败：参数校验未通过，请检查签名")) {
					t.Fatalf("garbled response at split %d: %s", split, body)
				}
				if bytes.ContainsRune(body, '\uFFFD') {
					t.Fatalf("U+FFFD replacement found at split %d: %s", split, body)
				}
				var m map[string]any
				if err := json.Unmarshal(body, &m); err != nil {
					t.Fatalf("invalid json at split %d: %v (%s)", split, err, body)
				}
			case err := <-errCh:
				t.Fatalf("http failed at split %d: %v", split, err)
			case <-time.After(6 * time.Second):
				t.Fatalf("timeout at split %d", split)
			}
		})
	}
}

func TestE2E_InvokerChannel(t *testing.T) {
	_, addr := startTestServer(t)
	// provider
	cl := dialWire(t, addr)
	cl.register("e2einv", "dev1")
	cl.waitRegistered("e2einv", "dev1")
	// invoker
	inv := dialWire(t, addr)
	inv.send(protocol.NewPacket(protocol.ITypeConnect))
	ack := inv.recv()
	if ack.Type != protocol.STypeInvokeConnected {
		t.Fatalf("expect S_TYPE_INVOKE_CONNECTED, got %v", ack.Type)
	}

	req := protocol.NewPacket(protocol.ITypeInvoke)
	req.Seq = 5
	req.Data = []byte(`{"group":"e2einv","action":"q"}`)
	inv.send(req)

	data, pseq := cl.waitInvoke()
	if !strings.Contains(string(data), `"group":"e2einv"`) {
		t.Fatalf("invoke params: %s", data)
	}
	// __NOT_COMPRESS_FOR_SEKIRO_SEKIRO 参数标记（对齐 ServiceSekiroInvoker）
	if !strings.Contains(string(data), protocol.DoNotCompress) {
		t.Fatalf("do-not-compress param missing: %s", data)
	}
	cl.respondInvoke(pseq, []byte(`{"status":0,"data":"invoker-ok"}`), nil)

	resp := inv.recv()
	if resp.Type != protocol.STypeInvokeResponse {
		t.Fatalf("expect S_TYPE_INVOKE_RESPONSE (downgraded), got %v", resp.Type)
	}
	if resp.Seq != 5 {
		t.Fatalf("invokerSeq mapping broken: got %d want 5", resp.Seq)
	}
	if !strings.Contains(string(resp.Data), "invoker-ok") {
		t.Fatalf("data: %s", resp.Data)
	}
	if resp.GetHeader(protocol.HeaderClientID) != "dev1" {
		t.Fatal("clientId header missing")
	}
}

func TestE2E_CompressedResponse(t *testing.T) {
	_, addr := startTestServer(t)
	cl := dialWire(t, addr)
	cl.register("e2egz", "dev1")
	cl.waitRegistered("e2egz", "dev1")

	type res struct {
		status int
		body   []byte
	}
	resCh := make(chan res, 1)
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://%s/business/invoke?group=e2egz&action=a", addr))
		if err != nil {
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		resCh <- res{resp.StatusCode, b}
	}()

	data, seq := cl.waitInvoke()
	// provider 以 gzip 压缩响应（COMPRESS_METHOD 头驱动）
	plain := []byte(`{"status":0,"data":"压缩响应解码成功"}`)
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	gz.Write(plain)
	gz.Close()
	cl.respondInvoke(seq, buf.Bytes(), map[string]string{
		protocol.CompressMethod:     protocol.CompressGzip,
		protocol.PayloadContentType: "application/json;charset=utf8",
	})
	_ = data

	select {
	case r := <-resCh:
		if r.status != 200 || !strings.Contains(string(r.body), "压缩响应解码成功") {
			t.Fatalf("compressed response broken: %d %s", r.status, r.body)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("timeout")
	}
}

func TestE2E_HTTPMisc(t *testing.T) {
	_, addr := startTestServer(t)
	base := fmt.Sprintf("http://%s", addr)

	// groupList
	resp, err := http.Get(base + "/business/groupList")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"status":0`) {
		t.Fatalf("groupList: %s", b)
	}

	// 非法 content-type 拒绝（对齐原文案）
	req, _ := http.NewRequest(http.MethodPost, base+"/business/invoke",
		strings.NewReader(`{"group":"g"}`))
	req.Header.Set("Content-Type", "text/plain")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 400 || !strings.Contains(string(b2), "only support contentType") {
		t.Fatalf("content-type reject: %d %s", resp2.StatusCode, b2)
	}

	// 非 API 路径 404（替代原 iinti.cn 反向代理）
	resp3, err := http.Get(base + "/sekiro-doc/")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 404 {
		t.Fatalf("non-api path should be 404, got %d", resp3.StatusCode)
	}

	// 空池快速失败（≤5 次）
	for i := 0; i < 3; i++ {
		resp4, err := http.Get(base + "/business/invoke?group=empty-pool&action=a")
		if err != nil {
			t.Fatal(err)
		}
		b4, _ := io.ReadAll(resp4.Body)
		resp4.Body.Close()
		if !strings.Contains(string(b4), `"message":"pool queue empty"`) {
			t.Fatalf("empty pool: %s", b4)
		}
	}

	// clientQueue
	resp5, err := http.Get(base + "/business/clientQueue?group=empty-pool")
	if err != nil {
		t.Fatal(err)
	}
	b5, _ := io.ReadAll(resp5.Body)
	resp5.Body.Close()
	if !strings.Contains(string(b5), `"data":[]`) {
		t.Fatalf("clientQueue: %s", b5)
	}
}

func TestE2E_UnknownProtocol(t *testing.T) {
	_, addr := startTestServer(t)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("NOT-A-KNOWN-PROTOCOL!!"))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "sekiro unknown protocol") {
		t.Fatalf("unknown protocol response: %q", string(buf[:n]))
	}
}

// ---- 极简 WebSocket 原始帧客户端（完全控制分片） ----

type wsRawClient struct {
	wire *wireClient
}

func dialWS(t *testing.T, addr, group, id string) *wsRawClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	key := make([]byte, 16)
	rand.Read(key)
	wsKey := base64.StdEncoding.EncodeToString(key)
	req := fmt.Sprintf("GET /register?group=%s&clientId=%s HTTP/1.1\r\n"+
		"Host: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		group, id, addr, wsKey)
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("ws handshake write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	// 读完握手响应头
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("ws handshake failed: %q %v", line, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("ws handshake headers: %v", err)
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	conn.SetReadDeadline(time.Time{})
	return &wsRawClient{wire: &wireClient{conn: conn, br: br, t: t}}
}

// sendFragmentedResponse 以原始 WS 帧发送分片响应：
// 在消息的第 split 字节处切断（可切断多字节 UTF-8 字符）。
// 逐帧独立解码即产生乱码（Java 版缺陷场景），
// 服务端必须字节级重组后解码（D12）。
func (w *wsRawClient) sendFragmentedResponse(msg []byte, split int, seq int32) {
	w.wire.t.Helper()
	// 注入 __sekiro_seq__（对齐 WS 协议关联字段）
	prefix := fmt.Sprintf(`{"__sekiro_seq__":%d,`, seq)
	payload := append([]byte(prefix), msg[1:]...) // 替换 msg 开头的 '{'
	off := len(prefix) - 1 + split                // 原 msg 偏移映射到 payload
	if off > len(payload) {
		off = len(payload)
	}
	writeWSFrame(w.wire.conn, payload[:off], true, false)
	writeWSFrame(w.wire.conn, payload[off:], false, true)
}

// writeWSFrame 写一个掩码 WS 帧（客户端→服务端必须掩码）。
func writeWSFrame(conn net.Conn, payload []byte, first, fin bool) {
	opcode := byte(0)
	if first {
		opcode = 1 // text
	}
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	var mask [4]byte
	rand.Read(mask[:])
	n := len(payload)
	var head []byte
	head = append(head, b0)
	switch {
	case n < 126:
		head = append(head, byte(n)|0x80)
	case n <= 0xFFFF:
		head = append(head, 126|0x80)
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(n))
		head = append(head, l[:]...)
	default:
		head = append(head, 127|0x80)
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		head = append(head, l[:]...)
	}
	head = append(head, mask[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	conn.Write(append(head, masked...))
}

func (w *wsRawClient) close() { w.wire.conn.Close() }

// readWSFrame 读一个服务端→客户端 WS 帧（服务端帧不掩码）。
// 跳过 ping/pong，返回 text 帧载荷。
func (w *wsRawClient) readWSFrame() []byte {
	w.wire.t.Helper()
	w.wire.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	rd := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(w.wire.br, b); err != nil {
			w.wire.t.Fatalf("ws read: %v", err)
		}
		return b
	}
	for {
		b0 := rd(1)[0]
		opcode := b0 & 0x0f
		b1 := rd(1)[0]
		masked := b1&0x80 != 0
		n := int(b1 & 0x7f)
		switch n {
		case 126:
			l := rd(2)
			n = int(binary.BigEndian.Uint16(l))
		case 127:
			l := rd(8)
			n = int(binary.BigEndian.Uint64(l))
		}
		var mask []byte
		if masked {
			mask = rd(4)
		}
		payload := rd(n)
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 1, 0: // text / continuation
			return payload
		case 8: // close
			w.wire.t.Fatal("ws closed by server")
		case 9, 10: // ping/pong：跳过
			continue
		default:
			continue
		}
	}
}

// waitWSInvoke 等待服务端下发的 WS 调用（JSON 文本帧 + __sekiro_seq__）。
func (w *wsRawClient) waitWSInvoke() ([]byte, int32) {
	w.wire.t.Helper()
	for {
		payload := w.readWSFrame()
		m, err := orderedjson.Parse(payload)
		if err != nil {
			w.wire.t.Fatalf("ws invoke json: %v (%s)", err, payload)
		}
		seq := m.GetInt(protocol.WSSeqNumber)
		if seq > 0 {
			return payload, int32(seq)
		}
	}
}
