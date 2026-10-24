package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx/snmptest"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// TestScale imports many 48-port switches with simulated network latency,
// discovers them all and checks every device is polled within its interval.
// It runs with ZIROX_SCALE=<devices> (CI uses a small number).
func TestScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("ZIROX_SCALE"))
	if n <= 0 {
		t.Skip("ZIROX_SCALE not set")
	}
	delay := 20 * time.Millisecond
	if v := os.Getenv("ZIROX_SCALE_DELAY"); v != "" {
		delay, _ = time.ParseDuration(v)
	}
	interval := 10 // seconds

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "z.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, _ := secret.LoadKey("", filepath.Join(dir, "k"))
	box, _ := secret.New(key)
	agents := map[string]*snmptest.Agent{}
	maxDisc, _ := strconv.Atoi(os.Getenv("ZIROX_MAX_DISCOVERY"))
	e := New(st, box, Options{
		MaxConcurrentDiscovery: maxDisc,
		Log:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dial: func(tg snmpx.Target) (snmpx.Session, error) {
			return agents[tg.Address].Dial()
		},
	})
	_ = st.SaveSettings(store.Settings{IntervalSec: interval, TimeoutSec: 2, Retries: 0, AutoSelectUp: true})
	enc, _ := box.Encrypt("public")
	c := store.Credential{Name: "c", Version: "2c", Community: enc}
	if err := st.SaveCredential(&c); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := 0; i < n; i++ {
		addr := fmt.Sprintf("10.%d.%d.%d", i/65536, i/256%256, i%256)
		a := snmptest.Switch(48)
		a.Delay = delay
		agents[addr] = a
		d := store.Device{Name: "sw-" + addr, Address: addr, Port: 161, CredentialID: c.ID, Enabled: true}
		if err := st.SaveDevice(&d); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); e.Wait() }()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Full discovery of every device, as after a CSV import.
	start := time.Now()
	for _, id := range ids {
		e.Refresh(id)
	}
	for e.AnyDiscovering() {
		time.Sleep(50 * time.Millisecond)
	}
	discovery := time.Since(start)
	counts, _ := st.ObjectCounts()
	objects, selected := 0, 0
	for _, id := range ids {
		d, _ := st.Device(id)
		if d.LastDiscovery.IsZero() || d.DiscoveryError != "" {
			t.Fatalf("%s not discovered: %q", d.Name, d.DiscoveryError)
		}
		objects += counts[id][1]
		selected += counts[id][0]
	}

	// Every device must be polled within one interval from now on.
	mark := time.Now()
	deadline := mark.Add(time.Duration(interval)*time.Second + 2*time.Second)
	var late []int64
	for time.Now().Before(deadline) {
		late = late[:0]
		for _, id := range ids {
			if s := e.Snapshot(id); s == nil || !s.At.After(mark) || !s.OK {
				late = append(late, id)
			}
		}
		if len(late) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cycle := time.Since(mark)
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	t.Logf("%d devices, %d objects (%d selected), %v latency per request", n, objects, selected, delay)
	t.Logf("discovery of all devices: %v (%d at a time)", discovery.Round(time.Millisecond), e.MaxConcurrentDiscovery())
	t.Logf("all devices polled again within %v (interval %d s)", cycle.Round(time.Millisecond), interval)
	t.Logf("goroutines %d, heap %d MB", runtime.NumGoroutine(), ms.HeapAlloc>>20)
	if len(late) > 0 {
		t.Fatalf("%d of %d devices not polled within the interval", len(late), n)
	}
}
