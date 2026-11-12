package snmpx_test

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"

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
	// sysUpTime + 3 ports x (11 metrics (v2c) + ifName + ifAlias) + 2 CPU
	// + 2 temp = 44 OIDs in two requests of at most 40.
	if oids != 44 || gets != 2 {
		t.Errorf("gets=%d oids=%d, want 2 calls with 44 OIDs", gets, oids)
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

func TestFirewallAndBGP(t *testing.T) {
	a := snmptest.Switch(1)
	a.Set("1.3.6.1.4.1.9.9.147.1.2.2.2.1.5.40.6", 1234)
	a.Set("1.3.6.1.4.1.9.9.147.1.2.2.2.1.5.40.7", 5000)
	a.Set("1.3.6.1.4.1.9.9.392.1.3.1.0", 17)
	a.Set("1.3.6.1.4.1.9.9.392.1.3.3.0", 15)
	a.Set("1.3.6.1.4.1.9.9.392.1.3.29.0", 2)
	a.Set("1.3.6.1.2.1.15.3.1.9.10.0.0.2", 65002)
	a.Set("1.3.6.1.2.1.15.3.1.2.10.0.0.2", 6)
	a.Set("1.3.6.1.2.1.15.3.1.3.10.0.0.2", 2)
	a.Set("1.3.6.1.2.1.15.3.1.16.10.0.0.2", 3600)
	a.Set("1.3.6.1.2.1.15.3.1.10.10.0.0.2", uint(42))
	_, objs := discover(t, a)
	for _, k := range []struct{ kind, key string }{
		{snmpx.KindFwConn, "connections"}, {snmpx.KindRAVPN, "VPN sessions"}, {snmpx.KindBGPPeer, "10.0.0.2"},
	} {
		if find(objs, k.kind, k.key) == nil {
			t.Fatalf("%s %q not discovered", k.kind, k.key)
		}
	}
	objs = snmpx.Merge(nil, objs, true)
	s, _ := a.Dial()
	samples, err := snmpx.Poll(s, objs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, x := range samples {
		k := x.Name
		for i := range x.Keys {
			k += " " + x.Keys[i] + "=" + x.Values[i]
		}
		got[k] = x.Value
	}
	want := map[string]float64{
		"zirox_firewall_connections type=current":                             1234,
		"zirox_firewall_connections type=max":                                 5000,
		"zirox_vpn_sessions type=all":                                         17,
		"zirox_vpn_sessions type=l2l":                                         2,
		"zirox_vpn_users":                                                     15,
		"zirox_bgp_peer_state peer=10.0.0.2 remote_as=65002":                  6,
		"zirox_bgp_peer_updates_received_total peer=10.0.0.2 remote_as=65002": 42,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (got %v)", k, got[k], v, got)
		}
	}
	if _, ok := got["zirox_vpn_sessions type=anyconnect"]; ok {
		t.Error("a missing OID must not produce a sample")
	}
}

func selectPorts(objs []store.Object, keys ...string) {
	for i := range objs {
		objs[i].ID = int64(i + 1)
		objs[i].Selected = false
		for _, k := range keys {
			if objs[i].Key == k {
				objs[i].Selected = true
			}
		}
	}
}

func TestPollChecksUptimeAndIfName(t *testing.T) {
	a := snmptest.Switch(8)
	_, found := discover(t, a)
	objs := snmpx.Merge(nil, found, false)
	selectPorts(objs, "Ethernet1/3", "Ethernet1/5")
	s, _ := a.Dial()

	r, err := snmpx.PollDevice(s, objs)
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasUptime || r.Uptime != 24*time.Hour {
		t.Errorf("uptime = %v (%v), want 24h", r.Uptime, r.HasUptime)
	}
	if len(r.Shifted) != 0 || r.Partial() {
		t.Errorf("clean poll reported shifted=%v partial=%v", r.Shifted, r.Partial())
	}
	info := 0
	for _, sm := range r.Samples {
		if sm.Name == snmpx.IfInfoMetric {
			info++
		}
	}
	if info != 2 {
		t.Errorf("want zirox_if_info for 2 ports, got %d", info)
	}

	// The description changes on the device: counters keep the discovered
	// ifAlias label, zirox_if_info shows the new one.
	a.Set("1.3.6.1.2.1.31.1.1.1.18.3", "UPLINK_CORE2")
	r, _ = snmpx.PollDevice(s, objs)
	for _, sm := range r.Samples {
		if sm.Values[1] != "Ethernet1/3" {
			continue
		}
		alias := sm.Values[3]
		if sm.Name == snmpx.IfInfoMetric && alias != "UPLINK_CORE2" {
			t.Errorf("if_info alias = %q", alias)
		}
		if sm.Name == "ifHCInOctets" && alias != "to Ethernet1/3" {
			t.Errorf("counter alias label changed to %q", alias)
		}
	}

	// After a reload the device renumbers: index 5 is now another port.
	a.Set("1.3.6.1.2.1.31.1.1.1.1.5", "Ethernet1/7")
	r, _ = snmpx.PollDevice(s, objs)
	if len(r.Shifted) != 1 || r.Shifted[0] != "Ethernet1/5" {
		t.Fatalf("shifted = %v, want [Ethernet1/5]", r.Shifted)
	}
	for _, sm := range r.Samples {
		if sm.Values[1] == "Ethernet1/5" {
			t.Errorf("value of a shifted port exported: %s", sm.Name)
		}
	}
	// A port that vanished from its index is shifted too.
	a.Delete("1.3.6.1.2.1.31.1.1.1.1.3")
	r, _ = snmpx.PollDevice(s, objs)
	if len(r.Shifted) != 2 {
		t.Errorf("shifted = %v, want both ports", r.Shifted)
	}
}

func TestPollPartial(t *testing.T) {
	a := snmptest.Switch(12)
	_, found := discover(t, a)
	objs := snmpx.Merge(nil, found, false)
	selectPorts(objs, "Ethernet1/1", "Ethernet1/3", "Ethernet1/5", "Ethernet1/7", "Ethernet1/9")
	s, _ := a.Dial()
	// Lose every request that carries Ethernet1/9's counters.
	a.FailGet = func(oids []string) bool {
		for _, o := range oids {
			if o == "1.3.6.1.2.1.31.1.1.1.6.9" {
				return true
			}
		}
		return false
	}
	r, err := snmpx.PollDevice(s, objs)
	if err != nil {
		t.Fatalf("partial poll must not fail: %v", err)
	}
	if !r.Partial() || r.Failed != 1 || r.Requests != 2 {
		t.Fatalf("partial=%v failed=%d requests=%d", r.Partial(), r.Failed, r.Requests)
	}
	got := map[string]bool{}
	for _, sm := range r.Samples {
		if sm.Name == "ifHCInOctets" {
			got[sm.Values[1]] = true
		}
	}
	// Objects are packed in discovery order: three ports share a request.
	if len(got) < 2 || got["Ethernet1/9"] {
		t.Errorf("ports with values: %v", got)
	}
	if len(r.LostObjects) == 0 {
		t.Error("lost objects not reported")
	}
	// Everything lost: the poll fails.
	a.FailGet = func([]string) bool { return true }
	if _, err := snmpx.PollDevice(s, objs); err == nil {
		t.Error("poll with every request lost must fail")
	}
}

func TestExplain(t *testing.T) {
	cases := []struct {
		err     error
		problem string
	}{
		{errors.New("request timeout (after 1 retries)"), snmpx.ProblemTimeout},
		{&snmpx.StatusError{Status: gosnmp.AuthorizationError}, snmpx.ProblemAuthz},
		{fmt.Errorf("walk ifDescr: %w", &snmpx.StatusError{Status: gosnmp.AuthorizationError}), snmpx.ProblemAuthz},
		{gosnmp.ErrUnknownUsername, snmpx.ProblemV3User},
		{gosnmp.ErrWrongDigest, snmpx.ProblemV3Auth},
		{errors.New("read udp 10.0.0.1:5000->10.0.0.2:161: connect: no route to host"), snmpx.ProblemNoRoute},
		{errors.New("read: connection refused"), snmpx.ProblemRefused},
		{snmpx.ErrNoData, snmpx.ProblemNoData},
	}
	for _, c := range cases {
		if got := snmpx.Explain(c.err, "2c", "10.210.136.17"); got.Problem != c.problem {
			t.Errorf("%v: problem %s, want %s", c.err, got.Problem, c.problem)
		}
	}
	if ex := snmpx.Explain(errors.New("request timeout"), "2c", "10.210.136.17"); !strings.Contains(ex.Hint, "10.210.136.17") {
		t.Errorf("timeout hint must name the source address: %s", ex.Hint)
	}
	if snmpx.CommunityWarning("ro@es0") == "" || snmpx.CommunityWarning("public") != "" {
		t.Error("community warning")
	}
}

func TestIdentify(t *testing.T) {
	a := snmptest.Switch(2)
	// A stack of two C9300 chassis.
	a.Set("1.3.6.1.2.1.47.1.1.1.1.5.1000", 3)
	a.Set("1.3.6.1.2.1.47.1.1.1.1.5.2000", 3)
	a.Set("1.3.6.1.2.1.47.1.1.1.1.5.1001", 9)
	a.Set("1.3.6.1.2.1.47.1.1.1.1.13.1000", "C9300-48P")
	a.Set("1.3.6.1.2.1.47.1.1.1.1.10.1000", "17.09.04a")
	a.Set("1.3.6.1.2.1.47.1.1.1.1.11.1000", "FOC1234X0AB")
	info, _ := discover(t, a)
	if info.Model != "C9300-48P ×2" || info.Version != "17.09.04a" || info.Serial != "FOC1234X0AB" || info.Vendor != "Cisco" {
		t.Errorf("identity = %+v", info.Identity)
	}

	cases := []struct{ descr, oid, model, version string }{
		{"Cisco NX-OS(tm) n9000, Software (n9000-dk9), Version 9.3(8), RELEASE SOFTWARE", ".1.3.6.1.4.1.9.12.3.1.3.1812", "Cisco Nexus 9000", "9.3(8)"},
		{"Cisco Adaptive Security Appliance Version 9.16(4)", "1.3.6.1.4.1.9.1.2313", "Cisco ASA", "9.16(4)"},
		{"Linux idrac 4.9.20 #1", "1.3.6.1.4.1.674.10892.5", "Linux", "4.9.20"},
		{"Cisco IOS XR Software (NCS-540), Version 7.5.2", "1.3.6.1.4.1.9.1.2984", "Cisco IOS XR", "7.5.2"},
		{"something", "1.3.6.1.4.1.99999.1", "enterprise 99999", ""},
	}
	for _, c := range cases {
		id := snmpx.IdentifyFromSystem(c.descr, c.oid)
		if id.Model != c.model || id.Version != c.version {
			t.Errorf("%q: model %q version %q, want %q %q", c.descr, id.Model, id.Version, c.model, c.version)
		}
	}
}

func TestDiagnose(t *testing.T) {
	a := snmptest.Switch(4)
	tg := snmpx.Target{Address: "127.0.0.1", Cred: store.Credential{Version: "2c", Community: "public"}, Timeout: time.Second}
	dialOK := func(snmpx.Target) (snmpx.Session, error) { return a.Dial() }
	timeout := func(snmpx.Target) (snmpx.Session, error) { return nil, errors.New("request timeout (after 1 retries)") }
	answers := func(string, int, time.Duration) bool { return true }
	silent := func(string, int, time.Duration) bool { return false }
	status := func(d snmpx.Diagnosis) string {
		var b strings.Builder
		for _, s := range d.Steps {
			b.WriteString(s.Status[:1])
		}
		return b.String()
	}

	d := snmpx.Diagnose(tg, dialOK, silent)
	if !d.OK || status(d) != "ooooo" || d.Source == "" || d.Info.SysName != "core-nx-01" || d.Info.Model != "Cisco Nexus 9000" {
		t.Fatalf("healthy device: %s %+v", status(d), d)
	}
	d = snmpx.Diagnose(tg, timeout, answers)
	if d.OK || status(d) != "ooofs" || d.Problem != snmpx.ProblemCredential {
		t.Errorf("wrong community: %s %s", status(d), d.Problem)
	}
	d = snmpx.Diagnose(tg, timeout, silent)
	if status(d) != "oofss" || d.Problem != snmpx.ProblemTimeout || len(d.Steps[2].Snippets) == 0 ||
		!strings.Contains(d.Steps[2].Snippets[0].Text, "permit host "+d.Source) {
		t.Errorf("no answer: %s %+v", status(d), d.Steps[2])
	}
	authz := func(snmpx.Target) (snmpx.Session, error) {
		return nil, &snmpx.StatusError{Status: gosnmp.AuthorizationError}
	}
	tg.Cred.Community = "ro@es0"
	d = snmpx.Diagnose(tg, authz, silent)
	if status(d) != "ooofs" || d.Problem != snmpx.ProblemAuthz || !strings.Contains(d.Steps[3].Hint, "'@'") {
		t.Errorf("authorizationError: %s %+v", status(d), d.Steps[3])
	}
	a.Delete("1.3.6.1.2.1.2.2.1.2")
	d = snmpx.Diagnose(tg, dialOK, silent)
	if d.OK || status(d) != "oooof" || d.Failed != "SNMP view" {
		t.Errorf("hidden ifTable: %s %s", status(d), d.Failed)
	}
	tg.Address = "no-such-host.invalid"
	if d = snmpx.Diagnose(tg, dialOK, silent); status(d) != "fssss" || d.Problem != snmpx.ProblemDNS {
		t.Errorf("bad name: %s", status(d))
	}
}
