package web

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Login throttling: after loginMaxFails wrong passwords within loginWindow
// from one address, or for one user name, logins from that address or for
// that name are refused for loginWindow. A correct password while blocked is
// refused too, so guessing cannot continue slowly in the background.
const (
	loginMaxFails = 5
	loginWindow   = 15 * time.Minute
)

type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time // "ip:..." and "user:..." keys
	now   func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{fails: map[string][]time.Time{}, now: time.Now}
}

func limiterKeys(ip, user string) []string {
	return []string{"ip:" + ip, "user:" + strings.ToLower(user)}
}

// blocked reports whether a login from ip for user must be refused and,
// if so, how long until it is allowed again.
func (l *loginLimiter) blocked(ip, user string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var wait time.Duration
	for _, k := range limiterKeys(ip, user) {
		ts := l.recent(k, now)
		if len(ts) >= loginMaxFails {
			if w := ts[len(ts)-1].Add(loginWindow).Sub(now); w > wait {
				wait = w
			}
		}
	}
	return wait > 0, wait
}

func (l *loginLimiter) fail(ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, k := range limiterKeys(ip, user) {
		l.fails[k] = append(l.recent(k, now), now)
	}
	// Keep the map small under a spray of random names.
	if len(l.fails) > 10000 {
		for k := range l.fails {
			if len(l.recent(k, now)) == 0 {
				delete(l.fails, k)
			}
		}
	}
}

// success forgets the failures of the user name (not of the address, so a
// valid account cannot be used to reset guessing on others).
func (l *loginLimiter) success(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, "user:"+strings.ToLower(user))
}

// recent returns the failures of key inside the window; caller holds mu.
func (l *loginLimiter) recent(k string, now time.Time) []time.Time {
	ts := l.fails[k]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > loginWindow {
		i++
	}
	if i == len(ts) {
		delete(l.fails, k)
		return nil
	}
	ts = ts[i:]
	l.fails[k] = ts
	return ts
}

// securityHeaders are sent with every HTML page. Scripts and styles come only
// from /static (style attributes stay allowed for the load bars); nothing may
// frame the UI.
func securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
}
