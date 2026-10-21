package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
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
