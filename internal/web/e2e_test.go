package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/engine"
	"github.com/Boor0n/zirox-snmp-exporter/internal/exporter"
	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx/snmptest"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
	"github.com/Boor0n/zirox-snmp-exporter/internal/web"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	c      *http.Client
	st     *store.Store
	en     *engine.Engine
	agents map[string]*snmptest.Agent
}

func setup(t *testing.T) *env {
	dir := t.TempDir()
	key, err := secret.LoadKey("", filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secret.New(key)
	st, err := store.Open(filepath.Join(dir, "z.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureDefaultProfiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := web.EnsureAdmin(st, "admin", "password123", false); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, agents: map[string]*snmptest.Agent{}}
	snmpx.SourceAddress = func(string) string { return "192.0.2.10" }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.en = engine.New(st, box, engine.Options{NoJitter: true, Log: log, Probe: func(string, int, time.Duration) bool { return false }, Dial: func(tg snmpx.Target) (snmpx.Session, error) {
		a, ok := e.agents[tg.Address]
		if !ok {
			return nil, errors.New("request timeout (after 1 retries)") // nothing at this address
		}
		if tg.Cred.Community != "public" {
			return nil, errors.New("request timeout (after 1 retries)") // devices ignore a wrong community
		}
		return a.Dial()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); e.en.Wait() })
	if err := e.en.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ui, err := web.New(st, e.en, box, web.Config{}, log)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	exporter.New(st, e.en).Register(mux)
	ui.Register(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	jar, _ := cookiejar.New(nil)
	e.c = &http.Client{Jar: jar}
	return e
}

func (e *env) get(path string) (int, string) {
	e.t.Helper()
	r, err := e.c.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func (e *env) post(path string, form url.Values) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	r, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func (e *env) waitDiscovery(id int64) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := e.get(fmt.Sprintf("/devices/%d/status", id))
		if strings.Contains(body, `"discovering":false`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("discovery did not finish")
}

func (e *env) waitPoll(id int64, after time.Time) *engine.Snapshot {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := e.en.Snapshot(id); s != nil && s.At.After(after) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("no poll happened")
	return nil
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)
	e.agents["10.0.0.1"] = snmptest.Switch(12)

	// The UI requires login.
	if code, body := e.get("/"); code != 200 || !strings.Contains(body, `name="password"`) {
		t.Fatalf("expected login page, got %d", code)
	}
	if code, body := e.post("/login", url.Values{"user": {"admin"}, "password": {"wrong"}}); code != 200 || !strings.Contains(body, "Wrong username") {
		t.Fatalf("bad password accepted: %d", code)
	}
	e.post("/login", url.Values{"user": {"admin"}, "password": {"password123"}})

	// Test connection with a wrong community, then add the device.
	devForm := url.Values{"address": {"10.0.0.1"}, "name": {"core-nx-01"}, "port": {"161"}, "enabled": {"on"},
		"credential_id": {"new"}, "version": {"2c"}, "community": {"nope"}, "profile_id": {"0"}}
	if _, body := e.post("/devices/test", devForm); !strings.Contains(body, "Stopped at <b>SNMP answer</b>") || !strings.Contains(body, "permit host 192.0.2.10") {
		t.Fatal("test with wrong community should fail")
	}
	devForm.Set("community", "public")
	if _, body := e.post("/devices/test", devForm); !strings.Contains(body, "Zirox can reach and read this device (core-nx-01)") {
		t.Fatalf("test connection failed: %s", flash(body))
	}
	if _, body := e.post("/devices/new", devForm); !strings.Contains(body, "core-nx-01") {
		t.Fatalf("device not created: %s", flash(body))
	}
	e.waitDiscovery(1)

	// Community is encrypted at rest.
	creds, _ := e.st.Credentials()
	if len(creds) != 1 || !strings.HasPrefix(creds[0].Community, "enc:v1:") || strings.Contains(creds[0].Community, "public") {
		t.Fatalf("credential not encrypted: %+v", creds)
	}

	_, card := e.get("/devices/1")
	for _, want := range []string{"Ethernet1/3", "Vlan10", "Supervisor CPU", "Module 1 Inlet Temp", "Ports", "Sensors"} {
		if !strings.Contains(card, want) {
			t.Errorf("device card lacks %q", want)
		}
	}

	if !strings.Contains(card, "<title>core-nx-01 · Zirox SNMP</title>") || !strings.Contains(card, "data-sel-count") {
		t.Error("device card lacks its tab title or the live selection counter")
	}
	if _, list := e.get("/devices?status=ok"); !strings.Contains(list, "<title>Devices · Zirox SNMP</title>") ||
		!strings.Contains(list, "data-sortable") || !strings.Contains(list, `class="stat st-ok on"`) {
		t.Error("device list lacks its title, sortable columns or the active status chip")
	}

	// Tick only Ethernet1/5 and the CPU, then apply.
	objs, _ := e.st.Objects(1)
	form := url.Values{}
	for _, o := range objs {
		if o.Key == "Ethernet1/5" || o.Key == "Supervisor CPU" {
			form.Add("obj", fmt.Sprint(o.ID))
		}
	}
	start := time.Now()
	if _, body := e.post("/devices/1/selection", form); !strings.Contains(body, "polling 2 objects") {
		t.Fatalf("selection not applied: %s", flash(body))
	}
	snap := e.waitPoll(1, start)
	if !snap.OK {
		t.Fatalf("poll failed: %s", snap.Err)
	}

	_, metrics := e.get("/probe?target=1")
	for _, want := range []string{
		`ifHCInOctets{ifAlias="to Ethernet1/5",ifDescr="Ethernet1/5",ifIndex="5",ifName="Ethernet1/5",role=""} 5000`,
		`zirox_cpu_utilization_percent{index="1",name="Supervisor CPU",window="5m"} 10`,
		`zirox_poll_success 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("probe lacks %q\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, `ifName="Ethernet1/3"`) || strings.Contains(metrics, "zirox_temperature_celsius{") {
		t.Error("unselected objects must not be exported")
	}
	// Scrapes read the cache only: no SNMP traffic.
	e.agents["10.0.0.1"].Counts()
	e.get("/probe?target=core-nx-01")
	e.get("/metrics/all")
	if g, w, _ := e.agents["10.0.0.1"].Counts(); g+w != 0 {
		t.Errorf("scrape caused %d SNMP requests", g+w)
	}

	// HTTP SD: global interval first, then a per-device override.
	sd := e.sd()
	if sd["__scrape_interval__"] != "60s" || sd["__param_target"] != "1" || sd["instance"] != "core-nx-01" {
		t.Fatalf("sd = %v", sd)
	}
	devForm.Set("credential_id", fmt.Sprint(creds[0].ID))
	devForm.Set("interval_sec", "6")
	e.post("/devices/1/edit", devForm)
	if sd := e.sd(); sd["__scrape_interval__"] != "6s" {
		t.Fatalf("device interval not applied: %v", sd)
	}
	// Profile beats global, device beats profile.
	profs, _ := e.st.Profiles()
	devForm.Set("interval_sec", "")
	devForm.Set("profile_id", fmt.Sprint(profs[2].ID)) // Slow (300 s)
	e.post("/devices/1/edit", devForm)
	if sd := e.sd(); sd["__scrape_interval__"] != "300s" {
		t.Fatalf("profile interval not applied: %v", sd)
	}
	e.post("/profiles", url.Values{"id": {fmt.Sprint(profs[2].ID)}, "name": {profs[2].Name}, "interval_sec": {"120"}, "timeout_sec": {"5"}, "retries": {"1"}, "auto_select_up": {"on"}})
	if sd := e.sd(); sd["__scrape_interval__"] != "120s" {
		t.Fatalf("profile change not applied: %v", sd)
	}

	// Refresh picks up a new port, keeps the old selection.
	snmptest.AddPort(e.agents["10.0.0.1"], 40, "Ethernet1/40", 6, true)
	e.post("/devices/1/refresh", nil)
	e.waitDiscovery(1)
	_, card = e.get("/devices/1")
	if !strings.Contains(card, "Ethernet1/40") || !strings.Contains(card, "new (highlighted)") {
		t.Error("refresh did not show the new port")
	}
	objs, _ = e.st.Objects(1)
	for _, o := range objs {
		want := o.Key == "Ethernet1/5" || o.Key == "Supervisor CPU" || o.Key == "Ethernet1/40"
		if o.Selected != want {
			t.Errorf("%s selected=%v after refresh, want %v", o.Key, o.Selected, want)
		}
	}

	// Cross-site posts are rejected.
	req, _ := http.NewRequest("POST", e.srv.URL+"/devices/1/delete", nil)
	req.Header.Set("Origin", "http://evil.example")
	if r, _ := e.c.Do(req); r.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site delete got %d", r.StatusCode)
	}

	// CSV import.
	e.agents["10.0.0.2"] = snmptest.Switch(2)
	_, body := e.post("/import", url.Values{"csv": {"name,address,group,version,community\nacc-1,10.0.0.2,DC1,2c,public\nbad,,DC1,2c,public\n"}})
	if !strings.Contains(body, "Added: 1 of 2") {
		t.Fatalf("import: %s", flash(body))
	}
	if creds, _ := e.st.Credentials(); len(creds) != 1 {
		t.Errorf("identical community should reuse the credential, have %d", len(creds))
	}

	// Overview, global search and live port numbers.
	if _, page := e.get("/"); !strings.Contains(page, "<h1>Overview</h1>") || !strings.Contains(page, "Needs attention") || !strings.Contains(page, "device added") {
		t.Error("overview lacks its sections or recent events")
	}
	if _, page := e.get("/search?q=Ethernet1/40"); !strings.Contains(page, "core-nx-01") || !strings.Contains(page, "Ethernet1/40") {
		t.Error("search did not find the port")
	}
	if r, err := e.c.Get(e.srv.URL + "/search?q=10.0.0.1"); err != nil || !strings.HasSuffix(r.Request.URL.Path, "/devices/1") {
		t.Error("an exact device address should open the device")
	}
	if _, body := e.get("/devices/1/live"); !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"ports":{`) {
		t.Errorf("live = %s", body)
	}
	_, card = e.get("/devices/1")
	if !strings.Contains(card, "data-tabs") || !strings.Contains(card, `class="avail"`) || !strings.Contains(card, "data-live-in") {
		t.Error("device card lacks tabs, availability or live columns")
	}

	// Bulk edit: group and profile for two devices; credentials kept.
	_, body = e.post("/devices/bulk", url.Values{"ids": {"1", "2"}, "action": {"edit"}, "set_group": {"1"}, "group": {"Core"}, "profile_id": {"0"}, "credential_id": {"keep"}})
	if !strings.Contains(body, "Changed: 2 devices") {
		t.Fatalf("bulk edit: %s", flash(body))
	}
	for _, id := range []int64{1, 2} {
		d, _ := e.st.Device(id)
		if d.Group != "Core" || d.ProfileID != 0 || d.CredentialID != creds[0].ID {
			t.Errorf("bulk edit left %+v", d)
		}
	}
}

func (e *env) sd() map[string]string {
	e.t.Helper()
	_, body := e.get("/sd")
	var out []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out) == 0 {
		e.t.Fatalf("sd: %v %s", err, body)
	}
	return out[0].Labels
}

var flashRe = regexp.MustCompile(`class="flash [a-z]+">([^<]*)<`)

func flash(body string) string {
	m := flashRe.FindAllStringSubmatch(body, -1)
	var out []string
	for _, x := range m {
		out = append(out, x[1])
	}
	return strings.Join(out, " | ")
}

func TestChangeLogAndScheduledRefreshSettings(t *testing.T) {
	e := setup(t)
	e.agents["10.0.0.1"] = snmptest.Switch(4)
	e.post("/login", url.Values{"user": {"admin"}, "password": {"wrong"}})
	e.post("/login", url.Values{"user": {"admin"}, "password": {"password123"}})

	devForm := url.Values{"address": {"10.0.0.1"}, "name": {"sw1"}, "port": {"161"}, "enabled": {"on"},
		"credential_id": {"new"}, "version": {"2c"}, "community": {"public"}, "profile_id": {"0"}}
	e.post("/devices/new", devForm)
	e.waitDiscovery(1)
	objs, _ := e.st.Objects(1)
	form := url.Values{}
	for _, o := range objs {
		if o.Key == "Ethernet1/2" || (o.Selected && o.Key != "Ethernet1/1") {
			form.Add("obj", fmt.Sprint(o.ID))
		}
	}
	e.post("/devices/1/selection", form)
	devForm.Set("address", "10.0.0.1")
	devForm.Set("credential_id", fmt.Sprint(mustDevice(t, e.st, 1).CredentialID))
	devForm.Set("group", "DC1")
	e.post("/devices/1/edit", devForm)

	if _, body := e.post("/settings", url.Values{"interval_sec": {"60"}, "timeout_sec": {"5"}, "retries": {"1"},
		"auto_refresh_hours": {"168"}, "auto_refresh_from": {"1"}, "auto_refresh_to": {"5"}}); !strings.Contains(body, "Global settings applied") {
		t.Fatalf("settings not saved: %s", flash(body))
	}
	if st := e.st.Settings(); st.AutoRefreshHours != 168 || st.AutoRefreshFrom != 1 || st.AutoRefreshTo != 5 {
		t.Fatalf("scheduled refresh settings = %+v", st)
	}
	e.post("/devices/1/delete", nil)

	_, page := e.get("/audit")
	for _, want := range []string{
		"login failed", "device added", "selection changed", "+ Ethernet1/2", "− Ethernet1/1",
		"group:  → DC1", "settings changed", "scheduled refresh every 168 h, 01:00-05:00", "device deleted",
	} {
		if !strings.Contains(html.UnescapeString(page), want) {
			t.Errorf("change log lacks %q", want)
		}
	}
	if _, page := e.get("/audit?q=DC1"); strings.Contains(page, "device added") || !strings.Contains(page, "device changed") {
		t.Error("change log search does not filter")
	}
}

func mustDevice(t *testing.T, st *store.Store, id int64) store.Device {
	t.Helper()
	d, err := st.Device(id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) api(method, path, token, body string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

var tokenRe = regexp.MustCompile(`zx_[A-Za-z0-9_-]+`)

func TestUsersRolesAndAPI(t *testing.T) {
	e := setup(t)
	e.agents["10.0.0.1"] = snmptest.Switch(4)
	e.post("/login", url.Values{"user": {"admin"}, "password": {"password123"}})

	// The only admin cannot be demoted.
	if _, body := e.post("/users/1", url.Values{"role": {"viewer"}}); !strings.Contains(body, "at least one administrator") {
		t.Fatalf("last admin demoted: %s", flash(body))
	}
	if _, body := e.post("/users", url.Values{"name": {"olga"}, "role": {"viewer"}, "password": {"viewerpass1"}, "confirm": {"viewerpass1"}}); !strings.Contains(body, "User olga added") {
		t.Fatalf("user not added: %s", flash(body))
	}
	_, page := e.post("/tokens", url.Values{"name": {"ansible"}, "role": {"admin"}})
	adminTok := tokenRe.FindString(page)
	_, page = e.post("/tokens", url.Values{"name": {"grafana"}, "role": {"viewer"}})
	viewTok := tokenRe.FindString(page)
	if adminTok == "" || viewTok == "" || adminTok == viewTok {
		t.Fatalf("tokens not shown: %q %q", adminTok, viewTok)
	}
	if _, page := e.get("/users"); strings.Contains(page, adminTok) {
		t.Fatal("token must be shown only once")
	}

	// API: auth and roles.
	if code, _ := e.api("GET", "/api/v1/devices", "", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := e.api("GET", "/api/v1/devices", "zx_wrong", ""); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	if code, body := e.api("GET", "/api/v1/devices", viewTok, ""); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("viewer list: %d %s", code, body)
	}
	newDev := `{"name":"sw1","address":"10.0.0.1","group":"DC1","version":"2c","community":"public","profile":"Fast (6 s)"}`
	if code, _ := e.api("POST", "/api/v1/devices", viewTok, newDev); code != 403 {
		t.Fatalf("viewer token wrote: %d", code)
	}
	code, body := e.api("POST", "/api/v1/devices", adminTok, newDev)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var dev struct {
		ID      int64 `json:"id"`
		Profile string
		Objects []struct {
			Key      string
			Selected bool
		}
	}
	_ = json.Unmarshal([]byte(body), &dev)
	if dev.Profile != "Fast (6 s)" {
		t.Fatalf("profile not set: %s", body)
	}
	e.waitDiscovery(dev.ID)
	if code, body := e.api("POST", fmt.Sprintf("/api/v1/devices/%d/selection", dev.ID), adminTok, `{"select":["Ethernet1/2"],"unselect":["Ethernet1/1"]}`); code != 200 {
		t.Fatalf("selection: %d %s", code, body)
	}
	if code, _ := e.api("POST", fmt.Sprintf("/api/v1/devices/%d/selection", dev.ID), adminTok, `{"select":["Ethernet9/9"]}`); code != 400 {
		t.Fatalf("unknown object accepted: %d", code)
	}
	_, body = e.api("GET", fmt.Sprintf("/api/v1/devices/%d", dev.ID), viewTok, "")
	_ = json.Unmarshal([]byte(body), &dev)
	sel := map[string]bool{}
	for _, o := range dev.Objects {
		sel[o.Key] = o.Selected
	}
	if !sel["Ethernet1/2"] || sel["Ethernet1/1"] || !sel["Ethernet1/3"] {
		t.Fatalf("selection = %v", sel)
	}
	if code, body := e.api("PATCH", fmt.Sprintf("/api/v1/devices/%d", dev.ID), adminTok, `{"group":"DC2","interval_sec":30}`); code != 200 || !strings.Contains(body, `"group": "DC2"`) || !strings.Contains(body, `"effective_interval_sec": 30`) {
		t.Fatalf("patch: %d %s", code, body)
	}
	if code, _ := e.api("PATCH", fmt.Sprintf("/api/v1/devices/%d", dev.ID), adminTok, `{"bogus":1}`); code != 400 {
		t.Fatalf("unknown field accepted: %d", code)
	}

	// Read-only user: sees devices, cannot change them, can change own password.
	e.post("/logout", nil)
	e.post("/login", url.Values{"user": {"olga"}, "password": {"viewerpass1"}})
	if code, page := e.get("/devices"); code != 200 || !strings.Contains(page, "sw1") || strings.Contains(page, "+ Add device") || !strings.Contains(page, "read-only") {
		t.Fatalf("viewer device list: %d", code)
	}
	if code, _ := e.post(fmt.Sprintf("/devices/%d/delete", dev.ID), nil); code != 403 {
		t.Fatalf("viewer deleted a device: %d", code)
	}
	if code, _ := e.get("/users"); code != 403 {
		t.Fatalf("viewer opened users: %d", code)
	}
	if _, body := e.post("/settings/password", url.Values{"current": {"viewerpass1"}, "new": {"viewerpass2"}, "confirm": {"viewerpass2"}}); !strings.Contains(body, "Password changed") {
		t.Fatalf("viewer password change: %s", flash(body))
	}
	if u, _ := e.st.UserByName("olga"); u.Role != store.RoleViewer {
		t.Fatalf("password change altered the role: %+v", u)
	}

	if code, _ := e.api("DELETE", fmt.Sprintf("/api/v1/devices/%d", dev.ID), adminTok, ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	log, _ := e.st.AuditLog("api:ansible", 20)
	if len(log) < 4 {
		t.Fatalf("API changes not in the change log: %+v", log)
	}
}

func TestCustomOIDs(t *testing.T) {
	e := setup(t)
	t.Cleanup(func() { snmpx.SetCustom(nil) })
	a := snmptest.Switch(2)
	a.Set("1.3.6.1.4.1.99999.1.1.2.7", "pool-a")
	a.Set("1.3.6.1.4.1.99999.1.1.3.7", 42)
	e.agents["10.0.0.1"] = a
	e.post("/login", url.Values{"user": {"admin"}, "password": {"password123"}})
	e.post("/devices/new", url.Values{"address": {"10.0.0.1"}, "name": {"sw1"}, "port": {"161"}, "enabled": {"on"},
		"credential_id": {"new"}, "version": {"2c"}, "community": {"public"}, "profile_id": {"0"}})
	e.waitDiscovery(1)

	// Check the OID on the device first.
	_, page := e.get("/custom-oids?check=1&device=1&walk=1&oid=.1.3.6.1.4.1.99999.1.1")
	if !strings.Contains(page, "1.3.6.1.4.1.99999.1.1.3.7") || !strings.Contains(page, "pool-a") || !strings.Contains(page, "not a number") {
		t.Fatalf("checker did not walk the subtree: %s", flash(page))
	}
	if _, page := e.get("/custom-oids?check=1&device=1&oid=1.3.6.1.4.1.99999.5.0"); !strings.Contains(page, "returned nothing") {
		t.Error("checker must say when an OID is absent")
	}

	// A bad form keeps what was typed.
	form := url.Values{"oid": {"1.3.6.1.4.1.99999.1.1.3"}, "mode": {"table"}, "name_oid": {"1.3.6.1.4.1.99999.1.1.2"},
		"name": {"ifHCInOctets"}, "help": {"Pool used"}, "type": {"gauge"}, "scale": {"1024"}, "auto_select": {"on"}}
	if _, page := e.post("/custom-oids", form); !strings.Contains(page, "already exported") || !strings.Contains(page, `value="1.3.6.1.4.1.99999.1.1.2"`) {
		t.Fatalf("expected an error with the form kept: %s", flash(page))
	}
	form.Set("name", "vendor_pool_used_bytes")
	if _, page := e.post("/custom-oids", form); !strings.Contains(page, "Saved") {
		t.Fatalf("custom OID not saved: %s", flash(page))
	}

	// Refresh finds it and polling exports it.
	e.post("/custom-oids/refresh-all", nil)
	e.waitDiscovery(1)
	if _, page := e.get("/devices/1"); !strings.Contains(page, "<h2>Custom") || !strings.Contains(page, "pool-a") {
		t.Fatal("device card lacks the Custom section")
	}
	start := time.Now()
	e.en.Reload(1)
	e.waitPoll(1, start)
	_, metrics := e.get("/probe?target=1")
	if !strings.Contains(metrics, `vendor_pool_used_bytes{index="7",name="pool-a"} 43008`) {
		t.Fatalf("metric missing from /probe:\n%s", metrics)
	}

	// Deleting it removes its objects from every device.
	c, _ := e.st.CustomOIDs()
	e.post("/custom-oids/"+fmt.Sprint(c[0].ID)+"/delete", nil)
	if objs, _ := e.st.Objects(1); len(objs) == 0 {
		t.Fatal("device lost its other objects")
	} else {
		for _, o := range objs {
			if strings.HasPrefix(o.Kind, "custom_") {
				t.Fatal("objects of a deleted custom OID remain")
			}
		}
	}
	_, page = e.get("/audit")
	for _, want := range []string{"custom OID added", "refresh all devices", "custom OID deleted"} {
		if !strings.Contains(page, want) {
			t.Errorf("change log lacks %q", want)
		}
	}
}

func (e *env) waitJob(path, doneMarker string) string {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, body := e.get(path)
		if strings.Contains(body, doneMarker) {
			return body
		}
		time.Sleep(30 * time.Millisecond)
	}
	e.t.Fatalf("%s did not finish", path)
	return ""
}

func TestDiagnosticsCheckAndScan(t *testing.T) {
	e := setup(t)
	e.agents["10.0.0.1"] = snmptest.Switch(4)
	e.post("/login", url.Values{"user": {"admin"}, "password": {"password123"}})
	devForm := url.Values{"address": {"10.0.0.1"}, "name": {"sw1"}, "port": {"161"}, "enabled": {"on"},
		"credential_id": {"new"}, "version": {"2c"}, "community": {"public"}, "profile_id": {"0"}}
	e.post("/devices/new", devForm)
	e.waitDiscovery(1)
	if d := mustDevice(t, e.st, 1); d.Model != "Cisco Nexus 9000" {
		t.Errorf("model = %q", d.Model)
	}
	// A second device that does not answer.
	devForm.Set("address", "10.0.0.9")
	devForm.Set("name", "dead")
	e.post("/devices/new", devForm)
	e.waitDiscovery(2)

	if _, body := e.get("/devices/1/diagnose"); !strings.Contains(body, "Zirox can reach and read this device") || !strings.Contains(body, "5 interfaces") {
		t.Fatalf("diagnostics of a healthy device: %s", flash(body))
	}
	if _, body := e.get("/devices/2/diagnose"); !strings.Contains(body, "Stopped at <b>SNMP answer</b>") {
		t.Fatalf("diagnostics of a dead device: %s", flash(body))
	}

	// Bulk check: the dead device is listed for the ACL.
	req, _ := http.NewRequest("POST", e.srv.URL+"/devices/bulk", strings.NewReader("ids=1&ids=2&action=check"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	jobPath := resp.Request.URL.Path
	if !strings.HasPrefix(jobPath, "/checks/") {
		t.Fatalf("bulk check went to %s", jobPath)
	}
	body := e.waitJob(jobPath, "Export CSV")
	if !strings.Contains(body, "Reachable <b>1</b>") || !strings.Contains(body, "Problems <b>1</b>") || !strings.Contains(body, "10.0.0.9\n") {
		t.Errorf("check page: %s", body)
	}
	if _, csvBody := e.get(jobPath + "/csv"); !strings.Contains(csvBody, "dead,10.0.0.9,,fail,SNMP answer,timeout,192.0.2.10") {
		t.Errorf("csv: %s", csvBody)
	}

	// Subnet scan of 10.0.0.0/29 finds the existing switch and a new one.
	e.agents["10.0.0.5"] = snmptest.Switch(2)
	e.agents["10.0.0.5"].Set("1.3.6.1.2.1.1.5.0", "acc-sw-05.example.net")
	scanPath, _ := e.postFollow("/scan", url.Values{"subnet": {"10.0.0.0/29"}, "cred": {fmt.Sprint(mustDevice(t, e.st, 1).CredentialID)}, "group": {"HQ2"}})
	if !strings.HasPrefix(scanPath, "/scan/") {
		t.Fatalf("scan went to %s", scanPath)
	}
	body = e.waitJob(scanPath, "Add selected")
	if !strings.Contains(body, "<b>2</b> addresses answered SNMP (1 not in Zirox yet)") || !strings.Contains(body, "(as sw1)") {
		t.Fatalf("scan results: %s", body)
	}
	e.post(scanPath+"/add", url.Values{"addr": {"10.0.0.5", "10.0.0.1"}, "group": {"HQ2"}})
	devs, _ := e.st.Devices()
	var added *store.Device
	for i := range devs {
		if devs[i].Address == "10.0.0.5" {
			added = &devs[i]
		}
	}
	if len(devs) != 3 || added == nil || added.Name != "acc-sw-05" || added.Group != "HQ2" {
		t.Fatalf("devices after scan: %+v", devs)
	}
	if _, body := e.post("/scan", url.Values{"subnet": {"10.0.0.0/16"}, "cred": {"1"}}); !strings.Contains(body, "too large") {
		t.Errorf("a /16 must be refused: %s", flash(body))
	}
}

func (e *env) postFollow(path string, form url.Values) (string, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	r, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.Request.URL.Path, string(b)
}

func TestPortRolesRulesAndFastPolling(t *testing.T) {
	e := setup(t)
	e.agents["10.0.0.1"] = snmptest.Switch(4)
	e.post("/login", url.Values{"user": {"admin"}, "password": {"password123"}})
	e.post("/devices/new", url.Values{"address": {"10.0.0.1"}, "name": {"sw1"}, "port": {"161"}, "enabled": {"on"},
		"credential_id": {"new"}, "version": {"2c"}, "community": {"public"}, "profile_id": {"0"}})
	e.waitDiscovery(1)

	// Uplinks every 5 s; the device itself stays at the global 60 s.
	st := e.st.Settings()
	if st.FastIntervalSec != 10 {
		t.Errorf("default uplink interval = %d, want 10", st.FastIntervalSec)
	}
	e.post("/settings", url.Values{"interval_sec": {"60"}, "timeout_sec": {"5"}, "retries": {"1"}, "auto_select_up": {"on"}, "fast_interval_sec": {"5"}})
	if got := e.st.Settings().FastIntervalSec; got != 5 {
		t.Fatalf("uplink interval not saved: %d", got)
	}

	// Mark Ethernet1/1 as uplink on the device page.
	objs, _ := e.st.Objects(1)
	form := url.Values{}
	var eth1 int64
	for _, o := range objs {
		if o.Selected {
			form.Add("obj", fmt.Sprint(o.ID))
		}
		if o.Key == "Ethernet1/1" {
			eth1 = o.ID
		}
	}
	form.Set(fmt.Sprintf("role_%d", eth1), "uplink")
	start := time.Now()
	e.post("/devices/1/selection", form)
	if _, card := e.get("/devices/1"); !strings.Contains(card, `name="role_`) || !strings.Contains(card, `<option selected>uplink</option>`) {
		t.Error("device page lacks the role selector")
	}
	e.waitPoll(1, start)
	deadline := time.Now().Add(5 * time.Second)
	for e.en.FastSnapshot(1) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fs := e.en.FastSnapshot(1); fs == nil || !fs.OK || fs.Interval != 5*time.Second {
		t.Fatalf("fast snapshot = %+v", fs)
	}

	_, fast := e.get("/probe?target=1&poll=fast")
	if !strings.Contains(fast, `ifHCInOctets{ifAlias="to Ethernet1/1",ifDescr="Ethernet1/1",ifIndex="1",ifName="Ethernet1/1",role="uplink"}`) ||
		!strings.Contains(fast, "zirox_fast_poll_success 1") || strings.Contains(fast, `ifName="Ethernet1/3"`) {
		t.Errorf("fast probe:\n%s", fast)
	}
	_, normal := e.get("/probe?target=1")
	if strings.Contains(normal, `ifHCInOctets{ifAlias="to Ethernet1/1"`) || !strings.Contains(normal, `ifName="Ethernet1/3"`) {
		t.Errorf("normal probe must leave the uplink to the fast target:\n%s", normal)
	}
	_, all := e.get("/metrics/all")
	if !strings.Contains(all, `ifName="Ethernet1/1",role="uplink"`) {
		t.Error("/metrics/all lacks the fast uplink")
	}
	_, body := e.get("/sd")
	var sd []struct {
		Labels map[string]string `json:"labels"`
	}
	_ = json.Unmarshal([]byte(body), &sd)
	if len(sd) != 2 || sd[1].Labels["__param_poll"] != "fast" || sd[1].Labels["__scrape_interval__"] != "5s" || sd[1].Labels["poll"] != "fast" {
		t.Errorf("sd = %s", body)
	}

	// A rule ticks VLANs as core; applying it changes the one existing VLAN.
	if _, page := e.post("/rules", url.Values{"field": {"name"}, "pattern": {"^Vlan"}, "action": {"poll"}, "role": {"core"}}); !strings.Contains(page, "Rule saved") {
		t.Fatalf("rule not saved: %s", flash(page))
	}
	if _, page := e.post("/rules", url.Values{"field": {"name"}, "pattern": {"("}, "action": {"poll"}}); !strings.Contains(page, "pattern") {
		t.Errorf("bad pattern accepted: %s", flash(page))
	}
	if _, page := e.get("/rules"); !strings.Contains(page, "<b>1</b> ports on 1 devices") || !strings.Contains(page, "sw1 Vlan10") {
		t.Error("rule preview missing")
	}
	if _, page := e.post("/rules/apply", nil); !strings.Contains(page, "Changed 1 ports on 1 devices") {
		t.Errorf("apply: %s", flash(page))
	}
	objs, _ = e.st.Objects(1)
	for _, o := range objs {
		if o.Key == "Vlan10" && (!o.Selected || o.Role != "core") {
			t.Errorf("Vlan10 = %+v", o)
		}
	}
	_, page := e.get("/audit")
	for _, want := range []string{"port roles changed", "port rule added", "port rules applied"} {
		if !strings.Contains(page, want) {
			t.Errorf("change log lacks %q", want)
		}
	}
}
