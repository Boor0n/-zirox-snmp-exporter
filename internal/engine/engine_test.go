package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx/snmptest"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

func TestBackoff(t *testing.T) {
	cases := []struct {
		fails int
		want  time.Duration
	}{{0, time.Minute}, {1, time.Minute}, {2, 2 * time.Minute}, {3, 4 * time.Minute}, {4, 8 * time.Minute}, {5, 10 * time.Minute}, {50, 10 * time.Minute}}
	for _, c := range cases {
		if got := backoff(time.Minute, c.fails); got != c.want {
			t.Errorf("backoff(1m, %d) = %v, want %v", c.fails, got, c.want)
		}
	}
	if got := backoff(20*time.Minute, 3); got != 20*time.Minute {
		t.Errorf("backoff never goes below the interval, got %v", got)
	}
}

func TestResolvePriority(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "z.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, _ := secret.LoadKey("", filepath.Join(dir, "k"))
	box, _ := secret.New(key)
	e := New(st, box, Options{})
	_ = st.SaveSettings(store.Settings{IntervalSec: 90, TimeoutSec: 5, Retries: 2})
	p := store.PollProfile{Name: "fast", IntervalSec: 6, TimeoutSec: 5, Retries: 2}
	_ = st.SaveProfile(&p)

	if eff := e.Resolve(store.Device{}); eff.Interval != 90*time.Second || eff.Retries != 2 {
		t.Errorf("global: %+v", eff)
	}
	eff := e.Resolve(store.Device{ProfileID: p.ID})
	if eff.Interval != 6*time.Second {
		t.Errorf("profile: %+v", eff)
	}
	// 5s timeout x 3 tries does not fit in a 6s interval: retries dropped, timeout capped.
	if eff.Retries != 0 || eff.Timeout > 4800*time.Millisecond {
		t.Errorf("timeout not clamped: %+v", eff)
	}
	if eff := e.Resolve(store.Device{ProfileID: p.ID, IntervalSec: 30}); eff.Interval != 30*time.Second || eff.Source != "device" {
		t.Errorf("device override: %+v", eff)
	}
	if eff := e.Resolve(store.Device{IntervalSec: 1}); eff.Interval != store.MinIntervalSec*time.Second {
		t.Errorf("minimum not enforced: %+v", eff)
	}
}

func TestScheduledRefresh(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "z.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, _ := secret.LoadKey("", filepath.Join(dir, "k"))
	box, _ := secret.New(key)
	a := snmptest.Switch(2)
	now := time.Date(2026, 10, 21, 3, 0, 0, 0, time.Local)
	e := New(st, box, Options{
		Dial: func(snmpx.Target) (snmpx.Session, error) { return a.Dial() },
		Now:  func() time.Time { return now },
	})
	enc, _ := box.Encrypt("public")
	c := store.Credential{Name: "c", Version: "2c", Community: enc}
	if err := st.SaveCredential(&c); err != nil {
		t.Fatal(err)
	}
	d := store.Device{Name: "sw", Address: "10.0.0.1", Port: 161, CredentialID: c.ID, Enabled: true}
	if err := st.SaveDevice(&d); err != nil {
		t.Fatal(err)
	}
	if err := e.DiscoverNow(d.ID); err != nil {
		t.Fatal(err)
	}
	// Pretend the last discovery was two days ago.
	d, _ = st.Device(d.ID)
	now = d.LastDiscovery.Add(48 * time.Hour)
	now = time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, time.Local)

	if n := e.AutoRefreshDue(); n != 0 {
		t.Fatalf("scheduled refresh is off by default, started %d", n)
	}
	_ = st.SaveSettings(store.Settings{IntervalSec: 60, TimeoutSec: 5, AutoRefreshHours: 24, AutoRefreshFrom: 1, AutoRefreshTo: 5})
	now = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	if n := e.AutoRefreshDue(); n != 0 {
		t.Fatalf("outside the 01-05 window, started %d", n)
	}
	a.Down = true // discovery will fail and keep the old timestamp
	now = time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, time.Local)
	if n := e.AutoRefreshDue(); n != 1 {
		t.Fatalf("due device not refreshed, started %d", n)
	}
	for e.Discovering(d.ID) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := e.AutoRefreshDue(); n != 0 {
		t.Fatalf("a failed device must wait for the next period, started %d", n)
	}
	log, _ := st.AuditLog("scheduled", 10)
	if len(log) != 1 || log[0].Target != "sw" || log[0].User != "system" {
		t.Fatalf("audit log = %+v", log)
	}
}

func TestRefreshWindow(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 1, 1, h, 30, 0, 0, time.Local) }
	cases := []struct {
		from, to, h int
		want        bool
	}{{1, 5, 0, false}, {1, 5, 1, true}, {1, 5, 4, true}, {1, 5, 5, false}, {22, 4, 23, true}, {22, 4, 2, true}, {22, 4, 12, false}, {3, 3, 15, true}}
	for _, c := range cases {
		st := store.Settings{AutoRefreshFrom: c.from, AutoRefreshTo: c.to}
		if got := st.InRefreshWindow(at(c.h)); got != c.want {
			t.Errorf("%02d-%02d at %02d:30 = %v, want %v", c.from, c.to, c.h, got, c.want)
		}
	}
}
