package snmpx

import "github.com/Boor0n/zirox-snmp-exporter/internal/store"

// Object kinds. Each kind knows which OIDs to poll for one discovered object.
const (
	KindIf          = "if"
	KindCiscoCPU    = "cpu_cisco"
	KindNexusCPU    = "cpu_nexus"
	KindHostCPU     = "cpu_host"
	KindCiscoMem    = "mem_cisco"
	KindNexusMem    = "mem_nexus"
	KindUcdMem      = "mem_ucd"
	KindHostStorage = "storage_host"
	KindEntTemp     = "temp_entity"
	KindEntSensor   = "sensor_entity"
	KindEnvTemp     = "temp_envmon"
	KindEnvFan      = "fan_envmon"
	KindEnvPSU      = "psu_envmon"
	KindFwConn      = "conn_firewall"
	KindRAVPN       = "vpn_cisco"
	KindBGPPeer     = "bgp_peer"
)

// Metric is one value polled per object: OID base + "." + object index.
type Metric struct {
	Name    string
	Help    string
	OID     string
	Suffix  string // appended after the object index
	Counter bool
	Window  string  // value of the last label (CPU "window", firewall/VPN "type")
	Scale   float64 // constant multiplier (0 = 1)
	Factor  bool    // multiply by the object's discovered factor
	NotV1   bool    // 64-bit counters are not available over SNMPv1
	OnlyV1  bool
}

// Kind describes how a kind of object is shown and polled.
type Kind struct {
	Category  string // section in the UI
	LabelKeys []string
	Labels    func(o store.Object) []string
	Metrics   []Metric
}

func idxName(o store.Object) []string { return []string{o.Idx, o.Name} }

var cpuLabels = []string{"index", "name", "window"}

// Kinds maps an object kind to its spec.
var Kinds = map[string]Kind{
	KindIf: {
		Category:  "Ports",
		LabelKeys: []string{"ifIndex", "ifName", "ifDescr", "ifAlias"},
		Labels:    func(o store.Object) []string { return []string{o.Idx, o.Name, o.Descr, o.Alias} },
		Metrics: []Metric{
			{Name: "ifHCInOctets", Help: "Octets received (64-bit).", OID: "1.3.6.1.2.1.31.1.1.1.6", Counter: true, NotV1: true},
			{Name: "ifHCOutOctets", Help: "Octets sent (64-bit).", OID: "1.3.6.1.2.1.31.1.1.1.10", Counter: true, NotV1: true},
			{Name: "ifHCInUcastPkts", Help: "Unicast packets received (64-bit).", OID: "1.3.6.1.2.1.31.1.1.1.7", Counter: true, NotV1: true},
			{Name: "ifHCOutUcastPkts", Help: "Unicast packets sent (64-bit).", OID: "1.3.6.1.2.1.31.1.1.1.11", Counter: true, NotV1: true},
			{Name: "ifInOctets", Help: "Octets received (32-bit).", OID: "1.3.6.1.2.1.2.2.1.10", Counter: true, OnlyV1: true},
			{Name: "ifOutOctets", Help: "Octets sent (32-bit).", OID: "1.3.6.1.2.1.2.2.1.16", Counter: true, OnlyV1: true},
			{Name: "ifInErrors", Help: "Inbound packets with errors.", OID: "1.3.6.1.2.1.2.2.1.14", Counter: true},
			{Name: "ifOutErrors", Help: "Outbound packets with errors.", OID: "1.3.6.1.2.1.2.2.1.20", Counter: true},
			{Name: "ifInDiscards", Help: "Inbound packets discarded.", OID: "1.3.6.1.2.1.2.2.1.13", Counter: true},
			{Name: "ifOutDiscards", Help: "Outbound packets discarded.", OID: "1.3.6.1.2.1.2.2.1.19", Counter: true},
			{Name: "ifOperStatus", Help: "Operational status (1=up, 2=down, ...).", OID: "1.3.6.1.2.1.2.2.1.8"},
			{Name: "ifAdminStatus", Help: "Admin status (1=up, 2=down, 3=testing).", OID: "1.3.6.1.2.1.2.2.1.7"},
			{Name: "ifHighSpeed", Help: "Interface speed in Mbit/s.", OID: "1.3.6.1.2.1.31.1.1.1.15", NotV1: true},
			{Name: "ifSpeed", Help: "Interface speed in bit/s.", OID: "1.3.6.1.2.1.2.2.1.5", OnlyV1: true},
		},
	},
	KindCiscoCPU: {
		Category: "CPU", LabelKeys: cpuLabels,
		Labels: func(o store.Object) []string { return []string{o.Idx, o.Name} },
		Metrics: []Metric{
			{Name: "zirox_cpu_utilization_percent", Help: "CPU utilization in percent.", OID: "1.3.6.1.4.1.9.9.109.1.1.1.1.7", Window: "1m"},
			{Name: "zirox_cpu_utilization_percent", Help: "CPU utilization in percent.", OID: "1.3.6.1.4.1.9.9.109.1.1.1.1.8", Window: "5m"},
		},
	},
	KindNexusCPU: {
		Category: "CPU", LabelKeys: cpuLabels,
		Labels: func(o store.Object) []string { return []string{o.Idx, o.Name} },
		Metrics: []Metric{
			{Name: "zirox_cpu_utilization_percent", Help: "CPU utilization in percent.", OID: "1.3.6.1.4.1.9.9.305.1.1.1", Window: "current"},
		},
	},
	KindHostCPU: {
		Category: "CPU", LabelKeys: cpuLabels,
		Labels: func(o store.Object) []string { return []string{o.Idx, o.Name} },
		Metrics: []Metric{
			{Name: "zirox_cpu_utilization_percent", Help: "CPU utilization in percent.", OID: "1.3.6.1.2.1.25.3.3.1.2", Window: "1m"},
		},
	},
	KindCiscoMem: {
		Category: "Memory", LabelKeys: []string{"index", "name"}, Labels: idxName,
		Metrics: []Metric{
			{Name: "zirox_memory_used_bytes", Help: "Memory used in bytes.", OID: "1.3.6.1.4.1.9.9.48.1.1.1.5"},
			{Name: "zirox_memory_free_bytes", Help: "Memory free in bytes.", OID: "1.3.6.1.4.1.9.9.48.1.1.1.6"},
		},
	},
	KindNexusMem: {
		Category: "Memory", LabelKeys: []string{"index", "name"}, Labels: idxName,
		Metrics: []Metric{
			{Name: "zirox_memory_utilization_percent", Help: "Memory utilization in percent.", OID: "1.3.6.1.4.1.9.9.305.1.1.2"},
		},
	},
	KindUcdMem: {
		Category: "Memory", LabelKeys: []string{"index", "name"}, Labels: idxName,
		Metrics: []Metric{
			{Name: "zirox_memory_total_bytes", Help: "Total memory in bytes.", OID: "1.3.6.1.4.1.2021.4.5", Scale: 1024},
			{Name: "zirox_memory_available_bytes", Help: "Available memory in bytes.", OID: "1.3.6.1.4.1.2021.4.6", Scale: 1024},
			{Name: "zirox_memory_buffers_bytes", Help: "Memory used for buffers in bytes.", OID: "1.3.6.1.4.1.2021.4.14", Scale: 1024},
			{Name: "zirox_memory_cached_bytes", Help: "Memory used for cache in bytes.", OID: "1.3.6.1.4.1.2021.4.15", Scale: 1024},
		},
	},
	KindHostStorage: {
		Category: "Storage", LabelKeys: []string{"index", "name"}, Labels: idxName,
		Metrics: []Metric{
			{Name: "zirox_storage_size_bytes", Help: "Storage size in bytes.", OID: "1.3.6.1.2.1.25.2.3.1.5", Factor: true},
			{Name: "zirox_storage_used_bytes", Help: "Storage used in bytes.", OID: "1.3.6.1.2.1.25.2.3.1.6", Factor: true},
		},
	},
	KindEntTemp: {
		Category: "Sensors", LabelKeys: []string{"index", "name"}, Labels: idxName,
		Metrics: []Metric{
			{Name: "zirox_temperature_celsius", Help: "Temperature in degrees Celsius.", OID: "1.3.6.1.2.1.99.1.1.1.4", Factor: true},
			{Name: "zirox_sensor_status", Help: "ENTITY-SENSOR status (1=ok, 2=unavailable, 3=nonoperational).", OID: "1.3.6.1.2.1.99.1.1.1.5"},
		},
	},
	KindEntSensor: {
		Category: "Sensors", LabelKeys: []string{"index", "name", "unit"},
		Labels: func(o store.Object) []string { return []string{o.Idx, o.Name, o.Descr} },
		Metrics: []Metric{
			{Name: "zirox_sensor_value", Help: "ENTITY-SENSOR value in the unit given by the unit label.", OID: "1.3.6.1.2.1.99.1.1.1.4", Factor: true},
		},
	},
	KindEnvTemp: {
		Category: "Sensors", LabelKeys: []string{"index", "name"}, Labels: idxName,
		Metrics: []Metric{
			{Name: "zirox_temperature_celsius", Help: "Temperature in degrees Celsius.", OID: "1.3.6.1.4.1.9.9.13.1.3.1.3"},
			{Name: "zirox_envmon_temperature_state", Help: "CISCO-ENVMON state (1=normal, 2=warning, 3=critical, 4=shutdown, 5=notPresent, 6=notFunctioning).", OID: "1.3.6.1.4.1.9.9.13.1.3.1.6"},
		},
	},
	KindEnvFan: {
		Category: "Sensors", LabelKeys: []string{"index", "name"}, Labels: idxName,
		Metrics: []Metric{
			{Name: "zirox_envmon_fan_state", Help: "CISCO-ENVMON fan state (1=normal, 2=warning, 3=critical, 4=shutdown, 5=notPresent, 6=notFunctioning).", OID: "1.3.6.1.4.1.9.9.13.1.4.1.3"},
		},
	},
	KindEnvPSU: {
		Category: "Sensors", LabelKeys: []string{"index", "name"}, Labels: idxName,
		Metrics: []Metric{
			{Name: "zirox_envmon_psu_state", Help: "CISCO-ENVMON power supply state (1=normal, 2=warning, 3=critical, 4=shutdown, 5=notPresent, 6=notFunctioning).", OID: "1.3.6.1.4.1.9.9.13.1.5.1.3"},
		},
	},
	// CISCO-FIREWALL-MIB cfwConnectionStatValue, indexed by service (40 = IP)
	// and stat type (6 = currently in use, 7 = highest since boot).
	KindFwConn: {
		Category: "Firewall", LabelKeys: []string{"type"},
		Labels: func(store.Object) []string { return nil },
		Metrics: []Metric{
			{Name: "zirox_firewall_connections", Help: "Firewall connections (type=current or max since boot).", OID: "1.3.6.1.4.1.9.9.147.1.2.2.2.1.5", Suffix: ".6", Window: "current"},
			{Name: "zirox_firewall_connections", Help: "Firewall connections (type=current or max since boot).", OID: "1.3.6.1.4.1.9.9.147.1.2.2.2.1.5", Suffix: ".7", Window: "max"},
		},
	},
	// CISCO-REMOTE-ACCESS-MONITOR-MIB crasActivity scalars (ASA).
	KindRAVPN: {
		Category: "Firewall", LabelKeys: []string{"type"},
		Labels: func(store.Object) []string { return nil },
		Metrics: []Metric{
			{Name: "zirox_vpn_sessions", Help: "Active VPN sessions by type.", OID: "1.3.6.1.4.1.9.9.392.1.3.1", Window: "all"},
			{Name: "zirox_vpn_sessions", Help: "Active VPN sessions by type.", OID: "1.3.6.1.4.1.9.9.392.1.3.29", Window: "l2l"},
			{Name: "zirox_vpn_sessions", Help: "Active VPN sessions by type.", OID: "1.3.6.1.4.1.9.9.392.1.3.35", Window: "anyconnect"},
			{Name: "zirox_vpn_users", Help: "Active VPN users.", OID: "1.3.6.1.4.1.9.9.392.1.3.3"},
		},
	},
	// BGP4-MIB bgpPeerTable, indexed by the peer's IPv4 address.
	KindBGPPeer: {
		Category: "Routing", LabelKeys: []string{"peer", "remote_as"},
		Labels: func(o store.Object) []string { return []string{o.Idx, o.Descr} },
		Metrics: []Metric{
			{Name: "zirox_bgp_peer_state", Help: "BGP peer state (1=idle, 2=connect, 3=active, 4=opensent, 5=openconfirm, 6=established).", OID: "1.3.6.1.2.1.15.3.1.2"},
			{Name: "zirox_bgp_peer_admin_status", Help: "BGP peer admin status (1=stop, 2=start).", OID: "1.3.6.1.2.1.15.3.1.3"},
			{Name: "zirox_bgp_peer_established_seconds", Help: "Seconds the peer has been in (or was last in) the established state.", OID: "1.3.6.1.2.1.15.3.1.16"},
			{Name: "zirox_bgp_peer_updates_received_total", Help: "BGP UPDATE messages received from the peer.", OID: "1.3.6.1.2.1.15.3.1.10", Counter: true},
		},
	},
}

// Categories in display order.
var Categories = []string{"Ports", "CPU", "Memory", "Sensors", "Storage", "Firewall", "Routing"}

// CategoryOf returns the UI section of an object kind.
func CategoryOf(kind string) string {
	if k, ok := Kinds[kind]; ok {
		return k.Category
	}
	return "Other"
}
