package web_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.en = engine.New(st, box, engine.Options{NoJitter: true, Log: log, Dial: func(tg snmpx.Target) (snmpx.Session, error) {
		a, ok := e.agents[tg.Address]
		if !ok {
			return nil, fmt.Errorf("no route to %s", tg.Address)
		}
		if tg.Cred.Community != "public" {
			return nil, fmt.Errorf("wrong community %q", tg.Cred.Community)
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
	if _, body := e.post("/devices/test", devForm); !strings.Contains(body, "No response") {
		t.Fatal("test with wrong community should fail")
	}
	devForm.Set("community", "public")
	if _, body := e.post("/devices/test", devForm); !strings.Contains(body, "Device answered: core-nx-01") {
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
		`ifHCInOctets{ifAlias="to Ethernet1/5",ifDescr="Ethernet1/5",ifIndex="5",ifName="Ethernet1/5"} 5000`,
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
