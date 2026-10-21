// Package web is the management UI: devices, port selection, credentials,
// poll profiles and global settings.
package web

import (
	"embed"
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/engine"
	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Config holds UI options.
type Config struct {
	CookieSecure bool
	Version      string
}

// Server serves the UI.
type Server struct {
	st    *store.Store
	en    *engine.Engine
	box   *secret.Box
	cfg   Config
	log   *slog.Logger
	pages map[string]*template.Template
}

// New parses the templates and returns the UI server.
func New(st *store.Store, en *engine.Engine, box *secret.Box, cfg Config, log *slog.Logger) (*Server, error) {
	s := &Server{st: st, en: en, box: box, cfg: cfg, log: log, pages: map[string]*template.Template{}}
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

	p := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireLogin(h)) }
	p("GET /{$}", s.devices)
	p("GET /devices/new", s.deviceForm)
	p("POST /devices/new", s.deviceSave)
	p("POST /devices/test", s.deviceTest)
	p("POST /devices/bulk", s.deviceBulk)
	p("GET /devices/{id}", s.deviceCard)
	p("GET /devices/{id}/edit", s.deviceForm)
	p("POST /devices/{id}/edit", s.deviceSave)
	p("POST /devices/{id}/refresh", s.deviceRefresh)
	p("POST /devices/{id}/toggle", s.deviceToggle)
	p("POST /devices/{id}/delete", s.deviceDelete)
	p("POST /devices/{id}/selection", s.deviceSelection)
	p("GET /devices/{id}/status", s.deviceStatus)
	p("GET /credentials", s.credentials)
	p("GET /credentials/new", s.credentialForm)
	p("POST /credentials/new", s.credentialSave)
	p("GET /credentials/{id}/edit", s.credentialForm)
	p("POST /credentials/{id}/edit", s.credentialSave)
	p("POST /credentials/{id}/delete", s.credentialDelete)
	p("GET /profiles", s.profiles)
	p("POST /profiles", s.profileSave)
	p("POST /profiles/{id}/delete", s.profileDelete)
	p("GET /settings", s.settings)
	p("POST /settings", s.settingsSave)
	p("POST /settings/password", s.passwordSave)
	p("GET /import", s.importPage)
	p("POST /import", s.importSave)
	p("GET /prometheus", s.prometheusHelp)
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
	"secs":  func(d time.Duration) int { return int(d.Seconds()) },
	"list":  func(v ...string) []string { return v },
	"speed": speedText,
	"iftype": func(t int) string {
		if n, ok := ifTypeNames[t]; ok {
			return n
		}
		return "type " + strconv.Itoa(t)
	},
	"physical": func(t int) bool { return physicalTypes[t] },
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
	data["Page"] = page
	data["User"] = s.sessionUser(r)
	data["Version"] = s.cfg.Version
	if m := r.URL.Query().Get("msg"); m != "" {
		data["Flash"] = m
	}
	if m := r.URL.Query().Get("err"); m != "" {
		data["FlashErr"] = m
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, path, msg string) {
	if msg != "" {
		path += sep(path) + "msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
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

// ---- devices ----

// DeviceRow is one line of the device list.
type DeviceRow struct {
	store.Device
	Selected    int
	Total       int
	Snap        *engine.Snapshot
	Eff         engine.Effective
	Discovering bool
	Credential  string
	Profile     string
}

func (s *Server) deviceRows() ([]DeviceRow, error) {
	devs, err := s.st.Devices()
	if err != nil {
		return nil, err
	}
	counts, err := s.st.ObjectCounts()
	if err != nil {
		return nil, err
	}
	creds, _ := s.st.Credentials()
	credName := map[int64]string{}
	for _, c := range creds {
		credName[c.ID] = c.Name
	}
	profs, _ := s.st.Profiles()
	profName := map[int64]string{}
	for _, p := range profs {
		profName[p.ID] = p.Name
	}
	rows := make([]DeviceRow, 0, len(devs))
	for _, d := range devs {
		c := counts[d.ID]
		rows = append(rows, DeviceRow{
			Device: d, Selected: c[0], Total: c[1], Snap: s.en.Snapshot(d.ID), Eff: s.en.Resolve(d),
			Discovering: s.en.Discovering(d.ID), Credential: credName[d.CredentialID], Profile: profName[d.ProfileID],
		})
	}
	return rows, nil
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deviceRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	group := r.URL.Query().Get("group")
	status := r.URL.Query().Get("status")
	groups := map[string]bool{}
	var shown []DeviceRow
	stats := map[string]int{}
	for _, d := range rows {
		if d.Group != "" {
			groups[d.Group] = true
		}
		st := rowStatus(d)
		stats[st]++
		if q != "" && !strings.Contains(strings.ToLower(d.Name+" "+d.Address+" "+d.SysName+" "+d.Group), q) {
			continue
		}
		if group != "" && d.Group != group {
			continue
		}
		if status != "" && st != status {
			continue
		}
		shown = append(shown, d)
	}
	var groupList []string
	for g := range groups {
		groupList = append(groupList, g)
	}
	sort.Strings(groupList)
	s.render(w, r, "devices", map[string]any{
		"Rows": shown, "Total": len(rows), "Groups": groupList, "Q": r.URL.Query().Get("q"),
		"Group": group, "Status": status, "Stats": stats, "AnyDiscovering": s.en.AnyDiscovering(),
	})
}

func rowStatus(d DeviceRow) string {
	switch {
	case !d.Enabled:
		return "disabled"
	case d.Discovering:
		return "discovering"
	case d.LastDiscovery.IsZero() && d.DiscoveryError != "":
		return "failing"
	case d.LastDiscovery.IsZero():
		return "pending"
	case d.Snap == nil:
		return "pending"
	case d.Snap.OK:
		return "ok"
	}
	return "failing"
}

func (s *Server) formLists() (map[string]any, error) {
	creds, err := s.st.Credentials()
	if err != nil {
		return nil, err
	}
	profs, err := s.st.Profiles()
	if err != nil {
		return nil, err
	}
	return map[string]any{"Credentials": creds, "Profiles": profs, "Global": s.st.Settings()}, nil
}

func (s *Server) deviceForm(w http.ResponseWriter, r *http.Request) {
	data, err := s.formLists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := store.Device{Port: 161, Enabled: true}
	if id := pathID(r); id != 0 {
		if d, err = s.st.Device(id); err != nil {
			http.NotFound(w, r)
			return
		}
	} else if creds := data["Credentials"].([]store.Credential); len(creds) > 0 {
		d.CredentialID = creds[0].ID
	}
	data["Device"] = d
	data["Cred"] = store.Credential{Version: "2c", V3Level: "authPriv", V3AuthProto: "SHA", V3PrivProto: "AES"}
	if d.CredentialID == 0 {
		data["NewCred"] = true
	}
	s.render(w, r, "device_form", data)
}

func deviceFromForm(r *http.Request) store.Device {
	d := store.Device{
		ID:          pathID(r),
		Name:        strings.TrimSpace(r.FormValue("name")),
		Address:     strings.TrimSpace(r.FormValue("address")),
		Port:        formInt(r, "port"),
		IntervalSec: formInt(r, "interval_sec"),
		Group:       strings.TrimSpace(r.FormValue("group")),
		Enabled:     r.FormValue("enabled") == "on",
	}
	d.ProfileID, _ = strconv.ParseInt(r.FormValue("profile_id"), 10, 64)
	d.CredentialID, _ = strconv.ParseInt(r.FormValue("credential_id"), 10, 64)
	if d.ID == 0 {
		d.ID, _ = strconv.ParseInt(r.FormValue("id"), 10, 64)
	}
	if d.Name == "" {
		d.Name = d.Address
	}
	return d
}

func credFromForm(r *http.Request) store.Credential {
	return store.Credential{
		Name:        strings.TrimSpace(r.FormValue("cred_name")),
		Version:     r.FormValue("version"),
		Community:   r.FormValue("community"),
		V3User:      strings.TrimSpace(r.FormValue("v3_user")),
		V3Level:     r.FormValue("v3_level"),
		V3AuthProto: r.FormValue("v3_auth_proto"),
		V3AuthPass:  r.FormValue("v3_auth_pass"),
		V3PrivProto: r.FormValue("v3_priv_proto"),
		V3PrivPass:  r.FormValue("v3_priv_pass"),
		V3Context:   strings.TrimSpace(r.FormValue("v3_context")),
	}
}

func validateCred(c store.Credential) error {
	switch c.Version {
	case "1", "2c":
		if c.Community == "" {
			return errors.New("enter a community")
		}
	case "3":
		if c.V3User == "" {
			return errors.New("enter an SNMPv3 user")
		}
		if c.V3Level != "noAuthNoPriv" && len(c.V3AuthPass) < 8 {
			return errors.New("SNMPv3 auth password must be at least 8 characters")
		}
		if c.V3Level == "authPriv" && len(c.V3PrivPass) < 8 {
			return errors.New("SNMPv3 privacy password must be at least 8 characters")
		}
	default:
		return errors.New("choose an SNMP version")
	}
	return nil
}

// resolveCredential returns the credential chosen on the device form: an
// existing one, or one typed in place (reusing an identical saved one).
func (s *Server) resolveCredential(r *http.Request, devName string) (store.Credential, error) {
	if r.FormValue("credential_id") != "new" {
		id, _ := strconv.ParseInt(r.FormValue("credential_id"), 10, 64)
		c, err := s.st.Credential(id)
		if err != nil {
			return c, errors.New("choose credentials")
		}
		return c, nil
	}
	c := credFromForm(r)
	if err := validateCred(c); err != nil {
		return c, err
	}
	return s.findOrCreateCredential(c, devName)
}

func (s *Server) findOrCreateCredential(c store.Credential, hint string) (store.Credential, error) {
	all, err := s.st.Credentials()
	if err != nil {
		return c, err
	}
	for _, ex := range all {
		plain, err := s.en.DecryptCredential(ex)
		if err != nil {
			continue
		}
		plain.ID, plain.Name = 0, ""
		cmp := c
		cmp.Name = ""
		if plain == cmp {
			return ex, nil
		}
	}
	if c.Name == "" {
		c.Name = "SNMPv" + c.Version + " (" + hint + ")"
	}
	if err := s.encrypt(&c); err != nil {
		return c, err
	}
	return c, s.st.SaveCredential(&c)
}

func (s *Server) encrypt(c *store.Credential) error {
	var err error
	for _, f := range []*string{&c.Community, &c.V3AuthPass, &c.V3PrivPass} {
		if *f, err = s.box.Encrypt(*f); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) rerenderDeviceForm(w http.ResponseWriter, r *http.Request, d store.Device, msg string, ok bool) {
	data, err := s.formLists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data["Device"] = d
	c := credFromForm(r)
	c.Community, c.V3AuthPass, c.V3PrivPass = "", "", "" // never echo secrets back
	data["Cred"] = c
	data["NewCred"] = r.FormValue("credential_id") == "new"
	if ok {
		data["TestOK"] = msg
	} else {
		data["Error"] = msg
	}
	s.render(w, r, "device_form", data)
}

func (s *Server) deviceSave(w http.ResponseWriter, r *http.Request) {
	d := deviceFromForm(r)
	if d.Address == "" {
		s.rerenderDeviceForm(w, r, d, "enter an IP address or DNS name", false)
		return
	}
	c, err := s.resolveCredential(r, d.Name)
	if err != nil {
		s.rerenderDeviceForm(w, r, d, err.Error(), false)
		return
	}
	d.CredentialID = c.ID
	isNew := d.ID == 0
	var before store.Device
	if !isNew {
		if before, err = s.st.Device(d.ID); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	if err := s.st.SaveDevice(&d); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE") {
			msg = "a device with this name already exists"
		}
		s.rerenderDeviceForm(w, r, d, msg, false)
		return
	}
	// A new device, or one whose address or credentials changed, gets a full discovery.
	if isNew || before.Address != d.Address || before.Port != d.Port || before.CredentialID != d.CredentialID || before.LastDiscovery.IsZero() {
		s.en.Refresh(d.ID)
		redirect(w, r, fmt.Sprintf("/devices/%d", d.ID), "Device saved, full discovery running")
		return
	}
	s.en.Reload(d.ID)
	redirect(w, r, fmt.Sprintf("/devices/%d", d.ID), "Device saved")
}

func (s *Server) deviceTest(w http.ResponseWriter, r *http.Request) {
	d := deviceFromForm(r)
	c, err := s.resolveCredentialNoSave(r)
	if err != nil {
		s.rerenderDeviceForm(w, r, d, err.Error(), false)
		return
	}
	eff := s.en.Resolve(d)
	info, err := s.en.Test(snmpx.Target{Address: d.Address, Port: d.Port, Cred: c, Timeout: max(eff.Timeout, 3*time.Second), Retries: 1})
	if err != nil {
		s.rerenderDeviceForm(w, r, d, "No response: "+err.Error(), false)
		return
	}
	s.rerenderDeviceForm(w, r, d, fmt.Sprintf("Device answered: %s — %s", info.SysName, firstLine(info.SysDescr)), true)
}

func (s *Server) resolveCredentialNoSave(r *http.Request) (store.Credential, error) {
	if r.FormValue("credential_id") == "new" {
		c := credFromForm(r)
		return c, validateCred(c)
	}
	id, _ := strconv.ParseInt(r.FormValue("credential_id"), 10, 64)
	c, err := s.st.Credential(id)
	if err != nil {
		return c, errors.New("choose credentials")
	}
	return s.en.DecryptCredential(c)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// ObjectGroup is one section of the device card.
type ObjectGroup struct {
	Category string
	Objects  []store.Object
	Selected int
}

func (s *Server) deviceCard(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	objs, err := s.st.Objects(d.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byCat := map[string]*ObjectGroup{}
	var nNew, nMissing int
	for _, o := range objs {
		cat := snmpx.CategoryOf(o.Kind)
		g, ok := byCat[cat]
		if !ok {
			g = &ObjectGroup{Category: cat}
			byCat[cat] = g
		}
		g.Objects = append(g.Objects, o)
		if o.Selected && o.State != store.StateMissing {
			g.Selected++
		}
		switch o.State {
		case store.StateNew:
			nNew++
		case store.StateMissing:
			nMissing++
		}
	}
	var groups []ObjectGroup
	for _, c := range append(snmpx.Categories, "Other") {
		if g, ok := byCat[c]; ok {
			groups = append(groups, *g)
		}
	}
	cred, _ := s.st.Credential(d.CredentialID)
	snap := s.en.Snapshot(d.ID)
	s.render(w, r, "device", map[string]any{
		"Device": d, "Groups": groups, "Snap": snap, "Eff": s.en.Resolve(d), "Discovering": s.en.Discovering(d.ID),
		"New": nNew, "Missing": nMissing, "Credential": cred, "SampleCount": sampleCount(snap),
	})
}

func sampleCount(s *engine.Snapshot) int {
	if s == nil {
		return 0
	}
	return len(s.Samples)
}

func (s *Server) deviceStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"discovering":%t}`, s.en.Discovering(pathID(r)))
}

func (s *Server) deviceRefresh(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	back := r.FormValue("back")
	if back != "list" {
		back = fmt.Sprintf("/devices/%d", id)
	} else {
		back = "/"
	}
	if s.en.Refresh(id) {
		redirect(w, r, back, "Full discovery started")
		return
	}
	redirect(w, r, back, "Discovery is already running")
}

func (s *Server) deviceToggle(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.st.SetDeviceEnabled(d.ID, !d.Enabled); err != nil {
		redirectErr(w, r, "/", err)
		return
	}
	s.en.Reload(d.ID)
	if !d.Enabled && d.LastDiscovery.IsZero() {
		s.en.Refresh(d.ID)
	}
	redirect(w, r, "/", "")
}

func (s *Server) deviceDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if err := s.st.DeleteDevice(id); err != nil {
		redirectErr(w, r, "/", err)
		return
	}
	s.en.Reload(id)
	redirect(w, r, "/", "Device deleted")
}

func (s *Server) deviceSelection(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, fmt.Sprintf("/devices/%d", id), err)
		return
	}
	sel := map[int64]bool{}
	for _, v := range r.Form["obj"] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			sel[n] = true
		}
	}
	if err := s.st.SaveSelection(id, sel); err != nil {
		redirectErr(w, r, fmt.Sprintf("/devices/%d", id), err)
		return
	}
	s.en.Reload(id)
	redirect(w, r, fmt.Sprintf("/devices/%d", id), fmt.Sprintf("Applied: polling %d objects", len(sel)))
}

func (s *Server) deviceBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, "/", err)
		return
	}
	var ids []int64
	for _, v := range r.Form["ids"] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	if len(ids) == 0 {
		redirect(w, r, "/", "Nothing selected")
		return
	}
	action := r.FormValue("action")
	for _, id := range ids {
		switch action {
		case "refresh":
			s.en.Refresh(id)
		case "enable", "disable":
			if err := s.st.SetDeviceEnabled(id, action == "enable"); err == nil {
				s.en.Reload(id)
			}
		case "delete":
			if err := s.st.DeleteDevice(id); err == nil {
				s.en.Reload(id)
			}
		case "profile":
			d, err := s.st.Device(id)
			if err != nil {
				continue
			}
			d.ProfileID, _ = strconv.ParseInt(r.FormValue("profile_id"), 10, 64)
			if err := s.st.SaveDevice(&d); err == nil {
				s.en.Reload(id)
			}
		}
	}
	names := map[string]string{"refresh": "Full discovery started", "enable": "Enabled", "disable": "Disabled", "delete": "Deleted", "profile": "Profile assigned"}
	redirect(w, r, "/", fmt.Sprintf("%s: %d devices", names[action], len(ids)))
}

// ---- credentials ----

func (s *Server) credentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.st.Credentials()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	devs, _ := s.st.Devices()
	uses := map[int64]int{}
	for _, d := range devs {
		uses[d.CredentialID]++
	}
	s.render(w, r, "credentials", map[string]any{"Credentials": creds, "Uses": uses})
}

func (s *Server) credentialForm(w http.ResponseWriter, r *http.Request) {
	c := store.Credential{Version: "2c", V3Level: "authPriv", V3AuthProto: "SHA", V3PrivProto: "AES"}
	if id := pathID(r); id != 0 {
		var err error
		if c, err = s.st.Credential(id); err != nil {
			http.NotFound(w, r)
			return
		}
		c.Community, c.V3AuthPass, c.V3PrivPass = "", "", ""
	}
	s.render(w, r, "credential_form", map[string]any{"Cred": c})
}

func (s *Server) credentialSave(w http.ResponseWriter, r *http.Request) {
	c := credFromForm(r)
	c.ID = pathID(r)
	if c.Name == "" {
		s.render(w, r, "credential_form", map[string]any{"Cred": c, "Error": "enter a name"})
		return
	}
	if c.ID != 0 {
		// Empty secret fields keep the stored value.
		old, err := s.st.Credential(c.ID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		plain, err := s.en.DecryptCredential(old)
		if err != nil {
			s.render(w, r, "credential_form", map[string]any{"Cred": c, "Error": err.Error()})
			return
		}
		if c.Community == "" {
			c.Community = plain.Community
		}
		if c.V3AuthPass == "" {
			c.V3AuthPass = plain.V3AuthPass
		}
		if c.V3PrivPass == "" {
			c.V3PrivPass = plain.V3PrivPass
		}
	}
	if err := validateCred(c); err != nil {
		c.Community, c.V3AuthPass, c.V3PrivPass = "", "", ""
		s.render(w, r, "credential_form", map[string]any{"Cred": c, "Error": err.Error()})
		return
	}
	if err := s.encrypt(&c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.st.SaveCredential(&c); err != nil {
		c.Community, c.V3AuthPass, c.V3PrivPass = "", "", ""
		s.render(w, r, "credential_form", map[string]any{"Cred": c, "Error": err.Error()})
		return
	}
	_ = s.en.ReloadAll()
	redirect(w, r, "/credentials", "Credentials saved")
}

func (s *Server) credentialDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteCredential(pathID(r)); err != nil {
		redirectErr(w, r, "/credentials", err)
		return
	}
	redirect(w, r, "/credentials", "Deleted")
}

// ---- poll profiles ----

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	profs, err := s.st.Profiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	devs, _ := s.st.Devices()
	uses := map[int64]int{}
	for _, d := range devs {
		uses[d.ProfileID]++
	}
	edit := store.PollProfile{IntervalSec: 60, TimeoutSec: 5, Retries: 1, AutoSelectUp: true}
	if id, _ := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64); id != 0 {
		if p, err := s.st.Profile(id); err == nil {
			edit = p
		}
	}
	s.render(w, r, "profiles", map[string]any{"Profiles": profs, "Uses": uses, "Edit": edit, "Global": s.st.Settings(), "Min": store.MinIntervalSec})
}

func (s *Server) profileSave(w http.ResponseWriter, r *http.Request) {
	p := store.PollProfile{
		Name:         strings.TrimSpace(r.FormValue("name")),
		IntervalSec:  formInt(r, "interval_sec"),
		TimeoutSec:   formInt(r, "timeout_sec"),
		Retries:      formInt(r, "retries"),
		AutoSelectUp: r.FormValue("auto_select_up") == "on",
	}
	p.ID, _ = strconv.ParseInt(r.FormValue("id"), 10, 64)
	if p.Name == "" {
		redirectErr(w, r, "/profiles", errors.New("enter a profile name"))
		return
	}
	if err := s.st.SaveProfile(&p); err != nil {
		redirectErr(w, r, "/profiles", err)
		return
	}
	_ = s.en.ReloadAll()
	redirect(w, r, "/profiles", "Profile saved and applied")
}

func (s *Server) profileDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteProfile(pathID(r)); err != nil {
		redirectErr(w, r, "/profiles", err)
		return
	}
	_ = s.en.ReloadAll()
	redirect(w, r, "/profiles", "Profile deleted; its devices now use the global settings")
}

// ---- settings ----

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings", map[string]any{"Settings": s.st.Settings(), "Min": store.MinIntervalSec, "Admin": s.st.GetSetting(store.SettingAdmin, "")})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	st := store.Settings{
		IntervalSec:  formInt(r, "interval_sec"),
		TimeoutSec:   formInt(r, "timeout_sec"),
		Retries:      formInt(r, "retries"),
		AutoSelectUp: r.FormValue("auto_select_up") == "on",
	}
	if err := s.st.SaveSettings(st); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	_ = s.en.ReloadAll()
	redirect(w, r, "/settings", "Global settings applied")
}

func (s *Server) passwordSave(w http.ResponseWriter, r *http.Request) {
	user := s.sessionUser(r)
	if !s.checkPassword(user, r.FormValue("current")) {
		redirectErr(w, r, "/settings", errors.New("current password is wrong"))
		return
	}
	pw := r.FormValue("new")
	if len(pw) < 8 || pw != r.FormValue("confirm") {
		redirectErr(w, r, "/settings", errors.New("new password: at least 8 characters, both fields must match"))
		return
	}
	if _, err := EnsureAdmin(s.st, user, pw, true); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	s.setSession(w, user)
	redirect(w, r, "/settings", "Password changed")
}

// ---- CSV import ----

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.formLists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "import", data)
}

// ImportResult reports one CSV line.
type ImportResult struct {
	Line int
	Name string
	OK   bool
	Msg  string
}

func (s *Server) importSave(w http.ResponseWriter, r *http.Request) {
	var src io.Reader = strings.NewReader(r.FormValue("csv"))
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		src = f
	}
	results, err := s.ImportCSV(src, r.FormValue("default_credential"), r.FormValue("default_profile"))
	data, _ := s.formLists()
	if data == nil {
		data = map[string]any{}
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	data["Results"] = results
	ok := 0
	for _, res := range results {
		if res.OK {
			ok++
		}
	}
	data["Imported"] = ok
	s.render(w, r, "import", data)
}

// ImportCSV adds devices from CSV with a header row. Known columns:
// name, address, port, group, profile, credential, version, community.
func (s *Server) ImportCSV(src io.Reader, defaultCred, defaultProfile string) ([]ImportResult, error) {
	cr := csv.NewReader(src)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	cr.Comment = '#'
	head, err := cr.Read()
	if err != nil {
		return nil, errors.New("empty CSV")
	}
	if len(head) == 1 && strings.Contains(head[0], ";") {
		return nil, errors.New("use a comma as the separator")
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	if _, ok := col["address"]; !ok {
		return nil, errors.New("no address column")
	}
	get := func(rec []string, k string) string {
		if i, ok := col[k]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []ImportResult
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			out = append(out, ImportResult{Line: line, Msg: err.Error()})
			continue
		}
		d := store.Device{Name: get(rec, "name"), Address: get(rec, "address"), Group: get(rec, "group"), Enabled: true}
		d.Port, _ = strconv.Atoi(get(rec, "port"))
		if d.Name == "" {
			d.Name = d.Address
		}
		res := ImportResult{Line: line, Name: d.Name}
		if d.Address == "" {
			res.Msg = "no address"
			out = append(out, res)
			continue
		}
		cred, err := s.importCredential(get(rec, "credential"), get(rec, "version"), get(rec, "community"), defaultCred, d.Name)
		if err != nil {
			res.Msg = err.Error()
			out = append(out, res)
			continue
		}
		d.CredentialID = cred.ID
		if p := get(rec, "profile"); p != "" {
			prof, err := s.st.ProfileByName(p)
			if err != nil {
				res.Msg = "no profile \"" + p + "\""
				out = append(out, res)
				continue
			}
			d.ProfileID = prof.ID
		} else if defaultProfile != "" {
			d.ProfileID, _ = strconv.ParseInt(defaultProfile, 10, 64)
		}
		if err := s.st.SaveDevice(&d); err != nil {
			res.Msg = err.Error()
			if strings.Contains(res.Msg, "UNIQUE") {
				res.Msg = "already exists"
			}
			out = append(out, res)
			continue
		}
		s.en.Refresh(d.ID)
		res.OK, res.Msg = true, "added, full discovery running"
		out = append(out, res)
	}
	return out, nil
}

func (s *Server) importCredential(name, version, community, def, hint string) (store.Credential, error) {
	switch {
	case name != "":
		c, err := s.st.CredentialByName(name)
		if err != nil {
			return c, errors.New("no credentials \"" + name + "\"")
		}
		return c, nil
	case community != "":
		if version == "" {
			version = "2c"
		}
		if version != "1" && version != "2c" {
			return store.Credential{}, errors.New("CSV can set a community only for v1/v2c; for v3 use the credential column")
		}
		return s.findOrCreateCredential(store.Credential{Version: version, Community: community}, hint)
	case def != "":
		id, _ := strconv.ParseInt(def, 10, 64)
		return s.st.Credential(id)
	}
	return store.Credential{}, errors.New("no credentials given")
}

// ---- Prometheus help ----

func (s *Server) prometheusHelp(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	s.render(w, r, "prometheus", map[string]any{"Base": scheme + "://" + r.Host, "Host": r.Host})
}
