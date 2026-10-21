package snmpx

import (
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
}

type wanted struct {
	obj    store.Object
	metric Metric
}

// Poll GETs only the OIDs of the selected objects and turns them into samples.
// Objects that are missing on the device or not selected are skipped.
func Poll(s Session, objs []store.Object) ([]Sample, error) {
	v1 := s.Version() == "1"
	byOID := map[string]wanted{}
	var oids []string
	for _, o := range objs {
		if !o.Selected || o.State == store.StateMissing {
			continue
		}
		k, ok := Kinds[o.Kind]
		if !ok {
			continue
		}
		for _, m := range k.Metrics {
			if (v1 && m.NotV1) || (!v1 && m.OnlyV1) {
				continue
			}
			oid := m.OID + "." + o.Idx
			if _, dup := byOID[oid]; dup {
				continue
			}
			byOID[oid] = wanted{obj: o, metric: m}
			oids = append(oids, oid)
		}
	}
	if len(oids) == 0 {
		return nil, nil
	}
	pdus, err := s.Get(oids)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(pdus))
	for _, p := range pdus {
		w, ok := byOID[trimOID(p.Name)]
		if !ok {
			continue
		}
		k := Kinds[w.obj.Kind]
		v := asFloat(p)
		if w.metric.Scale != 0 {
			v *= w.metric.Scale
		}
		if w.metric.Factor && w.obj.Factor != 0 {
			v *= w.obj.Factor
		}
		vals := k.Labels(w.obj)
		if len(vals) < len(k.LabelKeys) {
			vals = append(vals, w.metric.Window)
		}
		out = append(out, Sample{
			Name: w.metric.Name, Help: w.metric.Help, Counter: w.metric.Counter,
			Keys: k.LabelKeys, Values: vals, Value: v,
		})
	}
	return out, nil
}
