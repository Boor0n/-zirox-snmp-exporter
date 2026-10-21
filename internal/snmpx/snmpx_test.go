package snmpx_test

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx/snmptest"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

func discover(t *testing.T, a *snmptest.Agent) (snmpx.SystemInfo, []store.Object) {
	t.Helper()
	s, _ := a.Dial()
	info, objs, err := snmpx.Discover(s)
	if err != nil {
		t.Fatal(err)
	}
	return info, objs
}

func find(objs []store.Object, kind, key string) *store.Object {
	for i := range objs {
		if objs[i].Kind == kind && objs[i].Key == key {
			return &objs[i]
		}
	}
	return nil
}

func TestDiscoverSwitch(t *testing.T) {
	info, objs := discover(t, snmptest.Switch(4))
	if info.SysName != "core-nx-01" || info.SysObjectID != "1.3.6.1.4.1.9.12.3.1.3.1812" {
		t.Fatalf("info = %+v", info)
	}
	p := find(objs, snmpx.KindIf, "Ethernet1/3")
	if p == nil || p.Idx != "3" || p.OperStatus != 1 || p.SpeedMbps != 10000 || p.Alias != "to Ethernet1/3" || p.IfType != 6 {
		t.Fatalf("port = %+v", p)
	}
	cpu := find(objs, snmpx.KindCiscoCPU, "Supervisor CPU")
	if cpu == nil || cpu.Idx != "1" {
		t.Fatalf("cpu = %+v (objs %v)", cpu, objs)
	}
	temp := find(objs, snmpx.KindEntTemp, "Module 1 Inlet Temp")
	if temp == nil || math.Abs(temp.Factor-0.1) > 1e-9 {
		t.Fatalf("temp = %+v", temp)
	}
}

func TestMergeKeepsSelectionAndMarksChanges(t *testing.T) {
	a := snmptest.Switch(4)
	_, first := discover(t, a)
	objs := snmpx.Merge(nil, first, true)
	sel := map[string]bool{}
	for _, o := range objs {
		if o.State != store.StateActive {
			t.Fatalf("first discovery must not mark %s as %s", o.Key, o.State)
		}
		sel[o.Key] = o.Selected
	}
	// Up physical ports and health objects are auto-selected; down ports and VLANs are not.
	want := map[string]bool{"Ethernet1/1": true, "Ethernet1/2": false, "Ethernet1/3": true, "Vlan10": false, "Supervisor CPU": true, "Module 1 Inlet Temp": true}
	for k, v := range want {
		if sel[k] != v {
			t.Errorf("%s selected=%v, want %v", k, sel[k], v)
		}
	}

	// User unticks Ethernet1/1 and ticks Ethernet1/2.
	for i := range objs {
		switch objs[i].Key {
		case "Ethernet1/1":
			objs[i].Selected = false
		case "Ethernet1/2":
			objs[i].Selected = true
		}
	}
	// Device changes: a new port appears with a shifted ifIndex, Ethernet1/4 disappears.
	snmptest.AddPort(a, 9, "Ethernet1/5", 6, true)
	a.Delete("1.3.6.1.2.1.2.2.1.2.4")
	_, second := discover(t, a)
	merged := snmpx.Merge(objs, second, true)
	check := func(key, state string, selected bool) {
		t.Helper()
		o := find(merged, snmpx.KindIf, key)
		if o == nil {
			t.Fatalf("%s missing from merge", key)
		}
		if o.State != state || o.Selected != selected {
			t.Errorf("%s: state=%s selected=%v, want %s %v", key, o.State, o.Selected, state, selected)
		}
	}
	check("Ethernet1/1", store.StateActive, false)
	check("Ethernet1/2", store.StateActive, true)
	check("Ethernet1/5", store.StateNew, true)
	check("Ethernet1/4", store.StateMissing, false)
}

func TestPollOnlySelected(t *testing.T) {
	a := snmptest.Switch(48)
	_, found := discover(t, a)
	objs := snmpx.Merge(nil, found, false)
	for i := range objs {
		k := objs[i].Key
		objs[i].Selected = k == "Ethernet1/5" || k == "Ethernet1/10" || k == "Ethernet1/12" || k == "Supervisor CPU" || k == "Module 1 Inlet Temp"
	}
	a.Counts()
	s, _ := a.Dial()
	samples, err := snmpx.Poll(s, objs)
	if err != nil {
		t.Fatal(err)
	}
	gets, walks, oids := a.Counts()
	if walks != 0 {
		t.Errorf("poll must not walk, did %d walks", walks)
	}
	// 3 ports x 11 metrics (v2c) + 2 CPU + 2 temp = 37 OIDs.
	if oids != 37 || gets != 1 {
		t.Errorf("gets=%d oids=%d, want 1 call (fake) with 37 OIDs", gets, oids)
	}
	got := map[string]float64{}
	for _, sm := range samples {
		got[sm.Name+"/"+sm.Values[1]+"/"+last(sm.Values)] = sm.Value
	}
	if got["ifHCInOctets/Ethernet1/10/to Ethernet1/10"] != 10000 {
		t.Errorf("ifHCInOctets Ethernet1/10 = %v", got["ifHCInOctets/Ethernet1/10/to Ethernet1/10"])
	}
	if got["zirox_cpu_utilization_percent/Supervisor CPU/5m"] != 10 || got["zirox_cpu_utilization_percent/Supervisor CPU/1m"] != 12 {
		t.Errorf("cpu samples wrong: %v", got)
	}
	if v := got["zirox_temperature_celsius/Module 1 Inlet Temp/Module 1 Inlet Temp"]; math.Abs(v-31.5) > 1e-9 {
		t.Errorf("temperature = %v, want 31.5", v)
	}
	if _, ok := got["ifInOctets/Ethernet1/10/to Ethernet1/10"]; ok {
		t.Error("32-bit counters must not be polled over v2c")
	}
}

func last(v []string) string { return v[len(v)-1] }

// TestRealAgent runs against a real snmpd when ZIROX_TEST_SNMP=host:port is set
// (community "testcomm", v3 user ziroxv3 SHA/AES authpass123/privpass123).
func TestRealAgent(t *testing.T) {
	addr := os.Getenv("ZIROX_TEST_SNMP")
	if addr == "" {
		t.Skip("ZIROX_TEST_SNMP not set")
	}
	creds := map[string]store.Credential{
		"v2c": {Version: "2c", Community: "testcomm"},
		"v1":  {Version: "1", Community: "testcomm"},
		"v3":  {Version: "3", V3User: "ziroxv3", V3Level: "authPriv", V3AuthProto: "SHA", V3AuthPass: "authpass123", V3PrivProto: "AES", V3PrivPass: "privpass123"},
	}
	for name, c := range creds {
		t.Run(name, func(t *testing.T) {
			s, err := snmpx.Dial(snmpx.Target{Address: "127.0.0.1", Port: port(addr), Cred: c, Timeout: 2 * time.Second, Retries: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			info, found, err := snmpx.Discover(s)
			if err != nil {
				t.Fatal(err)
			}
			objs := snmpx.Merge(nil, found, true)
			for i := range objs {
				objs[i].Selected = true
			}
			samples, err := snmpx.Poll(s, objs)
			if err != nil {
				t.Fatal(err)
			}
			kinds := map[string]int{}
			for _, o := range objs {
				kinds[o.Kind]++
			}
			names := map[string]int{}
			for _, sm := range samples {
				names[sm.Name]++
			}
			t.Logf("%s: sysName=%s objects=%v metrics=%v", name, info.SysName, kinds, names)
			if kinds[snmpx.KindIf] == 0 || len(samples) == 0 {
				t.Fatal("expected interfaces and samples")
			}
			octets := "ifHCInOctets"
			if c.Version == "1" {
				octets = "ifInOctets"
			}
			if names[octets] == 0 {
				t.Errorf("no %s samples", octets)
			}
		})
	}
}

func port(addr string) int {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			n := 0
			for _, c := range addr[i+1:] {
				n = n*10 + int(c-'0')
			}
			return n
		}
	}
	return 161
}
