// Package engine schedules discovery and periodic polls and keeps the latest
// values in memory for Prometheus.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// maxBackoff caps how far polling of an unreachable device slows down.
const maxBackoff = 10 * time.Minute

// Snapshot is the latest poll result of one device.
type Snapshot struct {
	Samples  []snmpx.Sample
	At       time.Time
	LastOK   time.Time
	Duration time.Duration
	OK       bool
	Err      string
	Fails    int
	Interval time.Duration
}

// Effective is a device's resolved poll configuration.
type Effective struct {
	Interval     time.Duration
	Timeout      time.Duration
	Retries      int
	AutoSelectUp bool
	Source       string // "device", profile name or "global"
}

// Options configures the engine.
type Options struct {
	MaxConcurrentPolls     int
	MaxConcurrentDiscovery int
	Dial                   snmpx.Dialer
	Log                    *slog.Logger
	// NoJitter starts the first poll right away instead of at a random
	// point of the interval (used by tests).
	NoJitter bool
	// AutoRefreshCheck is how often the scheduled refresh looks for due
	// devices (default one minute).
	AutoRefreshCheck time.Duration
	// Now replaces time.Now for the scheduled refresh (used by tests).
	Now func() time.Time
}

// Engine runs one poll loop per enabled device.
type Engine struct {
	st   *store.Store
	box  *secret.Box
	dial snmpx.Dialer
	log  *slog.Logger

	noJitter bool
	arCheck  time.Duration
	now      func() time.Time

	pollSem chan struct{}
	discSem chan struct{}

	mu        sync.Mutex
	ctx       context.Context
	loops     map[int64]context.CancelFunc
	snaps     map[int64]*Snapshot
	locks     map[int64]*sync.Mutex
	running   map[int64]bool // discovery in progress
	autoTried map[int64]time.Time
	wg        sync.WaitGroup
	discCount int
}

// New creates an engine. Call Start to begin polling.
func New(st *store.Store, box *secret.Box, o Options) *Engine {
	if o.MaxConcurrentPolls <= 0 {
		o.MaxConcurrentPolls = 64
	}
	if o.MaxConcurrentDiscovery <= 0 {
		o.MaxConcurrentDiscovery = 8
	}
	if o.Dial == nil {
		o.Dial = snmpx.Dial
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.AutoRefreshCheck <= 0 {
		o.AutoRefreshCheck = time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Engine{
		st: st, box: box, dial: o.Dial, log: o.Log, noJitter: o.NoJitter,
		arCheck: o.AutoRefreshCheck, now: o.Now,
		autoTried: map[int64]time.Time{},
		pollSem:   make(chan struct{}, o.MaxConcurrentPolls),
		discSem:   make(chan struct{}, o.MaxConcurrentDiscovery),
		loops:     map[int64]context.CancelFunc{},
		snaps:     map[int64]*Snapshot{},
		locks:     map[int64]*sync.Mutex{},
		running:   map[int64]bool{},
	}
}

// Start launches poll loops for all enabled devices.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	e.ctx = ctx
	e.mu.Unlock()
	e.wg.Add(1)
	go e.autoRefreshLoop(ctx)
	return e.ReloadAll()
}

// autoRefreshLoop re-runs full discovery of devices whose last discovery is
// older than the configured period, so new ports are picked up without a
// click. Concurrency is bounded by the discovery semaphore.
func (e *Engine) autoRefreshLoop(ctx context.Context) {
	defer e.wg.Done()
	t := time.NewTicker(e.arCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.AutoRefreshDue()
		}
	}
}

// AutoRefreshDue starts discovery of every device that is due for a
// scheduled refresh and returns how many were started.
func (e *Engine) AutoRefreshDue() int {
	st := e.st.Settings()
	now := e.now()
	if st.AutoRefreshHours <= 0 || !st.InRefreshWindow(now) {
		return 0
	}
	period := time.Duration(st.AutoRefreshHours) * time.Hour
	devs, err := e.st.Devices()
	if err != nil {
		e.log.Error("scheduled refresh", "err", err)
		return 0
	}
	started := 0
	for _, d := range devs {
		if !d.Enabled || d.LastDiscovery.IsZero() || now.Sub(d.LastDiscovery) < period {
			continue
		}
		// A device that failed discovery keeps its old timestamp; do not
		// retry it before the next period.
		e.mu.Lock()
		tried := e.autoTried[d.ID]
		e.mu.Unlock()
		if !tried.IsZero() && now.Sub(tried) < period {
			continue
		}
		if e.Refresh(d.ID) {
			e.mu.Lock()
			e.autoTried[d.ID] = now
			e.mu.Unlock()
			_ = e.st.AddAudit("system", "scheduled refresh", d.Name, "")
			started++
		}
	}
	if started > 0 {
		e.log.Info("scheduled refresh started", "devices", started)
	}
	return started
}

// Wait blocks until all loops have stopped after the context is cancelled.
func (e *Engine) Wait() { e.wg.Wait() }

// Resolve works out the interval and timeouts: device > profile > global.
func (e *Engine) Resolve(d store.Device) Effective {
	g := e.st.Settings()
	eff := Effective{
		Interval: time.Duration(g.IntervalSec) * time.Second,
		Timeout:  time.Duration(g.TimeoutSec) * time.Second,
		Retries:  g.Retries, AutoSelectUp: g.AutoSelectUp, Source: "global",
	}
	if d.ProfileID != 0 {
		if p, err := e.st.Profile(d.ProfileID); err == nil {
			eff.Interval = time.Duration(p.IntervalSec) * time.Second
			eff.Timeout = time.Duration(p.TimeoutSec) * time.Second
			eff.Retries = p.Retries
			eff.AutoSelectUp = p.AutoSelectUp
			eff.Source = "profile \"" + p.Name + "\""
		}
	}
	if d.IntervalSec > 0 {
		eff.Interval = time.Duration(max(d.IntervalSec, store.MinIntervalSec)) * time.Second
		eff.Source = "device"
	}
	// A poll must finish well within its interval.
	if limit := eff.Interval * 8 / 10; eff.Timeout*time.Duration(eff.Retries+1) > limit {
		eff.Retries = 0
		eff.Timeout = min(eff.Timeout, limit)
	}
	return eff
}

// Target builds the SNMP target with decrypted credentials.
func (e *Engine) Target(d store.Device, eff Effective) (snmpx.Target, error) {
	c, err := e.st.Credential(d.CredentialID)
	if err != nil {
		return snmpx.Target{}, fmt.Errorf("credentials: %w", err)
	}
	if c, err = e.DecryptCredential(c); err != nil {
		return snmpx.Target{}, err
	}
	return snmpx.Target{Address: d.Address, Port: d.Port, Cred: c, Timeout: eff.Timeout, Retries: eff.Retries}, nil
}

// DecryptCredential returns c with plain-text secrets.
func (e *Engine) DecryptCredential(c store.Credential) (store.Credential, error) {
	var err error
	for _, f := range []*string{&c.Community, &c.V3AuthPass, &c.V3PrivPass} {
		if *f, err = e.box.Decrypt(*f); err != nil {
			return c, err
		}
	}
	return c, nil
}

// ReloadAll restarts every poll loop (after global or profile changes).
func (e *Engine) ReloadAll() error {
	devs, err := e.st.Devices()
	if err != nil {
		return err
	}
	keep := map[int64]bool{}
	for _, d := range devs {
		keep[d.ID] = true
		e.reload(d, false)
	}
	e.mu.Lock()
	for id, cancel := range e.loops {
		if !keep[id] {
			cancel()
			delete(e.loops, id)
			delete(e.snaps, id)
		}
	}
	e.mu.Unlock()
	return nil
}

// Reload restarts the poll loop of one device (after edit, selection change
// or discovery) and polls it right away so new objects show up quickly.
func (e *Engine) Reload(id int64) {
	d, err := e.st.Device(id)
	if errors.Is(err, store.ErrNotFound) {
		e.mu.Lock()
		if cancel, ok := e.loops[id]; ok {
			cancel()
			delete(e.loops, id)
		}
		delete(e.snaps, id)
		e.mu.Unlock()
		return
	}
	if err != nil {
		e.log.Error("reload device", "id", id, "err", err)
		return
	}
	e.reload(d, true)
}

// reload restarts a device's loop. Without immediate, the loop keeps the
// cadence of the previous polls, so a global change does not hit all
// devices at once.
func (e *Engine) reload(d store.Device, immediate bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancel, ok := e.loops[d.ID]; ok {
		cancel()
		delete(e.loops, d.ID)
	}
	if e.ctx == nil || !d.Enabled || d.LastDiscovery.IsZero() {
		if !d.Enabled {
			delete(e.snaps, d.ID)
		}
		return
	}
	ctx, cancel := context.WithCancel(e.ctx)
	e.loops[d.ID] = cancel
	e.wg.Add(1)
	go e.loop(ctx, d.ID, immediate)
}

func (e *Engine) deviceLock(id int64) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.locks[id]
	if !ok {
		l = &sync.Mutex{}
		e.locks[id] = l
	}
	return l
}

func (e *Engine) loop(ctx context.Context, id int64, immediate bool) {
	defer e.wg.Done()
	d, err := e.st.Device(id)
	if err != nil {
		return
	}
	eff := e.Resolve(d)
	objs, err := e.st.Objects(id)
	if err != nil {
		e.log.Error("load objects", "device", d.Name, "err", err)
		return
	}
	target, terr := e.Target(d, eff)

	// Spread first polls over the interval so devices are not hit all at once.
	wait := time.Duration(rand.Int64N(int64(eff.Interval)))
	if snap := e.Snapshot(id); snap != nil && !snap.At.IsZero() {
		wait = max(time.Until(snap.At.Add(eff.Interval)), 0)
	}
	if immediate || e.noJitter {
		wait = 0
	}
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		var samples []snmpx.Sample
		start := time.Now()
		err := terr
		if err == nil {
			samples, err = e.pollOnce(ctx, id, target, objs)
		}
		if ctx.Err() != nil {
			return
		}
		snap := &Snapshot{At: start, Duration: time.Since(start), Interval: eff.Interval}
		prev := e.Snapshot(id)
		if err != nil {
			fails++
			snap.Err = err.Error()
			snap.Fails = fails
			if prev != nil {
				snap.LastOK = prev.LastOK
			}
			e.log.Warn("poll failed", "device", d.Name, "fails", fails, "err", err)
		} else {
			fails = 0
			snap.OK = true
			snap.LastOK = start
			snap.Samples = samples
		}
		e.mu.Lock()
		e.snaps[id] = snap
		e.mu.Unlock()
		wait = backoff(eff.Interval, fails)
	}
}

func backoff(interval time.Duration, fails int) time.Duration {
	if fails <= 1 {
		return interval
	}
	d := interval << min(fails-1, 4)
	return max(min(d, maxBackoff), interval)
}

func (e *Engine) pollOnce(ctx context.Context, id int64, t snmpx.Target, objs []store.Object) ([]snmpx.Sample, error) {
	select {
	case e.pollSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-e.pollSem }()
	l := e.deviceLock(id)
	l.Lock()
	defer l.Unlock()
	s, err := e.dial(t)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return snmpx.Poll(s, objs)
}

// Snapshot returns the latest poll result of a device, or nil.
func (e *Engine) Snapshot(id int64) *Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snaps[id]
}

// MaxConcurrentDiscovery is how many full discoveries may run at once.
func (e *Engine) MaxConcurrentDiscovery() int { return cap(e.discSem) }

// Discovering reports whether a discovery of the device is in progress.
func (e *Engine) Discovering(id int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id]
}

// AnyDiscovering reports whether any discovery is in progress.
func (e *Engine) AnyDiscovering() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.discCount > 0
}

// Refresh starts a full discovery of a device in the background.
// It returns false when one is already running.
func (e *Engine) Refresh(id int64) bool {
	e.mu.Lock()
	if e.running[id] {
		e.mu.Unlock()
		return false
	}
	e.running[id] = true
	e.discCount++
	e.mu.Unlock()
	go func() {
		defer func() {
			e.mu.Lock()
			delete(e.running, id)
			e.discCount--
			e.mu.Unlock()
		}()
		if err := e.DiscoverNow(id); err != nil {
			e.log.Warn("discovery failed", "id", id, "err", err)
		}
	}()
	return true
}

// DiscoverNow runs a full discovery synchronously, merges it with the
// current objects and restarts polling.
func (e *Engine) DiscoverNow(id int64) error {
	e.discSem <- struct{}{}
	defer func() { <-e.discSem }()
	d, err := e.st.Device(id)
	if err != nil {
		return err
	}
	err = e.discover(d)
	if err != nil {
		_ = e.st.SetDiscoveryError(id, err.Error())
		return err
	}
	e.Reload(id)
	return nil
}

func (e *Engine) discover(d store.Device) error {
	eff := e.Resolve(d)
	// Walks are larger than polls; give them more time per request.
	eff.Timeout = max(eff.Timeout, 5*time.Second)
	eff.Retries = max(eff.Retries, 1)
	t, err := e.Target(d, eff)
	if err != nil {
		return err
	}
	l := e.deviceLock(d.ID)
	l.Lock()
	defer l.Unlock()
	s, err := e.dial(t)
	if err != nil {
		return err
	}
	defer s.Close()
	info, found, err := snmpx.Discover(s)
	if err != nil {
		return err
	}
	old, err := e.st.Objects(d.ID)
	if err != nil {
		return err
	}
	d.SysName, d.SysDescr, d.SysObjectID = info.SysName, info.SysDescr, info.SysObjectID
	return e.st.ApplyDiscovery(d, snmpx.Merge(old, found, eff.AutoSelectUp))
}

// Test checks that a device answers with the given credential.
func (e *Engine) Test(t snmpx.Target) (snmpx.SystemInfo, error) {
	s, err := e.dial(t)
	if err != nil {
		return snmpx.SystemInfo{}, err
	}
	defer s.Close()
	pdus, err := s.Get([]string{"1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.5.0"})
	if err != nil {
		return snmpx.SystemInfo{}, err
	}
	var info snmpx.SystemInfo
	for _, p := range pdus {
		v := fmt.Sprint(p.Value)
		if b, ok := p.Value.([]byte); ok {
			v = string(b)
		}
		if p.Name == ".1.3.6.1.2.1.1.1.0" || p.Name == "1.3.6.1.2.1.1.1.0" {
			info.SysDescr = v
		} else {
			info.SysName = v
		}
	}
	if len(pdus) == 0 {
		return info, errors.New("no answer to sysDescr")
	}
	return info, nil
}

// Lookup reads one OID or walks a subtree on a device, to check an OID by hand.
func (e *Engine) Lookup(d store.Device, oid string, walk bool) ([]snmpx.LookupRow, bool, error) {
	eff := e.Resolve(d)
	eff.Timeout = max(eff.Timeout, 5*time.Second)
	t, err := e.Target(d, eff)
	if err != nil {
		return nil, false, err
	}
	s, err := e.dial(t)
	if err != nil {
		return nil, false, err
	}
	defer s.Close()
	return snmpx.Lookup(s, oid, walk)
}
