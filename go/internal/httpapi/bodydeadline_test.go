package httpapi

// HTTP body 读取超时（bodyReadTimeout）回归验证。
//
// 该保护针对的故障模式：客户端发完请求首部就静默，服务端永久阻塞在
// io.ReadAll，占住 1 个 goroutine + 1 个 fd 且日志无痕迹。
//
// 最大风险点是「误杀」——deadline 若泄漏到响应期，15s 长轮询会被
// 全部打断。因此下面 4 个用例中，长轮询与慢速 body 两例是重点。

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/orderedjson"
	"sekiro-go/internal/protocol"
)

// fakeTransport 假provider：SendInvoke 后延迟 delay 再回响应包，
// 用于构造「响应期远超 bodyReadTimeout」的长轮询场景。
type fakeTransport struct {
	delay time.Duration
	cl    *group.Client
}

func (f *fakeTransport) SendInvoke(params *orderedjson.Map, seq int32) error {
	go func() {
		time.Sleep(f.delay)
		p := protocol.NewPacket(protocol.STypeInvokeResponse)
		p.Seq = seq
		p.AddHeader(protocol.PayloadContentType, "application/json;charset=utf8")
		p.Data = []byte(`{"result":"ok"}`)
		f.cl.HandleResponse(p)
	}()
	return nil
}

func (f *fakeTransport) Active() bool             { return true }
func (f *fakeTransport) Close()                   {}
func (f *fakeTransport) CloseAfter(time.Duration) {}

// withBodyTimeout 在指定 body 读取超时下启动测试服务端。
// 必须在并发请求开始前写bodyReadTimeout（-race 下需happens-before）。
func withBodyTimeout(t *testing.T, timeout time.Duration, providerDelay time.Duration) *httptest.Server {
	t.Helper()
	old := bodyReadTimeout
	bodyReadTimeout = timeout
	t.Cleanup(func() { bodyReadTimeout = old })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := group.NewManager(log)
	if providerDelay > 0 {
		g := mgr.GetOrCreate("longpoll")
		tr := &fakeTransport{delay: providerDelay}
		cl := group.NewClient(tr, g, "slow-provider", log)
		tr.cl = cl
		g.Register(cl)
	}
	srv := httptest.NewServer(Handler(mgr, compress.NewManager(), log, false))
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Close)
	return srv
}

func srvAddr(srv *httptest.Server) string {
	return srv.Listener.Addr().String()
}

// TestBodyReadTimeoutRejectsIncompleteRequest 核心回归：
// 发完首部但不发 body，服务端必须在 bodyReadTimeout 内放弃该连接，
// 而不是永久阻塞在 io.ReadAll。
func TestBodyReadTimeoutRejectsIncompleteRequest(t *testing.T) {
	const timeout = 2 * time.Second
	srv := withBodyTimeout(t, timeout, 0)

	conn, err := net.Dial("tcp", srvAddr(srv))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	hdr := "POST /business/invoke HTTP/1.1\r\nHost: x\r\n" +
		"Content-Type: application/x-www-form-urlencoded\r\n" +
		"Content-Length: 1000\r\n\r\n"
	if _, err := conn.Write([]byte(hdr)); err != nil {
		t.Fatalf("write hdr: %v", err)
	}
	t.Log("已发完整首部（Content-Length: 1000），body 0 字节")

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(timeout + 6*time.Second))
	// 用 http.ReadResponse 而非 io.ReadAll：裸 TCP 上的 ReadAll 必须等
	// 服务端关闭连接才返回 EOF，而本例的重点是「是否在超时内给出 408」，
	// 不是「服务端何时关连接」。
	resp, rerr := http.ReadResponse(bufio.NewReader(conn), nil)
	elapsed := time.Since(start)
	if rerr != nil {
		t.Fatalf("未在超时内收到响应（已等待 %v）: %v", elapsed, rerr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// 关键断言：必须在超时后释放，不能无限等
	if elapsed > timeout+3*time.Second {
		t.Fatalf("服务端未在超时后放弃不完整请求，已等待 %v", elapsed)
	}
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("期望 408 Request Timeout，实际 %d: %s", resp.StatusCode, body)
	}
	t.Logf("BODY-TIMEOUT: %v 后返回 408，连接被及时回收（body: %s）",
		elapsed.Round(time.Millisecond), strings.TrimSpace(string(body)))
}

// TestBodyReadTimeoutNotKillLongPoll 最关键的回归：body 超时不能误杀
// invoke 长轮询。bodyReadTimeout=1s 远小于 provider 3s 的响应耗时，
// 若deadline 泄漏到响应期，所有正常调用都会被误杀。
func TestBodyReadTimeoutNotKillLongPoll(t *testing.T) {
	const providerDelay = 3 * time.Second
	srv := withBodyTimeout(t, 1*time.Second, providerDelay)
	addr := srvAddr(srv)

	// 带 gzip：走compressWriter 包装路径，同时验证 Unwrap 生效
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableCompression: true},
		Timeout:   20 * time.Second,
	}
	body := "group=longpoll&action=slow"
	start := time.Now()
	resp, err := client.Post("http://"+addr+"/business/invoke",
		"application/x-www-form-urlencoded", strings.NewReader(body))
	if err != nil {
		t.Fatalf("长轮询被body 超时误杀（deadline 泄漏到响应期）: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", resp.StatusCode, string(raw))
	}
	if !strings.Contains(string(raw), `"result":"ok"`) {
		t.Fatalf("未拿到 provider 响应: %s", string(raw))
	}
	if elapsed < providerDelay {
		t.Fatalf("响应过快（%v），说明长轮询未真正等待", elapsed)
	}
	t.Logf("LONG-POLL: 正常完成，耗时 %v（bodyReadTimeout=1s，未被误杀）",
		elapsed.Round(time.Millisecond))
}

// TestBodyReadTimeoutClearedAfterRead 验证 deadline 在读完 body 后被清除：
// bodyReadTimeout=100ms 远小于请求处理耗时，若泄漏，后续请求必然失败。
func TestBodyReadTimeoutClearedAfterRead(t *testing.T) {
	srv := withBodyTimeout(t, 100*time.Millisecond, 50*time.Millisecond)
	addr := srvAddr(srv)

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   10 * time.Second,
	}
	for i := 0; i < 5; i++ {
		resp, err := client.Post("http://"+addr+"/business/invoke",
			"application/x-www-form-urlencoded",
			strings.NewReader("group=longpoll&action=x"))
		if err != nil {
			t.Fatalf("iter %d: 请求失败（deadline 泄漏?）: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iter %d: 状态码 %d", i, resp.StatusCode)
		}
	}
	t.Log("DEADLINE-CLEAR: 连续 5 个正常请求全部成功（bodyReadTimeout=100ms，未泄漏）")
}

// TestSlowButLegitimateBodyAllowed 验证「慢但不挂死」的 body 仍被接受：
// 在超时窗口内分批发送 body（约 10KB/s），应正常完成。
func TestSlowButLegitimateBodyAllowed(t *testing.T) {
	srv := withBodyTimeout(t, 3*time.Second, 50*time.Millisecond)
	addr := srvAddr(srv)

	payload := "group=longpoll&action=x&pad=" + strings.Repeat("p", 5000)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := "POST /business/invoke HTTP/1.1\r\nHost: x\r\n" +
		"Content-Type: application/x-www-form-urlencoded\r\n" +
		"Content-Length: " + strconv.Itoa(len(payload)) + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write hdr: %v", err)
	}
	// 500ms 内分批发完（约 10KB/s，慢但远快于 3s 超时）
	for i := 0; i < len(payload); i += 500 {
		end := i + 500
		if end > len(payload) {
			end = len(payload)
		}
		conn.Write([]byte(payload[i:end]))
		time.Sleep(50 * time.Millisecond)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("慢速但合法的 body 被拒: %d %s", resp.StatusCode, raw)
	}
	t.Log("SLOW-OK: 慢速 body（约 10KB/s，持续 500ms）被正常接受")
}
