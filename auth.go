package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieSession = "cd_session"
	cookiePending = "cd_pending"
	cookieCSRF    = "cd_csrf"
	cookieFlash   = "cd_flash"
	cookieNext    = "cd_next"

	sessionTTL = 30 * 24 * time.Hour
	pendingTTL = 10 * time.Minute
	linkTTL    = 15 * time.Minute
)

// ---------- signed values ----------

func (a *App) sign(payload string) string {
	mac := hmac.New(sha256.New, a.store.Secret())
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *App) unsign(v string) (string, bool) {
	i := strings.LastIndex(v, ".")
	if i < 0 {
		return "", false
	}
	payload, sig := v[:i], v[i+1:]
	mac := hmac.New(sha256.New, a.store.Secret())
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return "", false
	}
	return payload, true
}

// token format: purpose|memberID|sessionEpoch|expiryUnix
func (a *App) issueToken(purpose, id string, epoch int, ttl time.Duration) string {
	return a.sign(fmt.Sprintf("%s|%s|%d|%d", purpose, id, epoch, time.Now().Add(ttl).Unix()))
}

func (a *App) readToken(purpose, v string) (*Member, bool) {
	payload, ok := a.unsign(v)
	if !ok {
		return nil, false
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 4 || parts[0] != purpose {
		return nil, false
	}
	epoch, _ := strconv.Atoi(parts[2])
	exp, _ := strconv.ParseInt(parts[3], 10, 64)
	if time.Now().Unix() > exp {
		return nil, false
	}
	m, err := a.store.ByID(parts[1])
	if err != nil || m.SessionEpoch != epoch {
		return nil, false
	}
	if accessOf(m) == AccessNone {
		return nil, false
	}
	return m, true
}

func (a *App) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
	if ttl > 0 {
		c.Expires = time.Now().Add(ttl)
		c.MaxAge = int(ttl.Seconds())
	}
	http.SetCookie(w, c)
}

func (a *App) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
		Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (a *App) login(w http.ResponseWriter, m *Member) {
	a.setCookie(w, cookieSession, a.issueToken("s", m.ID, m.SessionEpoch, sessionTTL), sessionTTL)
	a.clearCookie(w, cookiePending)
	now := time.Now()
	a.store.Update(m.ID, func(x *Member) error { x.LastLoginAt = &now; return nil })
}

func (a *App) current(r *http.Request) *Member {
	c, err := r.Cookie(cookieSession)
	if err != nil {
		return nil
	}
	m, ok := a.readToken("s", c.Value)
	if !ok {
		return nil
	}
	return m
}

// ---------- CSRF (double submit) ----------

func (a *App) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(cookieCSRF); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	tok := randToken(24)
	a.setCookie(w, cookieCSRF, tok, sessionTTL)
	return tok
}

func (a *App) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(cookieCSRF)
	if err != nil {
		return false
	}
	got := r.FormValue("csrf")
	return len(got) >= 20 && subtle.ConstantTimeCompare([]byte(c.Value), []byte(got)) == 1
}

// ---------- flash messages ----------

func (a *App) flash(w http.ResponseWriter, kind, msg string) {
	a.setCookie(w, cookieFlash, base64.RawURLEncoding.EncodeToString([]byte(kind+"|"+msg)), 5*time.Minute)
}

type flashMsg struct {
	Kind string
	Text string
}

func (a *App) takeFlash(w http.ResponseWriter, r *http.Request) *flashMsg {
	c, err := r.Cookie(cookieFlash)
	if err != nil || c.Value == "" {
		return nil
	}
	a.clearCookie(w, cookieFlash)
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return nil
	}
	return &flashMsg{Kind: parts[0], Text: parts[1]}
}

// ---------- magic links ----------

type linkRec struct {
	MemberID string
	Purpose  string
	Next     string
	Expires  time.Time
}

type linkStore struct {
	mu sync.Mutex
	m  map[string]linkRec
}

func newLinkStore() *linkStore {
	ls := &linkStore{m: map[string]linkRec{}}
	go func() {
		for range time.Tick(time.Minute) {
			ls.mu.Lock()
			for k, v := range ls.m {
				if time.Now().After(v.Expires) {
					delete(ls.m, k)
				}
			}
			ls.mu.Unlock()
		}
	}()
	return ls
}

func (ls *linkStore) issue(memberID, purpose, next string, ttl time.Duration) string {
	tok := randToken(32)
	ls.mu.Lock()
	defer ls.mu.Unlock()
	// Only one live link per member: requesting a new one invalidates the old.
	for k, v := range ls.m {
		if v.MemberID == memberID {
			delete(ls.m, k)
		}
	}
	ls.m[tok] = linkRec{MemberID: memberID, Purpose: purpose, Next: next, Expires: time.Now().Add(ttl)}
	return tok
}

// consume redeems a token exactly once.
func (ls *linkStore) consume(tok string) (linkRec, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	rec, ok := ls.m[tok]
	if !ok || time.Now().After(rec.Expires) {
		delete(ls.m, tok)
		return linkRec{}, false
	}
	delete(ls.m, tok)
	return rec, true
}

// ---------- rate limiting ----------

type limiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{hits: map[string][]time.Time{}, max: max, window: window}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, time.Now())
	return true
}

// clientIP resolves the caller's address. X-Forwarded-For is only consulted
// when the immediate peer is a configured trusted proxy: otherwise anyone on
// the network could set the header and sidestep the login rate limit.
func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !a.trusted(host) {
		return host
	}
	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		return host
	}
	parts := strings.Split(fwd, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if ip != "" && !a.trusted(ip) {
			return ip // nearest address the proxy chain did not vouch for
		}
	}
	return host
}

func (a *App) trusted(ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, n := range a.trustedProxies {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// parseProxies reads a comma-separated list of IPs or CIDR blocks.
func parseProxies(spec string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			if ip := net.ParseIP(part); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				part = fmt.Sprintf("%s/%d", part, bits)
			}
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// landing picks where to send a member after a successful sign-in.
func landing(next string) string {
	if next != "" {
		return next
	}
	return "/account"
}

// ---------- audit log ----------

type AuditLog struct {
	mu   sync.Mutex
	path string
}

type auditEntry struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Subject string    `json:"subject,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	IP      string    `json:"ip,omitempty"`
}

func newAuditLog(dir string) *AuditLog { return &AuditLog{path: filepath.Join(dir, "audit.log")} }

func (al *AuditLog) Write(actor, action, subject, detail, ip string) {
	e := auditEntry{At: time.Now(), Actor: actor, Action: action, Subject: subject, Detail: detail, IP: ip}
	b, _ := json.Marshal(e)
	al.mu.Lock()
	defer al.mu.Unlock()
	f, err := os.OpenFile(al.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("audit log unavailable (%v); event was: %s", err, b)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("audit log write failed (%v); event was: %s", err, b)
	}
}

func (al *AuditLog) Tail(n int) []auditEntry {
	al.mu.Lock()
	defer al.mu.Unlock()
	b, err := os.ReadFile(al.path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]auditEntry, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		var e auditEntry
		if json.Unmarshal([]byte(lines[i]), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}
