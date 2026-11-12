// Package exporter serves cached SNMP values to Prometheus. Scrapes never
// touch the devices: they only read the engine's latest snapshots.
package exporter

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Boor0n/zirox-snmp-exporter/internal/engine"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// Exporter holds the HTTP handlers for Prometheus.
type Exporter struct {
	st *store.Store
	en *engine.Engine
	// SDAddress is the host:port Prometheus should scrape; empty = request Host.
	SDAddress string
	// Token, when set, must be sent as "Authorization: Bearer <token>".
	Token string
}

// New creates an exporter.
func New(st *store.Store, en *engine.Engine) *Exporter {
	return &Exporter{st: st, en: en}
}

// Register mounts /probe, /sd, /metrics and /metrics/all on mux.
func (x *Exporter) Register(mux *http.ServeMux) {
	mux.Handle("GET /probe", x.auth(http.HandlerFunc(x.probe)))
	mux.Handle("GET /sd", x.auth(http.HandlerFunc(x.sd)))
	mux.Handle("GET /metrics/all", x.auth(http.HandlerFunc(x.all)))
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}), &selfCollector{x: x})
	mux.Handle("GET /metrics", x.auth(promhttp.HandlerFor(reg, promhttp.HandlerOpts{})))
}

func (x *Exporter) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if x.Token != "" && r.Header.Get("Authorization") != "Bearer "+x.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (x *Exporter) findDevice(target string) (store.Device, error) {
	if id, err := strconv.ParseInt(target, 10, 64); err == nil {
		return x.st.Device(id)
	}
	devs, err := x.st.Devices()
	if err != nil {
		return store.Device{}, err
	}
	for _, d := range devs {
		if d.Name == target || d.Address == target {
			return d, nil
		}
	}
	return store.Device{}, store.ErrNotFound
}

// probe returns the metrics of one device: /probe?target=<id|name|address>.
func (x *Exporter) probe(w http.ResponseWriter, r *http.Request) {
	d, err := x.findDevice(r.URL.Query().Get("target"))
	if err != nil {
		http.Error(w, "unknown target", http.StatusNotFound)
		return
	}
	reg := prometheus.NewRegistry()
	if r.URL.Query().Get("poll") == "fast" {
		reg.MustRegister(&fastCollector{snap: x.en.FastSnapshot(d.ID)})
	} else {
		reg.MustRegister(&deviceCollector{dev: d, snap: x.en.Snapshot(d.ID)})
	}
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}).ServeHTTP(w, r)
}

// all returns every device in one response, with device and group labels.
func (x *Exporter) all(w http.ResponseWriter, r *http.Request) {
	devs, err := x.st.Devices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(&allCollector{x: x, devs: devs})
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}).ServeHTTP(w, r)
}

type sdTarget struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// sd is a Prometheus HTTP service discovery endpoint: one target per enabled
// device, scraped through /probe at the device's own poll interval.
func (x *Exporter) sd(w http.ResponseWriter, r *http.Request) {
	devs, err := x.st.Devices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	addr := x.SDAddress
	scheme, defPort := "http", "80"
	if r.TLS != nil {
		scheme, defPort = "https", "443"
	}
	if addr == "" {
		addr = r.Host
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, defPort)
		}
	}
	out := []sdTarget{}
	for _, d := range devs {
		if !d.Enabled {
			continue
		}
		eff := x.en.Resolve(d)
		timeout := min(eff.Interval, 10*time.Second)
		out = append(out, sdTarget{
			Targets: []string{addr},
			Labels: map[string]string{
				"__metrics_path__":    "/probe",
				"__scheme__":          scheme,
				"__param_target":      strconv.FormatInt(d.ID, 10),
				"__scrape_interval__": fmt.Sprintf("%ds", int(eff.Interval.Seconds())),
				"__scrape_timeout__":  fmt.Sprintf("%ds", int(timeout.Seconds())),
				"instance":            d.Name,
				"device":              d.Name,
				"address":             d.Address,
				"group":               d.Group,
				"sysName":             d.SysName,
			},
		})
		// Uplink ports polled at the fast interval get their own target, so
		// Prometheus scrapes them as often as they are polled.
		if eff.Fast > 0 && x.en.FastSnapshot(d.ID) != nil {
			out = append(out, sdTarget{
				Targets: []string{addr},
				Labels: map[string]string{
					"__metrics_path__":    "/probe",
					"__scheme__":          scheme,
					"__param_target":      strconv.FormatInt(d.ID, 10),
					"__param_poll":        "fast",
					"__scrape_interval__": fmt.Sprintf("%ds", int(eff.Fast.Seconds())),
					"__scrape_timeout__":  fmt.Sprintf("%ds", int(min(eff.Fast, 10*time.Second).Seconds())),
					"instance":            d.Name,
					"device":              d.Name,
					"address":             d.Address,
					"group":               d.Group,
					"sysName":             d.SysName,
					"poll":                "fast",
				},
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---- collectors ----

var (
	descUp       = prometheus.NewDesc("zirox_poll_success", "1 if the last SNMP poll of the device succeeded.", nil, nil)
	descDuration = prometheus.NewDesc("zirox_poll_duration_seconds", "Duration of the last SNMP poll.", nil, nil)
	descLastOK   = prometheus.NewDesc("zirox_poll_last_success_timestamp_seconds", "Unix time of the last successful poll.", nil, nil)
	descSamples  = prometheus.NewDesc("zirox_poll_samples", "Number of values returned by the last poll.", nil, nil)
	descInterval = prometheus.NewDesc("zirox_poll_interval_seconds", "Effective poll interval of the device.", nil, nil)
	descInfo     = prometheus.NewDesc("zirox_device_info", "Device information from the last discovery.", infoLabels, nil)
	infoLabels   = []string{"sysName", "sysDescr", "sysObjectID", "model", "version", "serial"}
)

type deviceCollector struct {
	dev  store.Device
	snap *engine.Snapshot
}

func (c *deviceCollector) Describe(chan<- *prometheus.Desc) {}

func (c *deviceCollector) Collect(ch chan<- prometheus.Metric) {
	emitDevice(ch, c.dev, c.snap, nil, nil)
}

func emitDevice(ch chan<- prometheus.Metric, d store.Device, snap *engine.Snapshot, extraKeys, extraVals []string) {
	constMetric := func(desc *prometheus.Desc, t prometheus.ValueType, v float64, lv ...string) {
		if m, err := prometheus.NewConstMetric(desc, t, v, append(append([]string{}, extraVals...), lv...)...); err == nil {
			ch <- m
		}
	}
	withExtra := func(desc *prometheus.Desc, name, help string, keys []string) *prometheus.Desc {
		if len(extraKeys) == 0 && desc != nil {
			return desc
		}
		return prometheus.NewDesc(name, help, append(append([]string{}, extraKeys...), keys...), nil)
	}
	info := withExtra(descInfo, "zirox_device_info", "Device information from the last discovery.", infoLabels)
	constMetric(info, prometheus.GaugeValue, 1, d.SysName, d.SysDescr, d.SysObjectID, d.Model, d.SWVersion, d.Serial)
	if !d.LastDiscovery.IsZero() {
		constMetric(withExtra(nil, "zirox_device_last_discovery_timestamp_seconds", "Unix time of the last full discovery (Add or Refresh).", nil), prometheus.GaugeValue, float64(d.LastDiscovery.Unix()))
	}
	up := 0.0
	if snap != nil && snap.OK {
		up = 1
	}
	constMetric(withExtra(descUp, "zirox_poll_success", "1 if the last SNMP poll of the device succeeded.", nil), prometheus.GaugeValue, up)
	if snap == nil {
		return
	}
	constMetric(withExtra(descDuration, "zirox_poll_duration_seconds", "Duration of the last SNMP poll.", nil), prometheus.GaugeValue, snap.Duration.Seconds())
	constMetric(withExtra(descInterval, "zirox_poll_interval_seconds", "Effective poll interval of the device.", nil), prometheus.GaugeValue, snap.Interval.Seconds())
	constMetric(withExtra(descSamples, "zirox_poll_samples", "Number of values returned by the last poll.", nil), prometheus.GaugeValue, float64(len(snap.Samples)))
	if snap.OK {
		partial := 0.0
		if snap.Partial {
			partial = 1
		}
		constMetric(withExtra(nil, "zirox_poll_partial", "1 if some SNMP requests of the last poll got no answer (values of those objects are missing or repeated).", nil), prometheus.GaugeValue, partial)
		constMetric(withExtra(nil, "zirox_poll_requests_failed", "SNMP GET requests of the last poll that got no answer.", nil), prometheus.GaugeValue, float64(snap.Failed))
		constMetric(withExtra(nil, "zirox_ports_index_shifted", "Ports whose ifIndex points at another interface; their values are withheld until the automatic Refresh.", nil), prometheus.GaugeValue, float64(len(snap.Shifted)))
		if snap.HasUptime {
			constMetric(withExtra(nil, "zirox_device_uptime_seconds", "Device uptime from sysUpTime (SNMP agent uptime).", nil), prometheus.GaugeValue, snap.Uptime.Seconds())
		}
	}
	if !snap.LastReboot.IsZero() {
		constMetric(withExtra(nil, "zirox_device_last_restart_timestamp_seconds", "Unix time of the last device restart seen by Zirox.", nil), prometheus.GaugeValue, float64(snap.LastReboot.Unix()))
	}
	if !snap.LastOK.IsZero() {
		constMetric(withExtra(descLastOK, "zirox_poll_last_success_timestamp_seconds", "Unix time of the last successful poll.", nil), prometheus.GaugeValue, float64(snap.LastOK.Unix()))
	}
	descs := map[string]*prometheus.Desc{}
	for _, s := range snap.Samples {
		emitSample(ch, descs, s, extraKeys, extraVals)
	}
}

func emitSample(ch chan<- prometheus.Metric, descs map[string]*prometheus.Desc, s snmpx.Sample, extraKeys, extraVals []string) {
	desc, ok := descs[s.Name]
	if !ok {
		desc = prometheus.NewDesc(s.Name, s.Help, append(append([]string{}, extraKeys...), s.Keys...), nil)
		descs[s.Name] = desc
	}
	t := prometheus.GaugeValue
	if s.Counter {
		t = prometheus.CounterValue
	}
	if m, err := prometheus.NewConstMetric(desc, t, s.Value, append(append([]string{}, extraVals...), s.Values...)...); err == nil {
		ch <- m
	}
}

type allCollector struct {
	x    *Exporter
	devs []store.Device
}

func (c *allCollector) Describe(chan<- *prometheus.Desc) {}

func (c *allCollector) Collect(ch chan<- prometheus.Metric) {
	for _, d := range c.devs {
		if !d.Enabled {
			continue
		}
		emitDevice(ch, d, c.x.en.Snapshot(d.ID), []string{"device", "group"}, []string{d.Name, d.Group})
		if fs := c.x.en.FastSnapshot(d.ID); fs != nil {
			emitFast(ch, fs, []string{"device", "group"}, []string{d.Name, d.Group})
		}
	}
}

// selfCollector reports the exporter's own health on /metrics.
type selfCollector struct{ x *Exporter }

var (
	descDevices = prometheus.NewDesc("zirox_devices", "Number of devices by state.", []string{"state"}, nil)
)

func (c *selfCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descDevices }

func (c *selfCollector) Collect(ch chan<- prometheus.Metric) {
	devs, err := c.x.st.Devices()
	if err != nil {
		return
	}
	counts := map[string]float64{"enabled": 0, "disabled": 0, "ok": 0, "failing": 0, "never_polled": 0}
	for _, d := range devs {
		if !d.Enabled {
			counts["disabled"]++
			continue
		}
		counts["enabled"]++
		switch s := c.x.en.Snapshot(d.ID); {
		case s == nil:
			counts["never_polled"]++
		case s.OK:
			counts["ok"]++
		default:
			counts["failing"]++
		}
	}
	for k, v := range counts {
		ch <- prometheus.MustNewConstMetric(descDevices, prometheus.GaugeValue, v, k)
	}
}

// fastCollector exports the uplink ports polled at the fast interval.
type fastCollector struct{ snap *engine.Snapshot }

func (c *fastCollector) Describe(chan<- *prometheus.Desc) {}

func (c *fastCollector) Collect(ch chan<- prometheus.Metric) { emitFast(ch, c.snap, nil, nil) }

func emitFast(ch chan<- prometheus.Metric, snap *engine.Snapshot, extraKeys, extraVals []string) {
	gauge := func(name, help string, v float64) {
		d := prometheus.NewDesc(name, help, extraKeys, nil)
		if m, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, v, extraVals...); err == nil {
			ch <- m
		}
	}
	up := 0.0
	if snap != nil && snap.OK {
		up = 1
	}
	gauge("zirox_fast_poll_success", "1 if the last fast poll of the device's uplink ports succeeded.", up)
	if snap == nil {
		return
	}
	gauge("zirox_fast_poll_duration_seconds", "Duration of the last fast poll of uplink ports.", snap.Duration.Seconds())
	gauge("zirox_fast_poll_interval_seconds", "Poll interval of uplink ports.", snap.Interval.Seconds())
	descs := map[string]*prometheus.Desc{}
	for _, s := range snap.Samples {
		emitSample(ch, descs, s, extraKeys, extraVals)
	}
}
