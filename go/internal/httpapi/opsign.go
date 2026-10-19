// /op 监控路径访问控制（OpPassword 启用时生效）。
//
// 设计（2026-10-19 方案确认）：
//   - 认证：POST /op/api/login {password} → 会话 Cookie（HttpOnly; SameSite=Strict；
//     https 反代下加 Secure），12h 滑动过期，内存态、重启失效
//   - 防爆破：每 IP 连续失败 3 次 → 锁定 2 分钟（期内 429 + Retry-After，
//     不比对密码）；成功登录清零；锁定期满计数清零重来
//   - IP 获取：X-Real-IP → X-Forwarded-For 首项 → RemoteAddr
//     （部署在可信内网 nginx 后，XFF 不做伪造防护）
//   - 密码比较 subtle.ConstantTimeCompare（防时序侧信道）
//   - 限流表每 10 分钟清理 30 分钟无活动条目（防 map 无限增长）
package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	opSessionTTL    = 12 * time.Hour // 会话有效期（滑动：每次有效访问续期）
	opMaxFails      = 3              // 连续失败上限
	opLockDuration  = 2 * time.Minute
	opSweepInterval = 10 * time.Minute
	opEntryIdleTTL  = 30 * time.Minute // 条目无活动保留时长（sweep 用）
)

// opAuth 持有会话表与登录限流表。仅 /op 路径使用，量级极小。
type opAuth struct {
	password string

	mu      sync.Mutex
	window  time.Time // 上次 sweep 时刻
	swept   bool
	session map[string]time.Time // token → 过期时刻
	fails   map[string]*opFail   // ip → 失败状态
}

type opFail struct {
	count       int
	lastFail    time.Time
	lockedUntil time.Time
}

func newOpAuth(password string) *opAuth {
	return &opAuth{
		password: password,
		window:   time.Now(),
		swept:    true,
		session:  make(map[string]time.Time),
		fails:    make(map[string]*opFail),
	}
}

// ---- IP 提取 ----

// opClientIP 反代场景取真实来源 IP：X-Real-IP → XFF 首项 → RemoteAddr。
// 可信边界：部署在内网 nginx 后（nginx-sekiro.conf 已配 X-Real-IP），
// 不防御伪造 XFF——伪造者只能锁自己无法锁他人（多值时取首项）。
func opClientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if first := strings.TrimSpace(strings.SplitN(v, ",", 2)[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---- 限流 ----

// lockedRemaining 返回该 IP 剩余锁定时长（未锁定或已到期返回 0）。
func (a *opAuth) lockedRemaining(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	f := a.fails[ip]
	if f == nil {
		return 0
	}
	d := time.Until(f.lockedUntil)
	if d < 0 {
		return 0
	}
	return d
}

// recordFail 记录一次失败；达到 opMaxFails 触发锁定并返回 true。
func (a *opAuth) recordFail(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	f := a.fails[ip]
	if f == nil {
		f = &opFail{}
		a.fails[ip] = f
	}
	f.count++
	f.lastFail = time.Now()
	if f.count >= opMaxFails {
		f.lockedUntil = time.Now().Add(opLockDuration)
		f.count = 0 // 锁定期满后清零重来
		return true
	}
	return false
}

func (a *opAuth) recordSuccess(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
}

// sweepLocked 清理过期条目（持锁调用）。锁定条目保留至锁定到期，
// 其余条目 30 分钟无活动删除；sweep 至少间隔 opSweepInterval。
func (a *opAuth) sweepLocked() {
	now := time.Now()
	if now.Sub(a.window) < opSweepInterval {
		return
	}
	a.window = now
	for ip, f := range a.fails {
		keep := now.Before(f.lockedUntil) || now.Sub(f.lastFail) < opEntryIdleTTL
		if !keep {
			delete(a.fails, ip)
		}
	}
	for tok, exp := range a.session {
		if now.After(exp) {
			delete(a.session, tok)
		}
	}
}

// ---- 会话 ----

func (a *opAuth) newSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见（系统熵源故障）；回退用时间种子的可预测
		// token 不可接受，直接 panic 暴露故障优于静默降级安全性
		panic("opauth: crypto/rand failed: " + err.Error())
	}
	tok := hex.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session[tok] = time.Now().Add(opSessionTTL)
	return tok
}

// validSession 校验并滑动续期。
func (a *opAuth) validSession(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.session[token]
	if !ok || time.Now().After(exp) {
		return false
	}
	a.session[token] = time.Now().Add(opSessionTTL)
	return true
}

// ---- 请求处理 ----

// cookieName 监控会话 Cookie 名。
const cookieName = "sekiro_op_session"

// secureCookie https 反代下为 Cookie 加 Secure。
func secureCookie(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// checkAuth /op 请求鉴权闸门：已登录放行；未登录 401。
// login 路径由调用方先行处理，不进入本函数。
func (a *opAuth) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if c, err := r.Cookie(cookieName); err == nil && a.validSession(c.Value) {
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":"unauthorized"}`))
	return false
}

// handleOpLogin POST /op/api/login {password}。
// 锁定中 → 429 + Retry-After；密码错 → 401 并计数；成功 → 会话 Cookie。
func (a *opAuth) handleOpLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ip := opClientIP(r)
	if d := a.lockedRemaining(ip); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"locked","retryAfterSeconds":` + strconv.Itoa(int(d.Seconds())+1) + `}`))
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.password)) != 1 {
		if locked := a.recordFail(ip); locked {
			w.Header().Set("Retry-After", strconv.Itoa(int(opLockDuration.Seconds())))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"locked","retryAfterSeconds":` + strconv.Itoa(int(opLockDuration.Seconds())) + `}`))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"wrong password"}`))
		return
	}

	a.recordSuccess(ip)
	tok := a.newSession()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/op",
		MaxAge:   int(opSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secureCookie(r),
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write([]byte(`{"ok":true}`))
}

// logout 会话撤销（可选端点 /op/api/logout）。
func (a *opAuth) handleOpLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.session, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/op", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureCookie(r),
	})
	w.Write([]byte(`{"ok":true}`))
}
