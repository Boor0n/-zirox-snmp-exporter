package web

import (
	"errors"
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

// EnsureAdmin creates the admin account on first start (when there are no
// users at all), or resets the password of that account when resetPassword
// is set.
func EnsureAdmin(st *store.Store, user, password string, resetPassword bool) (created bool, err error) {
	n, err := st.UserCount()
	if err != nil {
		return false, err
	}
	if n > 0 && !resetPassword {
		return false, nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return false, err
	}
	u, err := st.UserByName(user)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	u.Name, u.Hash, u.Role = user, string(hash), store.RoleAdmin
	return true, st.SaveUser(&u)
}

// checkPassword returns the user when the name and password match.
func (s *Server) checkPassword(name, password string) (store.User, bool) {
	u, err := s.st.UserByName(name)
	if err != nil {
		// Spend the same time as a real check so user names cannot be probed.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return u, false
	}
	return u, bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password)) == nil
}

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy password"), bcrypt.DefaultCost)

func (s *Server) setSession(w http.ResponseWriter, u store.User) {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	// The password hash and role are part of the signed message, so changing
	// either invalidates that user's existing sessions.
	val := url.QueryEscape(u.Name) + "|" + exp + "|" + s.box.Sign(sessionMsg(u, exp))
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: val, Path: "/", HttpOnly: true,
		Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
}

func sessionMsg(u store.User, exp string) string {
	return u.Name + "|" + exp + "|" + u.Role + "|" + u.Hash
}

// currentUser returns the logged-in user of a request.
func (s *Server) currentUser(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return store.User{}, false
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return store.User{}, false
	}
	name, err := url.QueryUnescape(parts[0])
	if err != nil {
		return store.User{}, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return store.User{}, false
	}
	u, err := s.st.UserByName(name)
	if err != nil || !s.box.Verify(sessionMsg(u, parts[1]), parts[2]) {
		return store.User{}, false
	}
	return u, true
}

// sessionUser returns the logged-in user name, or "".
func (s *Server) sessionUser(r *http.Request) string {
	u, _ := s.currentUser(r)
	return u.Name
}

func (s *Server) isAdmin(r *http.Request) bool {
	u, ok := s.currentUser(r)
	return ok && u.Role == store.RoleAdmin
}

// requireAdmin lets only admins through; read-only users get 403.
func (s *Server) requireAdmin(h http.Handler) http.Handler {
	return s.requireLogin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			http.Error(w, "read-only account: ask an administrator", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	}))
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
	u, ok := s.checkPassword(user, pass)
	if !ok {
		_ = s.st.AddAudit(user, "login failed", clientIP(r), "")
		time.Sleep(500 * time.Millisecond)
		s.render(w, r, "login", map[string]any{"Next": next, "Error": "Wrong username or password"})
		return
	}
	s.setSession(w, u)
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
