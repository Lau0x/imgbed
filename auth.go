package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "picnook_session"
	sessionTTL    = 7 * 24 * time.Hour
	maxFails      = 5
	failWindow    = 15 * time.Minute
	pbkdf2Iter    = 600_000 // OWASP 对 PBKDF2-HMAC-SHA256 的建议值
)

func (a *app) sign(msg string) string {
	m := hmac.New(sha256.New, a.cfg.secret)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// sessionVersion 随密码变化：改密码后，之前签发的登录状态全部失效
func (a *app) sessionVersion() string {
	src := "env:" + a.cfg.password
	if h := a.store.get().PasswordHash; h != "" {
		src = "hash:" + h
	}
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:8])
}

func (a *app) setSession(w http.ResponseWriter) {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    exp + "." + a.sign("session|"+exp+"|"+a.sessionVersion()),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *app) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign("session|"+exp+"|"+a.sessionVersion()))) {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && time.Now().Unix() < n
}

// checkPassword 优先使用后台改过的密码（加盐哈希），没改过时用 PICNOOK_PASSWORD
func (a *app) checkPassword(pw string) bool {
	if h := a.store.get().PasswordHash; h != "" {
		return verifyPassword(h, pw)
	}
	x := sha256.Sum256([]byte(pw))
	y := sha256.Sum256([]byte(a.cfg.password))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

// hashPassword 生成 pbkdf2-sha256$迭代次数$盐$哈希 格式的字符串
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func verifyPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

var errPasswordPolicy = errors.New("新密码长度需要在 8 到 128 位之间")

func checkPasswordPolicy(pw string) error {
	if n := len([]rune(pw)); n < 8 || n > 128 {
		return errPasswordPolicy
	}
	return nil
}

// sameOrigin 防 CSRF：带 Cookie 的写操作必须来自本站页面
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		o = r.Header.Get("Referer")
	}
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// clientIP 只在请求来自可信反向代理时才使用 X-Real-IP
func (a *app) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	for _, n := range a.cfg.trusted {
		if ip != nil && n.Contains(ip) {
			if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
				return real
			}
			break
		}
	}
	return host
}

type failRecord struct {
	count int
	first time.Time
}

type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{fails: make(map[string]*failRecord)}
}

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.fails[ip]
	if rec == nil {
		return false
	}
	if time.Since(rec.first) > failWindow {
		delete(l.fails, ip)
		return false
	}
	return rec.count >= maxFails
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 10000 {
		for k, rec := range l.fails {
			if time.Since(rec.first) > failWindow {
				delete(l.fails, k)
			}
		}
	}
	rec := l.fails[ip]
	if rec == nil || time.Since(rec.first) > failWindow {
		l.fails[ip] = &failRecord{count: 1, first: time.Now()}
		return
	}
	rec.count++
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.Contains(next, "\\") {
		return next
	}
	return "/"
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	switch r.Method {
	case http.MethodGet:
		if a.loggedIn(r) {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		a.render(w, http.StatusOK, "login.html", map[string]string{"Next": next})
	case http.MethodPost:
		ip := a.clientIP(r)
		data := map[string]string{"Next": next}
		if !sameOrigin(r) {
			data["Error"] = "请求来源无效，请刷新页面重试"
			a.render(w, http.StatusForbidden, "login.html", data)
			return
		}
		if a.limiter.blocked(ip) {
			data["Error"] = "失败次数过多，请 15 分钟后再试"
			a.render(w, http.StatusTooManyRequests, "login.html", data)
			return
		}
		if !a.checkPassword(r.PostFormValue("password")) {
			a.limiter.fail(ip)
			data["Error"] = "密码错误"
			a.render(w, http.StatusUnauthorized, "login.html", data)
			return
		}
		a.limiter.reset(ip)
		a.setSession(w)
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !sameOrigin(r) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
