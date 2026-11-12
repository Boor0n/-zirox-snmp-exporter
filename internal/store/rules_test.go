package store

import (
	"path/filepath"
	"testing"
)

func TestPortRules(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "z.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, bad := range []PortRule{
		{Pattern: ""},
		{Pattern: "(", Action: RulePoll},
		{Pattern: "x", Action: "maybe"},
		{Pattern: "x"}, // neither tick nor role
	} {
		if err := st.SavePortRule(&bad); err == nil {
			t.Errorf("rule %+v accepted", bad)
		}
	}
	up := PortRule{Field: FieldAlias, Pattern: "uplink|core", Action: RulePoll, Role: " Uplink "}
	vlan := PortRule{Field: FieldName, Pattern: "^vlan", Action: RuleSkip}
	srv := PortRule{Pattern: "SRV", Role: "server", Group: "DC1"}
	for _, r := range []*PortRule{&up, &vlan, &srv} {
		if err := st.SavePortRule(r); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := st.PortRules()
	if err != nil || len(rs) != 3 || rs[0].Role != "uplink" {
		t.Fatalf("rules = %+v, %v", rs, err)
	}

	cases := []struct {
		o        Object
		group    string
		sel      bool
		role     string
		matched  bool
		describe string
	}{
		{Object{Kind: "if", Name: "Eth1/49", Alias: "UPLINK to core"}, "", true, "uplink", true, "description match ticks and sets role"},
		{Object{Kind: "if", Name: "Vlan10", Alias: "uplink"}, "", true, "uplink", true, "first match wins"},
		{Object{Kind: "if", Name: "Vlan10"}, "", false, "", true, "name match unticks"},
		{Object{Kind: "if", Name: "Eth1/1", Alias: "srv-01"}, "DC1", true, "server", true, "keep default tick, set role"},
		{Object{Kind: "if", Name: "Eth1/1", Alias: "srv-01"}, "DC2", true, "", false, "group filter"},
		{Object{Kind: "cpu", Name: "uplink cpu"}, "", true, "", false, "only ports"},
	}
	for _, c := range cases {
		o := c.o
		m := rs.Apply(&o, c.group, true)
		if (m != nil) != c.matched || o.Selected != c.sel || o.Role != c.role {
			t.Errorf("%s: matched=%v selected=%v role=%q", c.describe, m != nil, o.Selected, o.Role)
		}
	}

	// Order: move the vlan rule to the top; now it wins for Vlan10 "uplink".
	if err := st.MovePortRule(vlan.ID, -1); err != nil {
		t.Fatal(err)
	}
	rs, _ = st.PortRules()
	if rs[0].ID != vlan.ID {
		t.Fatalf("move: %+v", rs)
	}
	o := Object{Kind: "if", Name: "Vlan10", Alias: "uplink"}
	rs.Apply(&o, "", true)
	if o.Selected || o.Role != "" {
		t.Errorf("after move Vlan10 = %+v", o)
	}

	// Apply to existing ports overrides matching ports only.
	c := Credential{Name: "c", Version: "2c", Community: "x"}
	if err := st.SaveCredential(&c); err != nil {
		t.Fatal(err)
	}
	d := Device{Name: "sw", Address: "10.0.0.1", Port: 161, Enabled: true, CredentialID: c.ID}
	if err := st.SaveDevice(&d); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyDiscovery(d, []Object{
		{Kind: "if", Key: "Eth1/49", Idx: "49", Name: "Eth1/49", Alias: "UPLINK"},
		{Kind: "if", Key: "Eth1/2", Idx: "2", Name: "Eth1/2", Selected: true, Role: "access"},
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := st.ApplyRulesAll(rs)
	if err != nil || changed[d.ID] != 1 {
		t.Fatalf("changed = %v, %v", changed, err)
	}
	objs, _ := st.Objects(d.ID)
	for _, o := range objs {
		switch o.Name {
		case "Eth1/49":
			if !o.Selected || o.Role != "uplink" {
				t.Errorf("Eth1/49 = %+v", o)
			}
		case "Eth1/2":
			if !o.Selected || o.Role != "access" {
				t.Errorf("unmatched port changed: %+v", o)
			}
		}
	}
	if err := st.SetObjectRoles(d.ID, map[int64]string{objs[0].ID: "Core!"}); err != nil {
		t.Fatal(err)
	}
	objs, _ = st.Objects(d.ID)
	if objs[0].Role != "core" {
		t.Errorf("role not normalised: %q", objs[0].Role)
	}
}
