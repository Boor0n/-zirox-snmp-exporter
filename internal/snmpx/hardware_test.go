package snmpx_test

import (
	"math"
	"testing"

	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx/snmptest"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

func pollAll(t *testing.T, a *snmptest.Agent, objs []store.Object) map[string]float64 {
	t.Helper()
	for i := range objs {
		objs[i].Selected = true
		objs[i].ID = int64(i + 1)
	}
	s, _ := a.Dial()
	samples, err := snmpx.Poll(s, objs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, sm := range samples {
		k := sm.Name
		for _, v := range sm.Values {
			k += "/" + v
		}
		got[k] = sm.Value
	}
	return got
}

func TestDellIDRAC(t *testing.T) {
	a := snmptest.New()
	a.Set("1.3.6.1.2.1.1.1.0", "Linux idrac 4.19")
	a.Set("1.3.6.1.2.1.1.5.0", "esx-01-idrac")
	snmptest.AddPort(a, 1, "eth0", 6, true)
	a.Set("1.3.6.1.4.1.674.10892.5.2.1.0", 3)
	a.Set("1.3.6.1.4.1.674.10892.5.4.700.20.1.6.1.1", 230)
	a.Set("1.3.6.1.4.1.674.10892.5.4.700.20.1.5.1.1", 3)
	a.Set("1.3.6.1.4.1.674.10892.5.4.700.20.1.8.1.1", "System Board Inlet Temp")
	a.Set("1.3.6.1.4.1.674.10892.5.4.700.12.1.6.1.1", 5400)
	a.Set("1.3.6.1.4.1.674.10892.5.4.700.12.1.5.1.1", 3)
	a.Set("1.3.6.1.4.1.674.10892.5.4.700.12.1.8.1.1", "System Board Fan1")
	a.Set("1.3.6.1.4.1.674.10892.5.4.600.12.1.5.1.1", 5)
	a.Set("1.3.6.1.4.1.674.10892.5.4.600.12.1.8.1.1", "PS1 Status")
	a.Set("1.3.6.1.4.1.674.10892.5.5.1.20.130.4.1.2.1", "Physical Disk 0:1:0")
	a.Set("1.3.6.1.4.1.674.10892.5.5.1.20.130.4.1.24.1", 3)
	_, objs := discover(t, a)
	for _, want := range []struct{ kind, key string }{
		{snmpx.KindDellSystem, "system"}, {snmpx.KindDellTemp, "System Board Inlet Temp"}, {snmpx.KindDellFan, "System Board Fan1"},
		{snmpx.KindDellPSU, "PS1 Status"}, {snmpx.KindDellDisk, "Physical Disk 0:1:0"},
	} {
		if find(objs, want.kind, want.key) == nil {
			t.Errorf("missing %s %q in %+v", want.kind, want.key, objs)
		}
	}
	got := pollAll(t, a, objs)
	if v := got["zirox_temperature_celsius/1.1/System Board Inlet Temp"]; math.Abs(v-23) > 1e-9 {
		t.Errorf("temperature = %v (%v)", v, got)
	}
	if got["zirox_fan_rpm/1.1/System Board Fan1"] != 5400 || got["zirox_dell_status/1.1/PS1 Status/psu"] != 5 ||
		got["zirox_dell_status/0/system/system"] != 3 || got["zirox_dell_status/1/Physical Disk 0:1:0/disk"] != 3 {
		t.Errorf("samples = %v", got)
	}
}

func TestHPEiLO(t *testing.T) {
	a := snmptest.New()
	a.Set("1.3.6.1.2.1.1.1.0", "HPE ProLiant")
	a.Set("1.3.6.1.2.1.1.5.0", "dl380")
	snmptest.AddPort(a, 1, "eth0", 6, true)
	a.Set("1.3.6.1.4.1.232.6.1.3.0", 2)
	a.Set("1.3.6.1.4.1.232.6.2.6.8.1.3.0.1", 11)
	a.Set("1.3.6.1.4.1.232.6.2.6.8.1.4.0.1", 21)
	a.Set("1.3.6.1.4.1.232.6.2.6.8.1.6.0.1", 2)
	a.Set("1.3.6.1.4.1.232.6.2.6.8.1.4.0.2", -99) // absent sensor
	a.Set("1.3.6.1.4.1.232.6.2.6.7.1.3.0.1", 3)
	a.Set("1.3.6.1.4.1.232.6.2.6.7.1.4.0.1", 3)
	a.Set("1.3.6.1.4.1.232.6.2.6.7.1.9.0.1", 2)
	a.Set("1.3.6.1.4.1.232.6.2.6.7.1.4.0.2", 2) // absent fan
	a.Set("1.3.6.1.4.1.232.6.2.6.7.1.9.0.2", 1)
	a.Set("1.3.6.1.4.1.232.6.2.9.3.1.3.0.1", 3)
	a.Set("1.3.6.1.4.1.232.6.2.9.3.1.4.0.1", 4)
	_, objs := discover(t, a)
	if find(objs, snmpx.KindHPTemp, "ambient 0.1") == nil || find(objs, snmpx.KindHPFan, "system 0.1") == nil ||
		find(objs, snmpx.KindHPPSU, "power supply 0.1") == nil || find(objs, snmpx.KindHPSystem, "system") == nil {
		t.Fatalf("objs = %+v", objs)
	}
	for _, o := range objs {
		if o.Idx == "0.2" {
			t.Errorf("absent %s discovered", o.Kind)
		}
	}
	got := pollAll(t, a, objs)
	if got["zirox_temperature_celsius/0.1/ambient 0.1"] != 21 || got["zirox_hp_condition/0.1/power supply 0.1/psu"] != 4 || got["zirox_hp_condition/0/system/system"] != 2 {
		t.Errorf("samples = %v", got)
	}
}

func TestIOSXRMemory(t *testing.T) {
	a := snmptest.New()
	a.Set("1.3.6.1.2.1.1.1.0", "Cisco IOS XR Software, NCS-5500")
	a.Set("1.3.6.1.2.1.1.5.0", "ncs-01")
	snmptest.AddPort(a, 1, "HundredGigE0/0/0/0", 6, true)
	a.Set("1.3.6.1.4.1.9.9.221.1.1.1.1.3.8193.1", "processor")
	a.Set("1.3.6.1.4.1.9.9.221.1.1.1.1.18.8193.1", uint64(3_000_000_000))
	a.Set("1.3.6.1.4.1.9.9.221.1.1.1.1.20.8193.1", uint64(5_000_000_000))
	_, objs := discover(t, a)
	if find(objs, snmpx.KindCiscoEMem, "processor 8193.1") == nil {
		t.Fatalf("objs = %+v", objs)
	}
	got := pollAll(t, a, objs)
	if got["zirox_memory_used_bytes/8193.1/processor 8193.1"] != 3e9 || got["zirox_memory_free_bytes/8193.1/processor 8193.1"] != 5e9 {
		t.Errorf("samples = %v", got)
	}
}
