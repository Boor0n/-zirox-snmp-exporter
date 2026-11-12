package snmpx

import (
	"strconv"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// Server hardware (Dell iDRAC, HPE iLO) and Cisco IOS-XR memory, found by
// the same full discovery as everything else.
const (
	KindDellSystem = "dell_system"
	KindDellTemp   = "dell_temp"
	KindDellFan    = "dell_fan"
	KindDellPSU    = "dell_psu"
	KindDellDisk   = "dell_disk"
	KindHPSystem   = "hp_system"
	KindHPTemp     = "hp_temp"
	KindHPFan      = "hp_fan"
	KindHPPSU      = "hp_psu"
	KindCiscoEMem  = "mem_cisco_enh"
)

const (
	// IDRAC-MIB-SMIv2 (iDRAC 7 and later).
	oidDellGlobalStatus = "1.3.6.1.4.1.674.10892.5.2.1"
	oidDellTempReading  = "1.3.6.1.4.1.674.10892.5.4.700.20.1.6"
	oidDellTempName     = "1.3.6.1.4.1.674.10892.5.4.700.20.1.8"
	oidDellTempStatus   = "1.3.6.1.4.1.674.10892.5.4.700.20.1.5"
	oidDellFanReading   = "1.3.6.1.4.1.674.10892.5.4.700.12.1.6"
	oidDellFanName      = "1.3.6.1.4.1.674.10892.5.4.700.12.1.8"
	oidDellFanStatus    = "1.3.6.1.4.1.674.10892.5.4.700.12.1.5"
	oidDellPSUStatus    = "1.3.6.1.4.1.674.10892.5.4.600.12.1.5"
	oidDellPSUName      = "1.3.6.1.4.1.674.10892.5.4.600.12.1.8"
	oidDellDiskName     = "1.3.6.1.4.1.674.10892.5.5.1.20.130.4.1.2"
	oidDellDiskStatus   = "1.3.6.1.4.1.674.10892.5.5.1.20.130.4.1.24"

	// CPQHLTH-MIB (HPE ProLiant with iLO).
	oidHPMibCondition = "1.3.6.1.4.1.232.6.1.3"
	oidHPTempLocale   = "1.3.6.1.4.1.232.6.2.6.8.1.3"
	oidHPTempCelsius  = "1.3.6.1.4.1.232.6.2.6.8.1.4"
	oidHPTempCond     = "1.3.6.1.4.1.232.6.2.6.8.1.6"
	oidHPFanLocale    = "1.3.6.1.4.1.232.6.2.6.7.1.3"
	oidHPFanPresent   = "1.3.6.1.4.1.232.6.2.6.7.1.4"
	oidHPFanCond      = "1.3.6.1.4.1.232.6.2.6.7.1.9"
	oidHPPSUPresent   = "1.3.6.1.4.1.232.6.2.9.3.1.3"
	oidHPPSUCond      = "1.3.6.1.4.1.232.6.2.9.3.1.4"

	// CISCO-ENHANCED-MEMPOOL-MIB (IOS-XR, NCS; also newer IOS-XE).
	oidCempName   = "1.3.6.1.4.1.9.9.221.1.1.1.1.3"
	oidCempHCUsed = "1.3.6.1.4.1.9.9.221.1.1.1.1.18"
	oidCempHCFree = "1.3.6.1.4.1.9.9.221.1.1.1.1.20"
)

const (
	dellStatusHelp = "Dell iDRAC status (1=other, 2=unknown, 3=ok, 4=nonCritical, 5=critical, 6=nonRecoverable)."
	hpCondHelp     = "HPE iLO condition (1=other, 2=ok, 3=degraded, 4=failed)."
)

var hwLabels = []string{"index", "name", "component"}

func init() {
	for k, v := range map[string]Kind{
		KindDellSystem: {Category: "Sensors", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_dell_status", Help: dellStatusHelp, OID: oidDellGlobalStatus, Window: "system"},
		}},
		KindDellTemp: {Category: "Sensors", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_temperature_celsius", Help: "Temperature in degrees Celsius.", OID: oidDellTempReading, Scale: 0.1},
			{Name: "zirox_dell_status", Help: dellStatusHelp, OID: oidDellTempStatus, Window: "temperature"},
		}},
		KindDellFan: {Category: "Sensors", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_fan_rpm", Help: "Fan speed in revolutions per minute.", OID: oidDellFanReading},
			{Name: "zirox_dell_status", Help: dellStatusHelp, OID: oidDellFanStatus, Window: "fan"},
		}},
		KindDellPSU: {Category: "Sensors", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_dell_status", Help: dellStatusHelp, OID: oidDellPSUStatus, Window: "psu"},
		}},
		KindDellDisk: {Category: "Storage", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_dell_status", Help: dellStatusHelp, OID: oidDellDiskStatus, Window: "disk"},
		}},
		KindHPSystem: {Category: "Sensors", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_hp_condition", Help: hpCondHelp, OID: oidHPMibCondition, Window: "system"},
		}},
		KindHPTemp: {Category: "Sensors", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_temperature_celsius", Help: "Temperature in degrees Celsius.", OID: oidHPTempCelsius},
			{Name: "zirox_hp_condition", Help: hpCondHelp, OID: oidHPTempCond, Window: "temperature"},
		}},
		KindHPFan: {Category: "Sensors", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_hp_condition", Help: hpCondHelp, OID: oidHPFanCond, Window: "fan"},
		}},
		KindHPPSU: {Category: "Sensors", LabelKeys: hwLabels, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_hp_condition", Help: hpCondHelp, OID: oidHPPSUCond, Window: "psu"},
		}},
		KindCiscoEMem: {Category: "Memory", LabelKeys: []string{"index", "name"}, Labels: idxName, Metrics: []Metric{
			{Name: "zirox_memory_used_bytes", Help: "Memory used in bytes.", OID: oidCempHCUsed, NotV1: true},
			{Name: "zirox_memory_free_bytes", Help: "Memory free in bytes.", OID: oidCempHCFree, NotV1: true},
		}},
	} {
		Kinds[k] = v
	}
}

// hpLocales names the cpqHe*Locale values.
var hpLocales = map[int]string{1: "other", 2: "unknown", 3: "system", 4: "system board", 5: "I/O board", 6: "CPU",
	7: "memory", 8: "storage", 9: "removable media", 10: "power supply", 11: "ambient", 12: "chassis", 13: "bridge card"}

func hpName(locale map[string]float64, idx, fallback string) string {
	if l, ok := locale[idx]; ok {
		if n, ok := hpLocales[int(l)]; ok {
			return n + " " + idx
		}
	}
	return fallback + " " + idx
}

// discoverHardware finds Dell, HPE and IOS-XR objects; cheap on devices
// that have none, one GET-NEXT walk per table.
func discoverHardware(s Session, haveCiscoMem bool) []store.Object {
	var out []store.Object
	add := func(kind, idx, name string) {
		out = append(out, store.Object{Kind: kind, Idx: idx, Name: name, Key: name, Factor: 1})
	}
	strs := func(col string) map[string]string {
		m := map[string]string{}
		pdus, _ := walkColumn(s, col)
		for idx, p := range pdus {
			m[idx] = asString(p)
		}
		return m
	}
	nums := func(col string) map[string]float64 {
		m := map[string]float64{}
		pdus, _ := walkColumn(s, col)
		for idx, p := range pdus {
			m[idx] = asFloat(p)
		}
		return m
	}
	named := func(names map[string]string, idx, fallback string) string {
		if n := names[idx]; n != "" {
			return n
		}
		return fallback + " " + idx
	}

	// Dell iDRAC.
	if pdus, _ := s.Get([]string{oidDellGlobalStatus + ".0"}); len(pdus) > 0 {
		add(KindDellSystem, "0", "system")
		names := strs(oidDellTempName)
		for idx := range nums(oidDellTempReading) {
			add(KindDellTemp, idx, named(names, idx, "temperature"))
		}
		names = strs(oidDellFanName)
		for idx := range nums(oidDellFanReading) {
			add(KindDellFan, idx, named(names, idx, "fan"))
		}
		names = strs(oidDellPSUName)
		for idx := range nums(oidDellPSUStatus) {
			add(KindDellPSU, idx, named(names, idx, "PSU"))
		}
		names = strs(oidDellDiskName)
		for idx := range nums(oidDellDiskStatus) {
			add(KindDellDisk, idx, named(names, idx, "disk"))
		}
	}

	// HPE iLO.
	if pdus, _ := s.Get([]string{oidHPMibCondition + ".0"}); len(pdus) > 0 {
		add(KindHPSystem, "0", "system")
		loc := nums(oidHPTempLocale)
		for idx, v := range nums(oidHPTempCelsius) {
			if v > 0 { // absent sensors read 0 or -99
				add(KindHPTemp, idx, hpName(loc, idx, "temperature"))
			}
		}
		loc = nums(oidHPFanLocale)
		present := nums(oidHPFanPresent)
		for idx := range nums(oidHPFanCond) {
			if p, ok := present[idx]; ok && p == 2 {
				continue
			}
			add(KindHPFan, idx, hpName(loc, idx, "fan"))
		}
		present = nums(oidHPPSUPresent)
		for idx := range nums(oidHPPSUCond) {
			if p, ok := present[idx]; ok && p == 2 {
				continue
			}
			add(KindHPPSU, idx, "power supply "+idx)
		}
	}

	// IOS-XR has no CISCO-MEMORY-POOL-MIB; its memory is in the enhanced pool table.
	if !haveCiscoMem {
		for idx, name := range strs(oidCempName) {
			if name == "" {
				name = "pool " + idx
			}
			add(KindCiscoEMem, idx, name+" "+idx)
		}
	}
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = out[i].Kind + " " + strconv.Itoa(i)
			out[i].Key = out[i].Name
		}
	}
	return out
}
