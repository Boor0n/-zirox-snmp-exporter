package snmpx

import (
	"net"
	"sort"
	"strings"

	"github.com/gosnmp/gosnmp"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// CISCO-CDP-MIB cdpCacheTable, indexed by ifIndex.deviceIndex.
const (
	oidCdpDeviceID   = "1.3.6.1.4.1.9.9.23.1.2.1.1.6"
	oidCdpDevicePort = "1.3.6.1.4.1.9.9.23.1.2.1.1.7"
)

// LLDP-MIB: lldpRemTable is indexed by timeMark.localPortNum.remIndex;
// lldpLocPortTable maps localPortNum to the local port's name.
const (
	oidLldpRemPortID   = "1.0.8802.1.1.2.1.4.1.1.7"
	oidLldpRemPortDesc = "1.0.8802.1.1.2.1.4.1.1.8"
	oidLldpRemSysName  = "1.0.8802.1.1.2.1.4.1.1.9"
	oidLldpLocPortID   = "1.0.8802.1.1.2.1.3.7.1.3"
	oidLldpLocPortDesc = "1.0.8802.1.1.2.1.3.7.1.4"
)

type neighbor struct{ name, port, proto string }

// discoverNeighbors fills Neighbor* of the ports from CDP, then LLDP for
// ports CDP does not cover. A port with several neighbours (a hub, a
// hypervisor with many VMs) gets the first by name.
func discoverNeighbors(s Session, ifs []store.Object) {
	byIdx := map[string]*store.Object{}
	byName := map[string]*store.Object{}
	for i := range ifs {
		o := &ifs[i]
		byIdx[o.Idx] = o
		for _, n := range []string{o.Name, o.Descr} {
			if n != "" {
				byName[strings.ToLower(n)] = o
			}
		}
	}
	found := map[*store.Object][]neighbor{}

	ports, _ := walkColumn(s, oidCdpDevicePort)
	ids, _ := walkColumn(s, oidCdpDeviceID)
	for idx, p := range ids {
		o := byIdx[strings.SplitN(idx, ".", 2)[0]]
		name := strings.TrimSpace(asString(p))
		if o == nil || name == "" {
			continue
		}
		port := ""
		if pp, ok := ports[idx]; ok {
			port = asString(pp)
		}
		found[o] = append(found[o], neighbor{name, port, "cdp"})
	}

	names, _ := walkColumn(s, oidLldpRemSysName)
	if len(names) > 0 {
		remPort, _ := walkColumn(s, oidLldpRemPortID)
		remDesc, _ := walkColumn(s, oidLldpRemPortDesc)
		locID, _ := walkColumn(s, oidLldpLocPortID)
		locDesc, _ := walkColumn(s, oidLldpLocPortDesc)
		locCols := []map[string]string{pduStrings(locDesc), pduStrings(locID)}
		local := func(num string) *store.Object {
			for _, col := range locCols {
				if o := byName[strings.ToLower(col[num])]; o != nil {
					return o
				}
			}
			// NX-OS and many others number LLDP ports by ifIndex.
			return byIdx[num]
		}
		for idx, p := range names {
			parts := strings.Split(idx, ".")
			if len(parts) != 3 {
				continue
			}
			o := local(parts[1])
			name := strings.TrimSpace(asString(p))
			if o == nil || name == "" {
				continue
			}
			if covered(found[o], "cdp") {
				continue
			}
			port := ""
			if d, ok := remDesc[idx]; ok {
				port = asString(d)
			}
			if pid, ok := remPort[idx]; ok && (port == "" || isMAC(port)) {
				port = portID(pid.Value)
			}
			found[o] = append(found[o], neighbor{name, port, "lldp"})
		}
	}

	for o, ns := range found {
		sort.Slice(ns, func(i, j int) bool { return ns[i].name < ns[j].name })
		o.Neighbor, o.NeighborPort, o.NeighborProto = ns[0].name, ns[0].port, ns[0].proto
	}
}

func covered(ns []neighbor, proto string) bool {
	for _, n := range ns {
		if n.proto == proto {
			return true
		}
	}
	return false
}

func pduStrings(m map[string]gosnmp.SnmpPDU) map[string]string {
	out := make(map[string]string, len(m))
	for k, p := range m {
		out[k] = asString(p)
	}
	return out
}

// portID prints an LLDP port ID: text as is, a 6-byte MAC as aa:bb:...
func portID(v any) string {
	b, ok := v.([]byte)
	if !ok {
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
	if len(b) == 6 && !printable(b) {
		return net.HardwareAddr(b).String()
	}
	return strings.TrimRight(string(b), "\x00")
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

func isMAC(s string) bool {
	_, err := net.ParseMAC(s)
	return err == nil || !printable([]byte(s))
}
