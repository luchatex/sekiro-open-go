package httpapi

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
)

func newTestAuth(t *testing.T) *opAuth {
	t.Helper()
	a := newOpAuth("secret")
	// 收紧 sweep 间隔的测试不影响行为正确性（sweep 幂等），保持默认即可
	return a
}

func loginBody(pwd string) *bytes.Reader {
	b, _ := json.Marshal(map[string]string{"password": pwd})
	return bytes.NewReader(b)
}

func loginRequest(pwd string) *http.Request {
	r := httptest.NewRequest("POST", "/op/api/login", loginBody(pwd))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// TestOpLoginSuccessAndSession 登录成功 → 会话 Cookie 有效 → checkAuth 放行。
func TestOpLoginSuccessAndSession(t *testing.T) {
	a := newTestAuth(t)
	r := loginRequest("secret")
	w := httptest.NewRecorder()
	a.handleOpLogin(w, r)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value == "" {
		t.Fatal("no session cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/op" {
		t.Fatalf("cookie flags wrong: %+v", c)
	}
	if !a.validSession(c.Value) {
		t.Fatal("session should be valid")
	}

	// checkAuth 带 Cookie 放行
	r2 := httptest.NewRequest("GET", "/op/api/stats", nil)
	r2.AddCookie(c)
	w2 := httptest.NewRecorder()
	if !a.checkAuth(w2, r2) {
		t.Fatal("checkAuth should pass with valid session")
	}
	if w2.Code != 200 {
		t.Fatalf("checkAuth wrote %d", w2.Code)
	}
}

// TestOpLoginRequiresJSON 登录写操作拒绝简单跨站表单类型。
func TestOpLoginRequiresJSON(t *testing.T) {
	a := newTestAuth(t)
	r := httptest.NewRequest("POST", "/op/api/login", loginBody("secret"))
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	a.handleOpLogin(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain login = %d, want 415", w.Code)
	}
}

// TestOpLoginWrongPasswordAndLock 连续 3 次失败锁定 → 429 → 期满恢复。
func TestOpLoginWrongPasswordAndLock(t *testing.T) {
	a := newTestAuth(t)
	for i := 1; i <= 3; i++ {
		r := loginRequest("wrong")
		w := httptest.NewRecorder()
		a.handleOpLogin(w, r)
		want := 401
		if i == 3 {
			want = 429 // 第 3 次失败即触发锁定
		}
		if w.Code != want {
			t.Fatalf("attempt %d: code %d, want %d", i, w.Code, want)
		}
	}
	// 锁定期内：即使密码正确也 429（不比对密码）
	r := loginRequest("secret")
	w := httptest.NewRecorder()
	a.handleOpLogin(w, r)
	if w.Code != 429 {
		t.Fatalf("locked request: %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Fatal("Retry-After missing on 429")
	}
}

// TestOpLockExpiry 锁定到期后可重新登录（计数已清零）。
func TestOpLockExpiry(t *testing.T) {
	a := newTestAuth(t)
	ip := "1.2.3.4"
	for i := 0; i < 3; i++ {
		a.recordFail(ip)
	}
	if d := a.lockedRemaining(ip); d <= 0 {
		t.Fatal("should be locked")
	}
	// 手动把锁定时刻拨回过去（模拟 2 分钟流逝）
	a.mu.Lock()
	a.fails[ip].lockedUntil = time.Now().Add(-time.Second)
	a.mu.Unlock()
	if d := a.lockedRemaining(ip); d != 0 {
		t.Fatalf("lock should be expired, got %v", d)
	}
	r := loginRequest("secret")
	w := httptest.NewRecorder()
	a.handleOpLogin(w, r)
	if w.Code != 200 {
		t.Fatalf("login after lock expiry: %d", w.Code)
	}
}

// TestOpClientIPTrustedProxy 只有可信代理来源才读取转发头；
// XFF 取最右侧（nginx 追加的真实 peer），公网直连忽略伪造头。
func TestOpClientIPTrustedProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/op/api/login", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	if got := opClientIP(r); got != "10.0.0.1" {
		t.Fatalf("remoteaddr fallback: %s", got)
	}
	r.Header.Set("X-Forwarded-For", "8.8.8.8, 10.0.0.9")
	if got := opClientIP(r); got != "10.0.0.9" {
		t.Fatalf("xff rightmost: %s", got)
	}
	r.Header.Set("X-Real-IP", "9.9.9.9")
	if got := opClientIP(r); got != "9.9.9.9" {
		t.Fatalf("x-real-ip priority: %s", got)
	}
	r.Header.Set("X-Real-IP", "not-an-ip")
	if got := opClientIP(r); got != "10.0.0.9" {
		t.Fatalf("invalid x-real-ip fallback: %s", got)
	}

	// 公网直连：伪造 X-Real-IP/XFF 必须全部忽略。
	r.RemoteAddr = "203.0.113.7:4567"
	r.Header.Set("X-Real-IP", "1.1.1.1")
	r.Header.Set("X-Forwarded-For", "2.2.2.2")
	if got := opClientIP(r); got != "203.0.113.7" {
		t.Fatalf("untrusted direct client spoofed IP: %s", got)
	}
}

// TestSecureCookieDirectTLS 直连 TLS 无 X-Forwarded-Proto 时也必须加 Secure。
func TestSecureCookieDirectTLS(t *testing.T) {
	r := httptest.NewRequest("POST", "/op/api/login", nil)
	r.TLS = &tls.ConnectionState{}
	if !secureCookie(r) {
		t.Fatal("direct TLS cookie must be Secure")
	}
}

// TestOpNewSessionSweepsExpired 新登录也要触发过期会话清理，
// 避免只有登录成功、从不失败时 session map 无界增长。
func TestOpNewSessionSweepsExpired(t *testing.T) {
	a := newTestAuth(t)
	a.mu.Lock()
	a.window = time.Now().Add(-2 * opSweepInterval)
	a.session["expired"] = time.Now().Add(-time.Hour)
	a.mu.Unlock()

	tok := a.newSession()
	a.mu.Lock()
	_, expired := a.session["expired"]
	_, current := a.session[tok]
	a.mu.Unlock()
	if expired || !current {
		t.Fatalf("expired=%v current=%v, want false/true", expired, current)
	}
}

// TestOpLogoutRequiresPOST 防止 GET 触发会话撤销（CSRF logout）。
func TestOpLogoutRequiresPOST(t *testing.T) {
	a := newTestAuth(t)
	r := httptest.NewRequest(http.MethodGet, "/op/api/logout", nil)
	w := httptest.NewRecorder()
	a.handleOpLogout(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout = %d, want 405", w.Code)
	}
}

// TestOpAuthGateOnHandler 端到端：配置密码后 /op 与 /op/api/* 全部 401，
// 登录后放行；业务路径不受影响。
func TestOpAuthGateOnHandler(t *testing.T) {
	h := Handler(group.NewManager(nil), compress.NewManager(), nil, true, "secret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// 未登录：/op 页面壳放行（200，登录遮罩由前端按 API 401 弹出）；
	// /op/api/* 全 401
	for _, p := range []string{"/op/api/stats", "/op/api/clients", "/op/api/errors"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s: %d, want 401", p, resp.StatusCode)
		}
	}
	pageResp, err := http.Get(srv.URL + "/op")
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	if pageResp.StatusCode != 200 || !strings.Contains(string(pageBody), "loginMask") {
		t.Fatalf("/op shell: %d (should serve page with loginMask)", pageResp.StatusCode)
	}

	// 登录拿 Cookie（jar 自动携带），会话内访问放行
	jarred := &http.Client{Jar: mustJar()}
	lr2, err := jarred.Post(srv.URL+"/op/api/login", "application/json",
		bytes.NewBufferString(`{"password":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	lr2.Body.Close()
	if lr2.StatusCode != 200 {
		t.Fatalf("login2: %d", lr2.StatusCode)
	}
	resp, err := jarred.Get(srv.URL + "/op/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stats with session: %d", resp.StatusCode)
	}

	// 业务路径不受密码保护
	resp2, err := http.Get(srv.URL + "/business/groupList")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("business path should be open, got %d", resp2.StatusCode)
	}
}

// mustJar 测试用 CookieJar（自动管理会话 Cookie）。
func mustJar() http.CookieJar {
	j, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return j
}

// TestOpNoPasswordOpen 未配置密码：/op 全开放（向后兼容）。
func TestOpNoPasswordOpen(t *testing.T) {
	h := Handler(group.NewManager(nil), compress.NewManager(), nil, true, "")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/op")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/op without password: %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("content type: %s", resp.Header.Get("Content-Type"))
	}
}
