package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// tokenPrefix makes API tokens easy to recognise in configs and secret scanners.
const tokenPrefix = "zx_"

func checkNewPassword(pw, confirm string) error {
	if len(pw) < 8 || pw != confirm {
		return errors.New("new password: at least 8 characters, both fields must match")
	}
	return nil
}

func setPassword(u *store.User, pw string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.Hash = string(hash)
	return nil
}

func formRole(r *http.Request) string {
	if r.FormValue("role") == store.RoleAdmin {
		return store.RoleAdmin
	}
	return store.RoleViewer
}

// hashToken returns the stored form of an API token.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	s.renderUsers(w, r, nil)
}

func (s *Server) renderUsers(w http.ResponseWriter, r *http.Request, data map[string]any) {
	users, err := s.st.Users()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tokens, err := s.st.APITokens()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Users"], data["Tokens"], data["Me"] = users, tokens, s.sessionUser(r)
	s.render(w, r, "users", data)
}

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request) {
	u := store.User{Name: strings.TrimSpace(r.FormValue("name")), Role: formRole(r)}
	if u.Name == "" {
		redirectErr(w, r, "/users", errors.New("enter a user name"))
		return
	}
	if err := checkNewPassword(r.FormValue("password"), r.FormValue("confirm")); err != nil {
		redirectErr(w, r, "/users", err)
		return
	}
	if err := setPassword(&u, r.FormValue("password")); err != nil {
		redirectErr(w, r, "/users", err)
		return
	}
	if err := s.st.SaveUser(&u); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			err = errors.New("a user with this name already exists")
		}
		redirectErr(w, r, "/users", err)
		return
	}
	s.audit(r, "user added", u.Name, u.Role)
	redirect(w, r, "/users", "User "+u.Name+" added")
}

// userUpdate changes a user's role and, when given, sets a new password.
func (s *Server) userUpdate(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.User(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	role := formRole(r)
	var changes []string
	if role != u.Role {
		if u.Role == store.RoleAdmin {
			if n, _ := s.st.AdminCount(); n <= 1 {
				redirectErr(w, r, "/users", errors.New("there must be at least one administrator"))
				return
			}
		}
		changes = append(changes, "role: "+u.Role+" → "+role)
		u.Role = role
	}
	if pw := r.FormValue("password"); pw != "" {
		if err := checkNewPassword(pw, pw); err != nil {
			redirectErr(w, r, "/users", err)
			return
		}
		if err := setPassword(&u, pw); err != nil {
			redirectErr(w, r, "/users", err)
			return
		}
		changes = append(changes, "password reset")
	}
	if len(changes) == 0 {
		redirect(w, r, "/users", "Nothing changed")
		return
	}
	if err := s.st.SaveUser(&u); err != nil {
		redirectErr(w, r, "/users", err)
		return
	}
	s.audit(r, "user changed", u.Name, strings.Join(changes, "; "))
	redirect(w, r, "/users", "User "+u.Name+" saved")
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.User(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if u.Name == s.sessionUser(r) {
		redirectErr(w, r, "/users", errors.New("you cannot delete yourself"))
		return
	}
	if u.Role == store.RoleAdmin {
		if n, _ := s.st.AdminCount(); n <= 1 {
			redirectErr(w, r, "/users", errors.New("there must be at least one administrator"))
			return
		}
	}
	if err := s.st.DeleteUser(u.ID); err != nil {
		redirectErr(w, r, "/users", err)
		return
	}
	s.audit(r, "user deleted", u.Name, "")
	redirect(w, r, "/users", "User "+u.Name+" deleted")
}

// tokenCreate makes a new API token and shows it once; only its hash is kept.
func (s *Server) tokenCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirectErr(w, r, "/users", errors.New("enter a token name"))
		return
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tok := tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	t := store.APIToken{Name: name, Hash: hashToken(tok), Role: formRole(r), CreatedBy: s.sessionUser(r)}
	if err := s.st.CreateAPIToken(&t); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			err = errors.New("a token with this name already exists")
		}
		redirectErr(w, r, "/users", err)
		return
	}
	s.audit(r, "API token created", t.Name, t.Role)
	w.Header().Set("Cache-Control", "no-store")
	s.renderUsers(w, r, map[string]any{"NewToken": tok, "NewTokenName": t.Name})
}

func (s *Server) tokenDelete(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.APIToken(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.st.DeleteAPIToken(t.ID); err != nil {
		redirectErr(w, r, "/users", err)
		return
	}
	s.audit(r, "API token revoked", t.Name, "")
	redirect(w, r, "/users", "Token "+t.Name+" revoked")
}
