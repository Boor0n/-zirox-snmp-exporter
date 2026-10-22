package snmpx_test

import (
	"os"
	"testing"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx/snmptest"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

func TestCustomOIDs(t *testing.T) {
	a := snmptest.Switch(2)
	// A vendor table with names in another column, and a scalar.
	a.Set("1.3.6.1.4.1.99999.1.1.2.7", "pool-a")
	a.Set("1.3.6.1.4.1.99999.1.1.3.7", 42)
	a.Set("1.3.6.1.4.1.99999.1.1.2.9", "pool-b")
	a.Set("1.3.6.1.4.1.99999.1.1.3.9", 43)
	a.Set("1.3.6.1.4.1.99999.2.0", uint(1000))

	table := store.CustomOID{ID: 1, Name: "vendor_pool_used", OID: ".1.3.6.1.4.1.99999.1.1.3", Mode: store.CustomTable, NameOID: "1.3.6.1.4.1.99999.1.1.2", Scale: 1024, AutoSelect: true}
	scalar := store.CustomOID{ID: 2, Name: "vendor_hits_total", Help: "Hits", OID: "1.3.6.1.4.1.99999.2.0", Mode: store.CustomScalar, Counter: true, Scale: 1}
	missing := store.CustomOID{ID: 3, Name: "vendor_absent", OID: "1.3.6.1.4.1.99999.3", Mode: store.CustomTable, Scale: 1, AutoSelect: true}
	for _, c := range []*store.CustomOID{&table, &scalar, &missing} {
		if err := snmpx.CheckCustom(c); err != nil {
			t.Fatal(err)
		}
	}
	snmpx.SetCustom([]store.CustomOID{table, scalar, missing})
	defer snmpx.SetCustom(nil)

	_, found := discover(t, a)
	objs := snmpx.Merge(nil, found, false)
	pa := find(objs, table.Kind(), "pool-a")
	hits := find(objs, scalar.Kind(), "vendor_hits_total")
	if pa == nil || pa.Idx != "7" || !pa.Selected || find(objs, table.Kind(), "pool-b") == nil {
		t.Fatalf("table objects wrong: %+v", pa)
	}
	if hits == nil || hits.Idx != "0" || hits.Selected || hits.Name != "Hits" {
		t.Fatalf("scalar object wrong: %+v", hits)
	}
	for _, o := range objs {
		if o.Kind == missing.Kind() {
			t.Fatalf("an OID the device lacks must add nothing, got %+v", o)
		}
	}
	if snmpx.CategoryOf(table.Kind()) != snmpx.CategoryCustom {
		t.Fatal("custom objects belong to the Custom section")
	}

	hits.Selected = true
	s, _ := a.Dial()
	samples, err := snmpx.Poll(s, objs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]snmpx.Sample{}
	for _, sm := range samples {
		if len(sm.Values) > 1 {
			got[sm.Name+"/"+sm.Values[1]] = sm
		} else {
			got[sm.Name] = sm
		}
	}
	if v := got["vendor_pool_used/pool-b"]; v.Value != 43*1024 || v.Counter || len(v.Keys) != 2 {
		t.Errorf("pool-b sample = %+v", v)
	}
	if v := got["vendor_hits_total"]; v.Value != 1000 || !v.Counter || len(v.Keys) != 0 {
		t.Errorf("scalar sample = %+v", v)
	}

	// Deleted definitions are no longer polled.
	snmpx.SetCustom(nil)
	samples, _ = snmpx.Poll(s, objs)
	for _, sm := range samples {
		if sm.Name == "vendor_pool_used" {
			t.Fatal("deleted custom OID still polled")
		}
	}
}

func TestCheckCustom(t *testing.T) {
	bad := []store.CustomOID{
		{Name: "", OID: "1.3.6.1", Mode: store.CustomTable},
		{Name: "9bad", OID: "1.3.6.1", Mode: store.CustomTable},
		{Name: "ifHCInOctets", OID: "1.3.6.1", Mode: store.CustomTable},
		{Name: "zirox_poll_success", OID: "1.3.6.1", Mode: store.CustomTable},
		{Name: "ok_name", OID: "iso.3.6", Mode: store.CustomTable},
		{Name: "ok_name", OID: "1.3.6.1", Mode: "other"},
		{Name: "ok_name", OID: "1.3.6.1", Mode: store.CustomTable, NameOID: "abc"},
	}
	for _, c := range bad {
		if err := snmpx.CheckCustom(&c); err == nil {
			t.Errorf("%+v must be rejected", c)
		}
	}
	c := store.CustomOID{Name: " ok_name ", OID: " .1.3.6.1.2.1.1.3.0 ", Mode: store.CustomScalar, NameOID: "1.2"}
	if err := snmpx.CheckCustom(&c); err != nil || c.OID != "1.3.6.1.2.1.1.3.0" || c.Name != "ok_name" || c.NameOID != "" || c.Scale != 1 {
		t.Fatalf("normalized = %+v, %v", c, err)
	}
}

// TestCustomRealAgent reads sysUpTime and the UCD load table from a real snmpd.
func TestCustomRealAgent(t *testing.T) {
	addr := os.Getenv("ZIROX_TEST_SNMP")
	if addr == "" {
		t.Skip("ZIROX_TEST_SNMP not set")
	}
	defs := []store.CustomOID{
		{ID: 1, Name: "test_uptime_ticks", OID: "1.3.6.1.2.1.1.3.0", Mode: store.CustomScalar, Scale: 1, AutoSelect: true},
		{ID: 2, Name: "test_load_x100", OID: "1.3.6.1.4.1.2021.10.1.5", NameOID: "1.3.6.1.4.1.2021.10.1.2", Mode: store.CustomTable, Scale: 1, AutoSelect: true},
	}
	snmpx.SetCustom(defs)
	defer snmpx.SetCustom(nil)
	s, err := snmpx.Dial(snmpx.Target{Address: "127.0.0.1", Port: port(addr), Cred: store.Credential{Version: "2c", Community: "testcomm"}, Timeout: 2 * time.Second, Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, found, err := snmpx.Discover(s)
	if err != nil {
		t.Fatal(err)
	}
	objs := snmpx.Merge(nil, found, false)
	samples, err := snmpx.Poll(s, objs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, sm := range samples {
		got[sm.Name]++
		t.Logf("%s %v = %v", sm.Name, sm.Values, sm.Value)
	}
	if got["test_uptime_ticks"] != 1 || got["test_load_x100"] != 3 {
		t.Fatalf("samples = %v", got)
	}
	rows, _, err := snmpx.Lookup(s, "1.3.6.1.4.1.2021.10.1.2", true)
	if err != nil || len(rows) != 3 || rows[0].Numeric {
		t.Fatalf("lookup = %+v, %v", rows, err)
	}
}

func TestColumnOf(t *testing.T) {
	for _, c := range [][3]string{
		{"1.3.6.1.4.1.2021.10.1", "1.3.6.1.4.1.2021.10.1.5.2", "1.3.6.1.4.1.2021.10.1.5"},
		{"1.3.6.1.4.1.2021.10.1.5", "1.3.6.1.4.1.2021.10.1.5.2", "1.3.6.1.4.1.2021.10.1.5"},
		{"1.3.6.1.4.1.9.9.147", "1.3.6.1.4.1.9.9.147.1.2.2.2.1.5.40.6", "1.3.6.1.4.1.9.9.147.1"},
		{"1.3.6.1.2.1.1.3.0", "1.3.6.1.2.1.1.3.0", ""},
	} {
		if got := snmpx.ColumnOf(c[0], c[1]); got != c[2] {
			t.Errorf("columnOf(%s, %s) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
