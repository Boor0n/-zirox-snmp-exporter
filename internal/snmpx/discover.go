package snmpx

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gosnmp/gosnmp"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// System OIDs.
const (
	oidSysDescr    = "1.3.6.1.2.1.1.1.0"
	oidSysObjectID = "1.3.6.1.2.1.1.2.0"
	oidSysName     = "1.3.6.1.2.1.1.5.0"
)

// Interface columns walked during discovery.
const (
	oidIfDescr     = "1.3.6.1.2.1.2.2.1.2"
	oidIfType      = "1.3.6.1.2.1.2.2.1.3"
	oidIfSpeed     = "1.3.6.1.2.1.2.2.1.5"
	oidIfOper      = "1.3.6.1.2.1.2.2.1.8"
	oidIfName      = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfHighSpeed = "1.3.6.1.2.1.31.1.1.1.15"
	oidIfAlias     = "1.3.6.1.2.1.31.1.1.1.18"

	oidEntPhysName     = "1.3.6.1.2.1.47.1.1.1.1.7"
	oidEntSensorType   = "1.3.6.1.2.1.99.1.1.1.1"
	oidEntSensorScale  = "1.3.6.1.2.1.99.1.1.1.2"
	oidEntSensorPrec   = "1.3.6.1.2.1.99.1.1.1.3"
	oidCpmCPUPhys      = "1.3.6.1.4.1.9.9.109.1.1.1.1.2"
	oidCpmCPU1min      = "1.3.6.1.4.1.9.9.109.1.1.1.1.7"
	oidCiscoMemName    = "1.3.6.1.4.1.9.9.48.1.1.1.2"
	oidNexusCPU        = "1.3.6.1.4.1.9.9.305.1.1.1.0"
	oidNexusMem        = "1.3.6.1.4.1.9.9.305.1.1.2.0"
	oidEnvTempDescr    = "1.3.6.1.4.1.9.9.13.1.3.1.2"
	oidEnvFanDescr     = "1.3.6.1.4.1.9.9.13.1.4.1.2"
	oidEnvPSUDescr     = "1.3.6.1.4.1.9.9.13.1.5.1.2"
	oidHrProcessorLoad = "1.3.6.1.2.1.25.3.3.1.2"
	oidHrStorageType   = "1.3.6.1.2.1.25.2.3.1.2"
	oidHrStorageDescr  = "1.3.6.1.2.1.25.2.3.1.3"
	oidHrStorageUnits  = "1.3.6.1.2.1.25.2.3.1.4"
	oidUcdMemTotal     = "1.3.6.1.4.1.2021.4.5.0"
	oidFwConnCurrent   = "1.3.6.1.4.1.9.9.147.1.2.2.2.1.5.40.6"
	oidCrasNumSessions = "1.3.6.1.4.1.9.9.392.1.3.1.0"
	oidBgpPeerRemoteAs = "1.3.6.1.2.1.15.3.1.9"
)

// hrStorage types worth exporting: RAM, virtual memory, fixed disks.
var hrStorageTypes = map[string]bool{
	"1.3.6.1.2.1.25.2.1.2": true,
	"1.3.6.1.2.1.25.2.1.3": true,
	"1.3.6.1.2.1.25.2.1.4": true,
}

// ENTITY-SENSOR-MIB sensor types we export, with their unit names.
var sensorUnits = map[int]string{
	3: "volts_ac", 4: "volts_dc", 5: "amperes", 6: "watts", 7: "hertz",
	8: "celsius", 9: "percent_rh", 10: "rpm", 11: "cmm", 14: "dbm",
}

// physicalIfTypes are auto-selected when up: ethernet, fast/gigabit, fibre channel, LAG.
var physicalIfTypes = map[int]bool{6: true, 62: true, 56: true, 69: true, 117: true, 161: true}

// SystemInfo is what discovery learns about the device itself.
type SystemInfo struct {
	SysDescr    string
	SysObjectID string
	SysName     string
}

// Discover walks the device once and returns everything that can be polled.
func Discover(s Session) (SystemInfo, []store.Object, error) {
	var info SystemInfo
	pdus, err := s.Get([]string{oidSysDescr, oidSysObjectID, oidSysName})
	if err != nil {
		return info, nil, fmt.Errorf("device not responding: %w", err)
	}
	for _, p := range pdus {
		switch trimOID(p.Name) {
		case oidSysDescr:
			info.SysDescr = asString(p)
		case oidSysObjectID:
			info.SysObjectID = trimOID(asString(p))
		case oidSysName:
			info.SysName = asString(p)
		}
	}
	if len(pdus) == 0 {
		return info, nil, fmt.Errorf("device returned no sysDescr/sysName")
	}

	var objs []store.Object
	ifs, err := discoverInterfaces(s)
	if err != nil {
		return info, nil, err
	}
	objs = append(objs, ifs...)
	// Health objects are best effort: a device that lacks a MIB returns nothing.
	objs = append(objs, discoverHealth(s)...)
	objs = append(objs, discoverCustom(s)...)
	return info, uniqueKeys(objs), nil
}

func discoverInterfaces(s Session) ([]store.Object, error) {
	descr, err := walkColumn(s, oidIfDescr)
	if err != nil {
		return nil, fmt.Errorf("walk ifDescr: %w", err)
	}
	cols := map[string]map[string]gosnmp.SnmpPDU{}
	for _, oid := range []string{oidIfType, oidIfSpeed, oidIfOper, oidIfName, oidIfHighSpeed, oidIfAlias} {
		c, err := walkColumn(s, oid)
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", oid, err)
		}
		cols[oid] = c
	}
	var out []store.Object
	for idx, d := range descr {
		o := store.Object{Kind: KindIf, Idx: idx, Descr: asString(d), Factor: 1}
		if p, ok := cols[oidIfName][idx]; ok {
			o.Name = asString(p)
		}
		if o.Name == "" {
			o.Name = o.Descr
		}
		if p, ok := cols[oidIfAlias][idx]; ok {
			o.Alias = asString(p)
		}
		if p, ok := cols[oidIfType][idx]; ok {
			o.IfType = int(asFloat(p))
		}
		if p, ok := cols[oidIfOper][idx]; ok {
			o.OperStatus = int(asFloat(p))
		}
		if p, ok := cols[oidIfHighSpeed][idx]; ok && asFloat(p) > 0 {
			o.SpeedMbps = int64(asFloat(p))
		} else if p, ok := cols[oidIfSpeed][idx]; ok {
			o.SpeedMbps = int64(asFloat(p) / 1e6)
		}
		o.Key = o.Name
		out = append(out, o)
	}
	return out, nil
}

func discoverHealth(s Session) []store.Object {
	var out []store.Object
	var physNames map[string]gosnmp.SnmpPDU
	physName := func(idx string) string {
		if physNames == nil {
			physNames, _ = walkColumn(s, oidEntPhysName)
			if physNames == nil {
				physNames = map[string]gosnmp.SnmpPDU{}
			}
		}
		if p, ok := physNames[idx]; ok {
			return asString(p)
		}
		return ""
	}
	add := func(kind, idx, name string, factor float64, descr string) {
		out = append(out, store.Object{Kind: kind, Idx: idx, Name: name, Key: name, Factor: factor, Descr: descr})
	}

	// Cisco CPU (IOS, IOS-XE, NX-OS, ASA).
	if cpus, _ := walkColumn(s, oidCpmCPU1min); len(cpus) > 0 {
		phys, _ := walkColumn(s, oidCpmCPUPhys)
		for idx := range cpus {
			name := ""
			if p, ok := phys[idx]; ok && asFloat(p) > 0 {
				name = physName(strconv.Itoa(int(asFloat(p))))
			}
			if name == "" {
				name = "CPU " + idx
			}
			add(KindCiscoCPU, idx, name, 1, "")
		}
	}
	// Nexus system-wide CPU and memory.
	if pdus, _ := s.Get([]string{oidNexusCPU, oidNexusMem}); len(pdus) > 0 {
		for _, p := range pdus {
			switch trimOID(p.Name) {
			case oidNexusCPU:
				add(KindNexusCPU, "0", "system", 1, "")
			case oidNexusMem:
				add(KindNexusMem, "0", "system", 1, "")
			}
		}
	}
	// Cisco memory pools.
	if pools, _ := walkColumn(s, oidCiscoMemName); len(pools) > 0 {
		for idx, p := range pools {
			add(KindCiscoMem, idx, asString(p), 1, "")
		}
	}
	// ENTITY-SENSOR-MIB sensors (Nexus, newer IOS-XE).
	if types, _ := walkColumn(s, oidEntSensorType); len(types) > 0 {
		scales, _ := walkColumn(s, oidEntSensorScale)
		precs, _ := walkColumn(s, oidEntSensorPrec)
		for idx, p := range types {
			unit, ok := sensorUnits[int(asFloat(p))]
			if !ok {
				continue
			}
			factor := 1.0
			if sc, ok := scales[idx]; ok {
				factor *= math.Pow(10, float64((int(asFloat(sc))-9)*3))
			}
			if pr, ok := precs[idx]; ok {
				factor /= math.Pow(10, asFloat(pr))
			}
			name := physName(idx)
			if name == "" {
				name = "sensor " + idx
			}
			if unit == "celsius" {
				add(KindEntTemp, idx, name, factor, "")
			} else {
				add(KindEntSensor, idx, name, factor, unit)
			}
		}
	} else if temps, _ := walkColumn(s, oidEnvTempDescr); len(temps) > 0 {
		// Older IOS and ASA expose temperatures only via CISCO-ENVMON-MIB.
		for idx, p := range temps {
			add(KindEnvTemp, idx, asString(p), 1, "")
		}
	}
	if fans, _ := walkColumn(s, oidEnvFanDescr); len(fans) > 0 {
		for idx, p := range fans {
			add(KindEnvFan, idx, asString(p), 1, "")
		}
	}
	if psus, _ := walkColumn(s, oidEnvPSUDescr); len(psus) > 0 {
		for idx, p := range psus {
			add(KindEnvPSU, idx, asString(p), 1, "")
		}
	}
	// Servers: HOST-RESOURCES-MIB and UCD-SNMP-MIB.
	if loads, _ := walkColumn(s, oidHrProcessorLoad); len(loads) > 0 {
		for idx := range loads {
			add(KindHostCPU, idx, "CPU "+idx, 1, "")
		}
	}
	if types, _ := walkColumn(s, oidHrStorageType); len(types) > 0 {
		descrs, _ := walkColumn(s, oidHrStorageDescr)
		units, _ := walkColumn(s, oidHrStorageUnits)
		for idx, p := range types {
			if !hrStorageTypes[trimOID(asString(p))] {
				continue
			}
			factor := 1.0
			if u, ok := units[idx]; ok && asFloat(u) > 0 {
				factor = asFloat(u)
			}
			name := "storage " + idx
			if d, ok := descrs[idx]; ok {
				name = asString(d)
			}
			add(KindHostStorage, idx, name, factor, "")
		}
	}
	if pdus, _ := s.Get([]string{oidUcdMemTotal}); len(pdus) > 0 {
		add(KindUcdMem, "0", "system", 1, "")
	}
	// Firewalls (ASA): connection table and remote-access VPN sessions.
	if pdus, _ := s.Get([]string{oidFwConnCurrent}); len(pdus) > 0 {
		add(KindFwConn, "40", "connections", 1, "")
	}
	if pdus, _ := s.Get([]string{oidCrasNumSessions}); len(pdus) > 0 {
		add(KindRAVPN, "0", "VPN sessions", 1, "")
	}
	// BGP neighbours, keyed by peer address.
	if peers, _ := walkColumn(s, oidBgpPeerRemoteAs); len(peers) > 0 {
		for idx, p := range peers {
			add(KindBGPPeer, idx, idx, 1, strconv.Itoa(int(asFloat(p))))
		}
	}
	return out
}

// uniqueKeys makes kind+key unique, so two ports with the same name both survive.
func uniqueKeys(objs []store.Object) []store.Object {
	seen := map[string]bool{}
	for i := range objs {
		o := &objs[i]
		if o.Key == "" {
			o.Key = o.Idx
		}
		if seen[o.Kind+"\x00"+o.Key] {
			o.Key = o.Key + " #" + o.Idx
		}
		seen[o.Kind+"\x00"+o.Key] = true
	}
	return objs
}

// Merge combines a fresh discovery with what the device had before.
// Existing objects keep their selection; new ones are auto-selected when
// autoSelect is on; vanished ones are kept as missing so nothing is lost.
// On the first discovery (no old objects) nothing is marked as new.
func Merge(old, found []store.Object, autoSelect bool) []store.Object {
	prev := map[string]store.Object{}
	for _, o := range old {
		prev[o.Kind+"\x00"+o.Key] = o
	}
	first := len(old) == 0
	out := make([]store.Object, 0, len(found))
	for _, o := range found {
		k := o.Kind + "\x00" + o.Key
		if p, ok := prev[k]; ok {
			o.Selected = p.Selected
			o.State = store.StateActive
			if p.State == store.StateNew {
				o.State = store.StateNew
			}
			delete(prev, k)
		} else {
			o.Selected = autoSelect && shouldAutoSelect(o)
			if strings.HasPrefix(o.Kind, "custom_") {
				// Custom OIDs carry their own auto-select switch.
				o.Selected = autoSelectCustom(o.Kind)
			}
			o.State = store.StateNew
			if first {
				o.State = store.StateActive
			}
		}
		out = append(out, o)
	}
	for _, o := range old {
		if _, ok := prev[o.Kind+"\x00"+o.Key]; ok {
			o.State = store.StateMissing
			out = append(out, o)
		}
	}
	return out
}

func shouldAutoSelect(o store.Object) bool {
	if o.Kind != KindIf {
		return true
	}
	return o.OperStatus == 1 && physicalIfTypes[o.IfType]
}

// walkColumn walks one table column and returns PDUs keyed by row index.
func walkColumn(s Session, column string) (map[string]gosnmp.SnmpPDU, error) {
	pdus, err := s.Walk(column)
	if err != nil {
		return nil, err
	}
	out := make(map[string]gosnmp.SnmpPDU, len(pdus))
	prefix := column + "."
	for _, p := range pdus {
		name := trimOID(p.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		switch p.Type {
		case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView:
			continue
		}
		out[strings.TrimPrefix(name, prefix)] = p
	}
	return out, nil
}

func trimOID(s string) string { return strings.TrimPrefix(s, ".") }

func asString(p gosnmp.SnmpPDU) string {
	switch v := p.Value.(type) {
	case []byte:
		return strings.TrimRight(string(v), "\x00")
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func asFloat(p gosnmp.SnmpPDU) float64 {
	switch v := p.Value.(type) {
	case []byte:
		f, _ := strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	}
	switch v := p.Value.(type) {
	case float32:
		return float64(v)
	case float64:
		return v
	}
	f, _ := gosnmp.ToBigInt(p.Value).Float64()
	return f
}
