package snmpx_test

import (
	"testing"

	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx/snmptest"
)

func TestNeighbors(t *testing.T) {
	a := snmptest.New()
	a.Set("1.3.6.1.2.1.1.1.0", "Cisco IOS Software")
	a.Set("1.3.6.1.2.1.1.5.0", "access-01")
	snmptest.AddPort(a, 10101, "Gi1/0/1", 6, true)
	snmptest.AddPort(a, 10102, "Gi1/0/2", 6, true)
	snmptest.AddPort(a, 10103, "Gi1/0/3", 6, true)
	snmptest.AddPort(a, 10104, "Gi1/0/4", 6, true)
	// CDP on Gi1/0/1: NX-OS device ID with serial and domain.
	a.Set("1.3.6.1.4.1.9.9.23.1.2.1.1.6.10101.1", "core-nx-01.dc.local(FOX1234ABCD)")
	a.Set("1.3.6.1.4.1.9.9.23.1.2.1.1.7.10101.1", "Ethernet1/49")
	// LLDP on Gi1/0/1 too (ignored: CDP covers it), and on Gi1/0/2 with
	// local port number 2 that only the local port table maps to the port.
	a.Set("1.0.8802.1.1.2.1.3.7.1.3.1", "Gi1/0/1")
	a.Set("1.0.8802.1.1.2.1.3.7.1.3.2", "Gi1/0/2")
	a.Set("1.0.8802.1.1.2.1.4.1.1.9.0.1.1", "core-nx-01")
	a.Set("1.0.8802.1.1.2.1.4.1.1.9.0.2.1", "esx-07")
	a.Set("1.0.8802.1.1.2.1.4.1.1.8.0.2.1", "vmnic0")
	// LLDP numbered by ifIndex (NX-OS style) with a MAC as port ID.
	a.Set("1.0.8802.1.1.2.1.4.1.1.9.0.10103.1", "fw-01")
	a.Set("1.0.8802.1.1.2.1.4.1.1.7.0.10103.1", []byte{0x00, 0x1b, 0x21, 0xaa, 0xbb, 0xcc})

	_, objs := discover(t, a)
	for _, c := range []struct{ port, name, rport, proto string }{
		{"Gi1/0/1", "core-nx-01.dc.local(FOX1234ABCD)", "Ethernet1/49", "cdp"},
		{"Gi1/0/2", "esx-07", "vmnic0", "lldp"},
		{"Gi1/0/3", "fw-01", "00:1b:21:aa:bb:cc", "lldp"},
		{"Gi1/0/4", "", "", ""},
	} {
		o := find(objs, snmpx.KindIf, c.port)
		if o == nil || o.Neighbor != c.name || o.NeighborPort != c.rport || o.NeighborProto != c.proto {
			t.Errorf("%s: %+v", c.port, o)
		}
	}
	got := pollAll(t, a, objs)
	if got["zirox_if_neighbor_info/10102/Gi1/0/2/esx-07/vmnic0/lldp"] != 1 {
		t.Errorf("neighbor info missing: %v", got)
	}
	for k := range got {
		if len(k) > 22 && k[:22] == "zirox_if_neighbor_info" && k[23:28] == "10104" {
			t.Errorf("port without neighbour exported: %s", k)
		}
	}
}
