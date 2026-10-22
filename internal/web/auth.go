package web

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

const (
	cookieName = "zirox_session"
	sessionTTL = 12 * time.Hour
)

// EnsureAdmin creates the admin account on first start, or resets its
// password when resetPassword is set.
func EnsureAdmin(st *store.Store, user, password string, resetPassword bool) (created bool, err error) {
	if st.GetSetting(store.SettingAdminPw, "") != "" && !resetPassword {
		return false, nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return false, err
	}
	if err := st.SetSetting(store.SettingAdmin, user); err != nil {
		return false, err
	}
	return true, st.SetSetting(store.SettingAdminPw, string(hash))
}

func (s *Server) checkPassword(user, password string) bool {
	if user != s.st.GetSetting(store.SettingAdmin, "") {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(s.st.GetSetting(store.SettingAdminPw, "")), []byte(password)) == nil
}

func (s *Server) setSession(w http.ResponseWriter, user string) {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	// The password hash is part of the signed message, so changing the
	// password invalidates all existing sessions.
	msg := user + "|" + exp
	val := url.QueryEscape(user) + "|" + exp + "|" + s.box.Sign(msg+"|"+s.st.GetSetting(store.SettingAdminPw, ""))
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: val, Path: "/", HttpOnly: true,
		Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
}

func (s *Server) sessionUser(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return ""
	}
	user, err := url.QueryUnescape(parts[0])
	if err != nil {
		return ""
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	if !s.box.Verify(user+"|"+parts[1]+"|"+s.st.GetSetting(store.SettingAdminPw, ""), parts[2]) {
		return ""
	}
	if user != s.st.GetSetting(store.SettingAdmin, "") {
		return ""
	}
	return user
}

// requireLogin protects the UI and rejects cross-site form posts.
func (s *Server) requireLogin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.sessionUser(r) == "" {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true // non-browser clients; the SameSite cookie already blocks cross-site posts
	}
	u, err := url.Parse(src)
	return err == nil && u.Host == r.Host
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", map[string]any{"Next": r.URL.Query().Get("next")})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	user, pass := r.FormValue("user"), r.FormValue("password")
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	if !s.checkPassword(user, pass) {
		_ = s.st.AddAudit(user, "login failed", clientIP(r), "")
		time.Sleep(500 * time.Millisecond)
		s.render(w, r, "login", map[string]any{"Next": next, "Error": "Wrong username or password"})
		return
	}
	s.setSession(w, user)
	_ = s.st.AddAudit(user, "login", clientIP(r), "")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
