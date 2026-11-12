package snmpx

import (
	"strings"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// Sample is one metric value ready for Prometheus.
type Sample struct {
	Name    string
	Help    string
	Counter bool
	Keys    []string
	Values  []string
	Value   float64
	// Obj is the ID of the object the value belongs to (0 = device level).
	Obj int64
}

// IfInfoMetric carries the live port description, so a changed ifAlias
// does not split the traffic series that keep the discovered one.
const IfInfoMetric = "zirox_if_info"

// Checks added to every poll to make sure the values still belong to the
// objects they are exported as.
const (
	oidIfNameCol  = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfAliasCol = "1.3.6.1.2.1.31.1.1.1.18"
)

type wanted struct {
	gi     int // index into the poll's groups
	obj    store.Object
	metric Metric
	check  string // "ifName" or "ifAlias" for verification OIDs
}

// PollResult is everything one poll learned about a device.
type PollResult struct {
	Samples []Sample
	// Uptime is sysUpTime; HasUptime is false when the device did not return it.
	Uptime    time.Duration
	HasUptime bool
	// Shifted lists ports whose ifIndex now points at another interface (or
	// at none). Their values are dropped; a Refresh fixes the mapping.
	Shifted []string
	// Requests and Failed count GET requests; Failed > 0 with values present
	// means the poll was partial. LostObjects are the IDs whose values were
	// in the failed requests.
	Requests    int
	Failed      int
	LostObjects map[int64]bool
	// Err is the first request error, set when Failed > 0.
	Err error
}

// Partial reports whether some but not all requests failed.
func (r PollResult) Partial() bool { return r.Failed > 0 && r.Failed < r.Requests }

// group is the OIDs of one object; a group never spans two requests so a
// port's counters and its ifName check arrive together.
type group struct {
	obj  store.Object
	oids []string
}

// Poll GETs only the OIDs of the selected objects and turns them into samples.
func Poll(s Session, objs []store.Object) ([]Sample, error) {
	r, err := PollDevice(s, objs)
	return r.Samples, err
}

// PollDevice GETs the selected objects plus sysUpTime and the ifName of every
// selected port. Requests are sent in chunks; a lost chunk only loses its
// own objects. The error is set only when every request failed.
func PollDevice(s Session, objs []store.Object) (PollResult, error) {
	v1 := s.Version() == "1"
	byOID := map[string]wanted{}
	var groups []group
	for _, o := range objs {
		if !o.Selected || o.State == store.StateMissing {
			continue
		}
		k, ok := kindSpec(o.Kind)
		if !ok {
			continue
		}
		gi := len(groups)
		g := group{obj: o}
		add := func(oid string, w wanted) {
			if _, dup := byOID[oid]; dup {
				return
			}
			byOID[oid] = w
			g.oids = append(g.oids, oid)
		}
		if o.Kind == KindIf {
			add(oidIfNameCol+"."+o.Idx, wanted{gi: gi, obj: o, check: "ifName"})
			add(oidIfAliasCol+"."+o.Idx, wanted{gi: gi, obj: o, check: "ifAlias"})
		}
		for _, m := range k.Metrics {
			if (v1 && m.NotV1) || (!v1 && m.OnlyV1) {
				continue
			}
			add(m.OID+"."+o.Idx+m.Suffix, wanted{gi: gi, obj: o, metric: m})
		}
		if len(g.oids) > 0 {
			groups = append(groups, g)
		}
	}
	var res PollResult
	if len(groups) == 0 {
		return res, nil
	}

	// Pack whole objects into requests of at most maxOidsPerGet OIDs.
	type chunk struct {
		oids   []string
		groups []int
	}
	// sysUpTime rides in the first request, unless a custom OID polls it already.
	chunks := []chunk{{}}
	if _, ok := byOID[oidSysUpTime]; !ok {
		chunks[0].oids = []string{oidSysUpTime}
	}
	for gi, g := range groups {
		c := &chunks[len(chunks)-1]
		if len(c.oids)+len(g.oids) > maxOidsPerGet && len(c.oids) > 0 {
			chunks = append(chunks, chunk{})
			c = &chunks[len(chunks)-1]
		}
		c.oids = append(c.oids, g.oids...)
		c.groups = append(c.groups, gi)
	}

	type ifCheck struct {
		name, alias string
		gotName     bool
		gotAlias    bool
	}
	checks := map[int]*ifCheck{}
	answered := map[int]bool{}
	ifNameSupported := false
	var values []struct {
		w wanted
		v float64
	}
	for _, c := range chunks {
		res.Requests++
		pdus, err := s.Get(c.oids)
		if err != nil {
			res.Failed++
			if res.Err == nil {
				res.Err = err
			}
			if res.LostObjects == nil {
				res.LostObjects = map[int64]bool{}
			}
			for _, gi := range c.groups {
				res.LostObjects[groups[gi].obj.ID] = true
			}
			continue
		}
		for _, gi := range c.groups {
			answered[gi] = true
		}
		for _, p := range pdus {
			name := trimOID(p.Name)
			if name == oidSysUpTime {
				res.Uptime = time.Duration(asFloat(p)) * 10 * time.Millisecond
				res.HasUptime = true
			}
			w, ok := byOID[name]
			if !ok {
				continue
			}
			if w.check != "" {
				ch := checks[w.gi]
				if ch == nil {
					ch = &ifCheck{}
					checks[w.gi] = ch
				}
				if w.check == "ifName" {
					ch.name, ch.gotName = strings.TrimSpace(asString(p)), true
					ifNameSupported = true
				} else {
					ch.alias, ch.gotAlias = asString(p), true
				}
				continue
			}
			values = append(values, struct {
				w wanted
				v float64
			}{w, asFloat(p)})
		}
	}
	if res.Failed == res.Requests {
		return res, res.Err
	}

	// A port is shifted when the device knows ifName but the name at the
	// polled index is different or gone.
	shifted := map[int]bool{}
	for gi, g := range groups {
		o := g.obj
		if o.Kind != KindIf || !answered[gi] || !ifNameSupported {
			continue
		}
		ch := checks[gi]
		switch {
		case ch == nil || !ch.gotName:
			shifted[gi] = true
		case ch.name != "" && ch.name != strings.TrimSpace(o.Name):
			shifted[gi] = true
		}
		if shifted[gi] {
			res.Shifted = append(res.Shifted, o.Name)
		}
	}

	res.Samples = make([]Sample, 0, len(values)+len(checks))
	for _, x := range values {
		w := x.w
		if shifted[w.gi] {
			continue
		}
		k, _ := kindSpec(w.obj.Kind)
		v := x.v
		if w.metric.Scale != 0 {
			v *= w.metric.Scale
		}
		if w.metric.Factor && w.obj.Factor != 0 {
			v *= w.obj.Factor
		}
		vals := k.Labels(w.obj)
		keys := k.LabelKeys
		if len(vals) < len(keys) {
			if w.metric.Window != "" {
				vals = append(vals, w.metric.Window)
			} else {
				keys = keys[:len(vals)]
			}
		}
		res.Samples = append(res.Samples, Sample{
			Name: w.metric.Name, Help: w.metric.Help, Counter: w.metric.Counter,
			Keys: keys, Values: vals, Value: v, Obj: w.obj.ID,
		})
	}
	for gi, g := range groups {
		o := g.obj
		if o.Kind != KindIf || shifted[gi] || !answered[gi] {
			continue
		}
		alias := o.Alias
		if ch := checks[gi]; ch != nil && ch.gotAlias {
			alias = ch.alias
		}
		res.Samples = append(res.Samples, Sample{
			Name: IfInfoMetric, Help: "Live port names and description (ifAlias); value is always 1.",
			Keys: []string{"ifIndex", "ifName", "ifDescr", "ifAlias"}, Values: []string{o.Idx, o.Name, o.Descr, alias},
			Value: 1, Obj: o.ID,
		})
	}
	return res, nil
}
