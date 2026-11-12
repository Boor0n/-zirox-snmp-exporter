// Package web is the management UI: devices, port selection, credentials,
// poll profiles and global settings.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/engine"
	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Config holds UI options.
type Config struct {
	CookieSecure bool
	Version      string
	// SecretKey goes into downloaded backups; nil when it comes from
	// ZIROX_SECRET_KEY (then the backup says to set it on the new server).
	SecretKey []byte
	// HTTPS, when on: the certificate file (served at /tls/cert.pem),
	// whether Zirox made it, its SHA-256 and expiry, for the Prometheus page.
	TLSCertFile    string
	TLSSelfSigned  bool
	TLSFingerprint string
	TLSNotAfter    time.Time
}

// Server serves the UI.
type Server struct {
	st    *store.Store
	en    *engine.Engine
	box   *secret.Box
	cfg   Config
	log   *slog.Logger
	pages map[string]*template.Template
	jobs  jobs
	upd   updateChecker
	limit *loginLimiter
}

// New parses the templates and returns the UI server.
func New(st *store.Store, en *engine.Engine, box *secret.Box, cfg Config, log *slog.Logger) (*Server, error) {
	s := &Server{st: st, en: en, box: box, cfg: cfg, log: log, pages: map[string]*template.Template{}, limit: newLoginLimiter()}
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if name == "layout" || name == "partials" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", n)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

// Register mounts the UI routes.
func (s *Server) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	if s.cfg.TLSCertFile != "" {
		// Public by nature: browsers and Prometheus fetch it to trust a self-signed certificate.
		mux.HandleFunc("GET /tls/cert.pem", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.Header().Set("Content-Disposition", `attachment; filename="zirox.crt"`)
			http.ServeFile(w, r, s.cfg.TLSCertFile)
		})
	}

	p := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireLogin(h)) }
	a := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAdmin(h)) }
	p("GET /{$}", s.overview)
	p("GET /devices", s.devices)
	p("GET /search", s.search)
	p("GET /devices/{id}/live", s.deviceLive)
	a("GET /devices/new", s.deviceForm)
	a("POST /devices/new", s.deviceSave)
	a("POST /devices/test", s.deviceTest)
	a("POST /devices/bulk", s.deviceBulk)
	p("GET /devices/{id}", s.deviceCard)
	a("GET /devices/{id}/edit", s.deviceForm)
	a("POST /devices/{id}/edit", s.deviceSave)
	a("POST /devices/{id}/refresh", s.deviceRefresh)
	a("POST /devices/{id}/toggle", s.deviceToggle)
	a("POST /devices/{id}/delete", s.deviceDelete)
	a("POST /devices/{id}/selection", s.deviceSelection)
	p("GET /devices/{id}/status", s.deviceStatus)
	p("GET /devices/{id}/diagnose", s.deviceDiagnose)
	p("GET /checks/{job}", s.checkPage)
	p("GET /checks/{job}/csv", s.checkCSV)
	a("GET /scan", s.scanPage)
	a("POST /scan", s.scanStart)
	a("GET /scan/{job}", s.scanPage)
	a("POST /scan/{job}/add", s.scanAdd)
	p("GET /credentials", s.credentials)
	a("GET /credentials/new", s.credentialForm)
	a("POST /credentials/new", s.credentialSave)
	a("GET /credentials/{id}/edit", s.credentialForm)
	a("POST /credentials/{id}/edit", s.credentialSave)
	a("POST /credentials/{id}/delete", s.credentialDelete)
	p("GET /rules", s.rules)
	a("POST /rules", s.ruleSave)
	a("POST /rules/apply", s.rulesApply)
	a("POST /rules/{id}/delete", s.ruleDelete)
	a("POST /rules/{id}/move", s.ruleMove)
	p("GET /profiles", s.profiles)
	a("POST /profiles", s.profileSave)
	a("POST /profiles/{id}/delete", s.profileDelete)
	a("GET /config", s.configPage)
	a("GET /config/export.yaml", s.configExport)
	a("POST /config/import", s.configImport)
	a("GET /config/backup.tar.gz", s.configBackup)
	p("GET /settings", s.settings)
	a("POST /settings", s.settingsSave)
	p("POST /settings/password", s.passwordSave)
	a("GET /import", s.importPage)
	a("POST /import", s.importSave)
	p("GET /prometheus", s.prometheusHelp)
	a("GET /users", s.users)
	a("POST /users", s.userCreate)
	a("POST /users/{id}", s.userUpdate)
	a("POST /users/{id}/delete", s.userDelete)
	a("POST /tokens", s.tokenCreate)
	a("POST /tokens/{id}/delete", s.tokenDelete)
	p("GET /custom-oids", s.customOIDs)
	a("POST /custom-oids", s.customOIDSave)
	a("POST /custom-oids/{id}/delete", s.customOIDDelete)
	a("POST /custom-oids/refresh-all", s.customRefreshAll)
	s.registerAPI(mux)
	p("GET /audit", s.auditPage)
}

// audit records a change made through the UI. Failures are logged, not
// shown: the change itself already happened.
func (s *Server) audit(r *http.Request, action, target, details string) {
	if err := s.st.AddAudit(s.actor(r), action, target, details); err != nil {
		s.log.Warn("audit log", "err", err)
	}
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	entries, err := s.st.AuditLog(q, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "audit", map[string]any{"Entries": entries, "Q": q})
}

// deviceChanges describes what an edit changed, for the audit log.
func (s *Server) deviceChanges(a, b store.Device) string {
	var ch []string
	add := func(field string, x, y any) {
		if x != y {
			ch = append(ch, fmt.Sprintf("%s: %v → %v", field, x, y))
		}
	}
	add("name", a.Name, b.Name)
	add("address", a.Address, b.Address)
	add("port", a.Port, b.Port)
	add("group", a.Group, b.Group)
	add("interval", a.IntervalSec, b.IntervalSec)
	if a.ProfileID != b.ProfileID {
		ch = append(ch, "profile: "+s.profileName(a.ProfileID)+" → "+s.profileName(b.ProfileID))
	}
	if a.CredentialID != b.CredentialID {
		ch = append(ch, "credentials: "+s.credName(a.CredentialID)+" → "+s.credName(b.CredentialID))
	}
	return strings.Join(ch, "; ")
}

func (s *Server) profileName(id int64) string {
	if p, err := s.st.Profile(id); err == nil {
		return p.Name
	}
	return "global"
}

func (s *Server) credName(id int64) string {
	if c, err := s.st.Credential(id); err == nil {
		return c.Name
	}
	return fmt.Sprint(id)
}

var funcs = template.FuncMap{
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		d := time.Since(t).Round(time.Second)
		switch {
		case d < time.Minute:
			return fmt.Sprintf("%d s ago", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%d min ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%d h ago", int(d.Hours()))
		}
		return t.Format("02.01.2006 15:04")
	},
	"dur": func(d time.Duration) string {
		if d >= time.Second {
			return fmt.Sprintf("%.1f s", d.Seconds())
		}
		return fmt.Sprintf("%d ms", d.Milliseconds())
	},
	"secs":    func(d time.Duration) int { return int(d.Seconds()) },
	"uptime":  uptimeText,
	"bits":    bitsText,
	"errrate": errText,
	"rate": func(m map[int64]engine.PortRate, id int64) *engine.PortRate {
		if r, ok := m[id]; ok {
			return &r
		}
		return nil
	},
	"pct":   func(v float64) int { return int(v + 0.5) },
	"hm":    func(t time.Time) string { return t.Format("15:04") },
	"list":  func(v ...string) []string { return v },
	"list2": func(v ...any) []any { return v },
	// roles lists the role choices, plus cur when a rule set another word.
	"roles": func(cur string) []string {
		if cur == "" || slices.Contains(store.Roles, cur) {
			return store.Roles
		}
		return append(slices.Clone(store.Roles), cur)
	},
	"hasPrefix": strings.HasPrefix,
	"status":    rowStatus,
	"speed":     speedText,
	"iftype": func(t int) string {
		if n, ok := ifTypeNames[t]; ok {
			return n
		}
		return "type " + strconv.Itoa(t)
	},
	"physical": func(t int) bool { return physicalTypes[t] },
	"hostkey":  store.HostKey,
	"upper":    strings.ToUpper,
	"oper": func(s int) string {
		switch s {
		case 1:
			return "up"
		case 2:
			return "down"
		case 5:
			return "dormant"
		case 6:
			return "notPresent"
		case 7:
			return "lowerLayerDown"
		}
		return "?"
	},
}

var ifTypeNames = map[int]string{
	1: "other", 6: "ethernet", 24: "loopback", 53: "virtual", 56: "fibreChannel", 62: "fastEthernet",
	69: "fastEthernetFX", 117: "gigabitEthernet", 131: "tunnel", 135: "l2vlan", 136: "SVI/VLAN",
	161: "port-channel", 166: "mpls", 71: "wifi", 23: "ppp", 32: "frameRelay", 150: "mplsTunnel",
}

var physicalTypes = map[int]bool{6: true, 56: true, 62: true, 69: true, 117: true, 161: true}

// uptimeText shows a duration as "12d 4h", "3h 20m" or "5m".
func uptimeText(d time.Duration) string {
	d = d.Round(time.Minute)
	days, h, m := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func speedText(mbps int64) string {
	switch {
	case mbps <= 0:
		return ""
	case mbps >= 1000 && mbps%1000 == 0:
		return fmt.Sprintf("%d Gbit/s", mbps/1000)
	}
	return fmt.Sprintf("%d Mbit/s", mbps)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "no template "+page, http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	securityHeaders(w.Header())
	data["Page"] = page
	data["Title"] = pageTitles[page]
	u, _ := s.currentUser(r)
	data["User"] = u.Name
	data["CanEdit"] = u.Role == store.RoleAdmin
	data["Version"] = s.cfg.Version
	if tag, url := s.upd.Latest(); newer(tag, s.cfg.Version) {
		data["UpdateTag"], data["UpdateURL"] = tag, url
	}
	if m := r.URL.Query().Get("msg"); m != "" {
		data["Flash"] = m
	}
	if m := r.URL.Query().Get("err"); m != "" {
		data["FlashErr"] = m
	}
	if m := r.URL.Query().Get("warn"); m != "" {
		data["FlashWarn"] = m
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

// pageTitles name each page in the browser tab, so several open tabs can be told apart.
var pageTitles = map[string]string{
	"devices": "Devices", "device": "Device", "device_form": "Device", "credentials": "Credentials",
	"credential_form": "Credentials", "profiles": "Poll profiles", "rules": "Port rules", "config": "Configuration", "settings": "Settings", "import": "Import",
	"custom_oids": "Custom OIDs", "overview": "Overview", "search": "Search", "diagnose": "Diagnostics", "check": "Access check", "scan": "Find devices", "prometheus": "Prometheus", "audit": "Change log", "users": "Users", "login": "Log in",
}

func redirect(w http.ResponseWriter, r *http.Request, path, msg string) {
	if msg != "" {
		path += sep(path) + "msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// redirectWarn redirects with a success message and, when warn is set, a warning.
func redirectWarn(w http.ResponseWriter, r *http.Request, path, msg, warn string) {
	if warn != "" {
		path += sep(path) + "warn=" + url.QueryEscape(warn)
	}
	redirect(w, r, path, msg)
}

func redirectErr(w http.ResponseWriter, r *http.Request, path string, err error) {
	http.Redirect(w, r, path+sep(path)+"err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
}

func sep(path string) string {
	if strings.Contains(path, "?") {
		return "&"
	}
	return "?"
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func formInt(r *http.Request, key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(key)))
	return n
}
