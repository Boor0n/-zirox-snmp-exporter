// Package engine schedules discovery and periodic polls and keeps the latest
// values in memory for Prometheus.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
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
	// Problem is the class of Err (snmpx.Problem*), Hint what to check.
	Problem  string
	Hint     string
	Fails    int
	Interval time.Duration

	// Uptime is the device's sysUpTime at the poll; LastReboot the time the
	// device last restarted as seen by Zirox (zero when not seen).
	Uptime     time.Duration
	HasUptime  bool
	LastReboot time.Time
	// Partial is set when some GET requests were lost; Requests/Failed count them.
	Partial  bool
	Requests int
	Failed   int
	// Shifted lists ports whose ifIndex moved; their values are withheld
	// until the automatic Refresh maps them again.
	Shifted []string
	// Prev and PrevAt are the samples of the previous successful poll, to
	// turn counters into rates for the UI.
	Prev   []snmpx.Sample
	PrevAt time.Time
	// DownSince is when the device stopped answering (zero while it answers).
	DownSince time.Time
}

// autoFixPause is the shortest time between two automatic refreshes of one
// device triggered by a reboot or an ifIndex shift.
const autoFixPause = 15 * time.Minute

// uptimeWrap is where 32-bit sysUpTime (hundredths of a second) wraps, after
// about 497 days. A drop close to it is a wrap, not a reboot.
const uptimeWrap = time.Duration(1<<32) * 10 * time.Millisecond

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
	// Probe checks whether anything answers SNMP at an address, whatever the
	// credentials (default snmpx.ProbeV3).
	Probe snmpx.Prober
}

// Engine runs one poll loop per enabled device.
type Engine struct {
	st    *store.Store
	box   *secret.Box
	dial  snmpx.Dialer
	probe snmpx.Prober
	log   *slog.Logger

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
	autoFixed map[int64]time.Time // last reboot/shift triggered refresh
	history   map[int64]*availability
	wg        sync.WaitGroup
	discCount int
}

// New creates an engine. Call Start to begin polling.
func New(st *store.Store, box *secret.Box, o Options) *Engine {
	// Sized for about a thousand devices; each limit only caps work running
	// at the same moment, never the number of devices.
	if o.MaxConcurrentPolls <= 0 {
		o.MaxConcurrentPolls = 128
	}
	if o.MaxConcurrentDiscovery <= 0 {
		o.MaxConcurrentDiscovery = 32
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
	if o.Probe == nil {
		o.Probe = snmpx.ProbeV3
	}
	return &Engine{
		st: st, box: box, dial: o.Dial, probe: o.Probe, log: o.Log, noJitter: o.NoJitter,
		arCheck: o.AutoRefreshCheck, now: o.Now,
		autoTried: map[int64]time.Time{},
		autoFixed: map[int64]time.Time{},
		history:   map[int64]*availability{},
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
		var res snmpx.PollResult
		start := time.Now()
		err := terr
		if err == nil {
			res, err = e.pollOnce(ctx, id, target, objs)
		}
		if ctx.Err() != nil {
			return
		}
		snap := &Snapshot{At: start, Duration: time.Since(start), Interval: eff.Interval}
		prev := e.Snapshot(id)
		if prev != nil {
			snap.LastReboot = prev.LastReboot
			if prev.OK {
				snap.Prev, snap.PrevAt = prev.Samples, prev.At
			} else {
				snap.Prev, snap.PrevAt = prev.Prev, prev.PrevAt
			}
		}
		if err != nil {
			fails++
			ex := snmpx.Explain(err, target.Cred.Version, snmpx.SourceAddress(d.Address))
			snap.Err, snap.Problem, snap.Hint = ex.Summary, ex.Problem, ex.Hint
			if ex.Raw != "" {
				snap.Err += " (" + ex.Raw + ")"
			}
			snap.Fails = fails
			snap.DownSince = start
			if prev != nil {
				snap.LastOK = prev.LastOK
				if !prev.DownSince.IsZero() {
					snap.DownSince = prev.DownSince
				}
			}
			if fails == 1 && (prev == nil || prev.OK) {
				_ = e.st.AddAudit("system", "device not answering", d.Name, snap.Err)
			}
			e.log.Warn("poll failed", "device", d.Name, "fails", fails, "err", err)
		} else {
			if prev != nil && !prev.OK {
				_ = e.st.AddAudit("system", "device answers again", d.Name, "down for "+start.Sub(prev.DownSince).Round(time.Second).String())
			}
			fails = 0
			snap.OK = true
			snap.LastOK = start
			snap.Samples = res.Samples
			snap.Requests, snap.Failed = res.Requests, res.Failed
			snap.Uptime, snap.HasUptime = res.Uptime, res.HasUptime
			snap.Shifted = res.Shifted
			if res.Partial() {
				snap.Partial = true
				ex := snmpx.Explain(res.Err, target.Cred.Version, "")
				snap.Err = fmt.Sprintf("%d of %d requests got no answer: %s", res.Failed, res.Requests, ex.Summary)
				snap.Problem, snap.Hint = snmpx.ProblemPartial, "Packet loss to the device or an overloaded agent; values of the affected objects are repeated from the previous poll once"
				// Repeat the lost objects' previous values once, so one lost
				// UDP packet does not leave a hole in every graph.
				if prev != nil && prev.OK && !prev.Partial {
					for _, sm := range prev.Samples {
						if res.LostObjects[sm.Obj] {
							snap.Samples = append(snap.Samples, sm)
						}
					}
				}
				e.log.Warn("partial poll", "device", d.Name, "failed", res.Failed, "requests", res.Requests, "err", res.Err)
			}
			e.checkConsistency(d, prev, snap)
		}
		e.record(id, start, snap.OK)
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

// checkConsistency notices reboots and ifIndex shifts and starts a Refresh
// for them, at most once per autoFixPause per device.
func (e *Engine) checkConsistency(d store.Device, prev, snap *Snapshot) {
	rebooted := false
	if snap.HasUptime && prev != nil && prev.HasUptime && snap.Uptime < prev.Uptime && prev.Uptime < uptimeWrap-time.Hour {
		rebooted = true
		snap.LastReboot = snap.At.Add(-snap.Uptime)
		e.log.Warn("device restarted", "device", d.Name, "uptime", snap.Uptime.Round(time.Second))
		_ = e.st.AddAudit("system", "device restarted", d.Name, "uptime "+snap.Uptime.Round(time.Second).String())
	}
	if !rebooted && len(snap.Shifted) == 0 {
		return
	}
	now := e.now()
	e.mu.Lock()
	last := e.autoFixed[d.ID]
	due := last.IsZero() || now.Sub(last) >= autoFixPause
	if due {
		e.autoFixed[d.ID] = now
	}
	e.mu.Unlock()
	if !due {
		return
	}
	reason := "device restarted"
	if len(snap.Shifted) > 0 {
		reason = "port numbering changed: " + shortNames(snap.Shifted)
		e.log.Warn("ifIndex shift", "device", d.Name, "ports", snap.Shifted)
	}
	if e.Refresh(d.ID) {
		_ = e.st.AddAudit("system", "automatic refresh", d.Name, reason)
	}
}

func shortNames(names []string) string {
	const max = 5
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(" and %d more", len(names)-max)
}

func (e *Engine) pollOnce(ctx context.Context, id int64, t snmpx.Target, objs []store.Object) (snmpx.PollResult, error) {
	select {
	case e.pollSem <- struct{}{}:
	case <-ctx.Done():
		return snmpx.PollResult{}, ctx.Err()
	}
	defer func() { <-e.pollSem }()
	l := e.deviceLock(id)
	l.Lock()
	defer l.Unlock()
	s, err := e.dial(t)
	if err != nil {
		return snmpx.PollResult{}, err
	}
	defer s.Close()
	return snmpx.PollDevice(s, objs)
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
		msg := err.Error()
		if c, cerr := e.st.Credential(d.CredentialID); cerr == nil {
			msg = snmpx.Explain(err, c.Version, snmpx.SourceAddress(d.Address)).Error()
		}
		_ = e.st.SetDiscoveryError(id, msg)
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
	d.Model, d.SWVersion, d.Serial = info.Model, info.Version, info.Serial
	return e.st.ApplyDiscovery(d, snmpx.Merge(old, found, eff.AutoSelectUp))
}

// Test checks that a device answers with the given credential.
func (e *Engine) Test(t snmpx.Target) (snmpx.SystemInfo, error) {
	s, err := e.dial(t)
	if err != nil {
		return snmpx.SystemInfo{}, err
	}
	defer s.Close()
	pdus, err := s.Get([]string{"1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.2.0", "1.3.6.1.2.1.1.5.0"})
	if err != nil {
		return snmpx.SystemInfo{}, err
	}
	var info snmpx.SystemInfo
	for _, p := range pdus {
		v := fmt.Sprint(p.Value)
		if b, ok := p.Value.([]byte); ok {
			v = string(b)
		}
		switch strings.TrimPrefix(p.Name, ".") {
		case "1.3.6.1.2.1.1.1.0":
			info.SysDescr = v
		case "1.3.6.1.2.1.1.2.0":
			info.SysObjectID = strings.TrimPrefix(v, ".")
		default:
			info.SysName = v
		}
	}
	if len(pdus) == 0 {
		return info, snmpx.ErrNoData
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

// DiagnoseTarget runs the access ladder against a target. Short timeouts keep
// a check of an unreachable device to a few seconds.
func (e *Engine) DiagnoseTarget(t snmpx.Target) snmpx.Diagnosis {
	t.Timeout = min(max(t.Timeout, 2*time.Second), 5*time.Second)
	t.Retries = min(t.Retries, 1)
	return snmpx.Diagnose(t, e.dial, e.probe)
}

// Diagnose runs the access ladder for a saved device.
func (e *Engine) Diagnose(d store.Device) snmpx.Diagnosis {
	t, err := e.Target(d, e.Resolve(d))
	if err != nil {
		return snmpx.Diagnosis{Address: d.Address, Failed: "Credentials", Problem: snmpx.ProblemConfig,
			Steps: []snmpx.Step{{Name: "Credentials", Status: snmpx.StepFail, Detail: err.Error()}}}
	}
	return e.DiagnoseTarget(t)
}

// Identify asks a target for its system group only: used by the subnet scan.
func (e *Engine) Identify(t snmpx.Target) (snmpx.SystemInfo, error) {
	info, err := e.Test(t)
	if err == nil {
		info.Identity = snmpx.IdentifyFromSystem(info.SysDescr, info.SysObjectID)
	}
	return info, err
}

// ---- availability history ----

// historySlot is the width of one availability bucket; 48 of them cover a day.
const (
	historySlot  = 30 * time.Minute
	historySlots = 48
)

type availability struct {
	slot [historySlots]struct {
		start    int64 // unix time of the bucket start, to drop stale ones
		ok, fail int
	}
}

func (e *Engine) record(id int64, at time.Time, ok bool) {
	b := at.Truncate(historySlot).Unix()
	i := int(b/int64(historySlot.Seconds())) % historySlots
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.history[id]
	if h == nil {
		h = &availability{}
		e.history[id] = h
	}
	sl := &h.slot[i]
	if sl.start != b {
		sl.start, sl.ok, sl.fail = b, 0, 0
	}
	if ok {
		sl.ok++
	} else {
		sl.fail++
	}
}

// Bucket is the availability of a device in one half hour.
type Bucket struct {
	Start    time.Time
	OK, Fail int
}

// State is "ok", "fail", "mixed" or "none" (not polled).
func (b Bucket) State() string {
	switch {
	case b.OK == 0 && b.Fail == 0:
		return "none"
	case b.Fail == 0:
		return "ok"
	case b.OK == 0:
		return "fail"
	}
	return "mixed"
}

// History returns the last 24 hours of a device in half-hour buckets,
// oldest first. It is kept in memory and starts empty after a restart.
func (e *Engine) History(id int64) []Bucket {
	now := e.now().Truncate(historySlot)
	out := make([]Bucket, historySlots)
	e.mu.Lock()
	h := e.history[id]
	e.mu.Unlock()
	for k := 0; k < historySlots; k++ {
		start := now.Add(-time.Duration(historySlots-1-k) * historySlot)
		out[k].Start = start
		if h == nil {
			continue
		}
		e.mu.Lock()
		sl := h.slot[int(start.Unix()/int64(historySlot.Seconds()))%historySlots]
		e.mu.Unlock()
		if sl.start == start.Unix() {
			out[k].OK, out[k].Fail = sl.ok, sl.fail
		}
	}
	return out
}

// ---- live port rates ----

// PortRate is what one port did between the last two polls.
type PortRate struct {
	InBps, OutBps float64 // bits per second
	ErrPerMin     float64 // in+out errors per minute
	Util          float64 // max(in, out) in percent of the speed, -1 if unknown
}

// PortRates turns the counters of the last two polls into rates, by object ID.
func PortRates(s *Snapshot) map[int64]PortRate {
	out := map[int64]PortRate{}
	if s == nil || !s.OK || len(s.Prev) == 0 || !s.At.After(s.PrevAt) {
		return out
	}
	dt := s.At.Sub(s.PrevAt).Seconds()
	type key struct {
		obj  int64
		name string
	}
	prev := make(map[key]float64, len(s.Prev))
	for _, p := range s.Prev {
		prev[key{p.Obj, p.Name}] = p.Value
	}
	speed := map[int64]float64{}
	delta := func(sm snmpx.Sample) (float64, bool) {
		pv, ok := prev[key{sm.Obj, sm.Name}]
		if !ok || sm.Value < pv {
			return 0, false // new or wrapped/reset counter
		}
		return (sm.Value - pv) / dt, true
	}
	for _, sm := range s.Samples {
		if sm.Obj == 0 {
			continue
		}
		r, seen := out[sm.Obj]
		if !seen {
			r.Util = -1
		}
		switch sm.Name {
		case "ifHCInOctets", "ifInOctets":
			if v, ok := delta(sm); ok {
				r.InBps = v * 8
			}
		case "ifHCOutOctets", "ifOutOctets":
			if v, ok := delta(sm); ok {
				r.OutBps = v * 8
			}
		case "ifInErrors", "ifOutErrors":
			if v, ok := delta(sm); ok {
				r.ErrPerMin += v * 60
			}
		case "ifHighSpeed":
			speed[sm.Obj] = sm.Value * 1e6
		case "ifSpeed":
			if _, ok := speed[sm.Obj]; !ok {
				speed[sm.Obj] = sm.Value
			}
		default:
			if !seen {
				continue
			}
		}
		out[sm.Obj] = r
	}
	for id, r := range out {
		if sp := speed[id]; sp > 0 {
			r.Util = 100 * max(r.InBps, r.OutBps) / sp
			out[id] = r
		}
	}
	return out
}
