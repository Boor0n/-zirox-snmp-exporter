package snmpx

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ENTITY-MIB columns used to name the hardware.
const (
	oidEntPhysClass    = "1.3.6.1.2.1.47.1.1.1.1.5"
	oidEntPhysSoftRev  = "1.3.6.1.2.1.47.1.1.1.1.10"
	oidEntPhysSerial   = "1.3.6.1.2.1.47.1.1.1.1.11"
	oidEntPhysModel    = "1.3.6.1.2.1.47.1.1.1.1.13"
	entClassChassis    = 3
	enterprisePrefix   = "1.3.6.1.4.1."
	ciscoNXOSDescrPart = "NX-OS"
)

// Identity is what a device is: model, software version and serial number.
type Identity struct {
	Model   string
	Version string
	Serial  string
	Vendor  string
}

// identify reads the chassis entries of ENTITY-MIB. Stacks report one
// chassis per member; the model is then "C9300-48P ×3".
func identify(s Session, info SystemInfo) Identity {
	var id Identity
	classes, _ := walkColumn(s, oidEntPhysClass)
	var chassis []int
	for idx, p := range classes {
		if int(asFloat(p)) == entClassChassis {
			if n, err := strconv.Atoi(idx); err == nil {
				chassis = append(chassis, n)
			}
		}
	}
	sort.Ints(chassis)
	if len(chassis) > 0 {
		var oids []string
		first := strconv.Itoa(chassis[0])
		for _, col := range []string{oidEntPhysModel, oidEntPhysSoftRev, oidEntPhysSerial} {
			oids = append(oids, col+"."+first)
		}
		if pdus, err := s.Get(oids); err == nil {
			for _, p := range pdus {
				v := strings.TrimSpace(asString(p))
				switch trimOID(p.Name) {
				case oids[0]:
					id.Model = v
				case oids[1]:
					id.Version = v
				case oids[2]:
					id.Serial = v
				}
			}
		}
		if id.Model != "" && len(chassis) > 1 {
			id.Model += " ×" + strconv.Itoa(len(chassis))
		}
	}
	fb := IdentifyFromSystem(info.SysDescr, info.SysObjectID)
	if id.Model == "" {
		id.Model = fb.Model
	}
	if id.Version == "" {
		id.Version = fb.Version
	}
	id.Vendor = fb.Vendor
	return id
}

var (
	reVersion = regexp.MustCompile(`(?i)\bversion\s+([0-9][0-9A-Za-z.()_:-]*[0-9A-Za-z)])`)
	reKernel  = regexp.MustCompile(`^Linux \S+ (\S+)`)
	reWindows = regexp.MustCompile(`Windows Version ([0-9.]+)`)
)

// vendors by IANA enterprise number (the 7th arc of sysObjectID).
var vendors = map[string]string{
	"9": "Cisco", "674": "Dell", "232": "HPE", "11": "HP", "8072": "Net-SNMP",
	"2636": "Juniper", "12356": "Fortinet", "25461": "Palo Alto", "2620": "Check Point",
	"311": "Microsoft", "6876": "VMware", "2011": "Huawei", "14988": "MikroTik",
	"6027": "Dell EMC", "1588": "Brocade", "3375": "F5", "30065": "Arista",
	"12325": "pfSense", "4526": "Netgear", "171": "D-Link", "41112": "Ubiquiti",
}

// descrModels are model names recognisable in sysDescr when the device has
// no ENTITY-MIB. First match wins.
var descrModels = []struct{ needle, model string }{
	{"Integrated Dell Remote Access Controller", "Dell iDRAC"},
	{"iDRAC", "Dell iDRAC"},
	{"Integrated Lights-Out", "HPE iLO"},
	{"iLO", "HPE iLO"},
	{"ProLiant", "HPE ProLiant"},
	{"Adaptive Security Appliance", "Cisco ASA"},
	{"Firepower Threat Defense", "Cisco Firepower"},
	{"IOS XR", "Cisco IOS XR"},
	{"IOS-XE", "Cisco IOS XE"},
	{"n9000", "Cisco Nexus 9000"},
	{"n7000", "Cisco Nexus 7000"},
	{"n5000", "Cisco Nexus 5000"},
	{"n3000", "Cisco Nexus 3000"},
	{ciscoNXOSDescrPart, "Cisco Nexus"},
	{"FortiGate", "FortiGate"},
	{"Palo Alto", "Palo Alto"},
	{"VMware ESXi", "VMware ESXi"},
	{"Windows", "Windows"},
	{"Linux", "Linux"},
}

// IdentifyFromSystem guesses vendor, model family and software version from
// sysDescr and sysObjectID. It is the fallback for devices without ENTITY-MIB.
func IdentifyFromSystem(descr, objectID string) Identity {
	var id Identity
	oid := trimOID(objectID)
	if strings.HasPrefix(oid, enterprisePrefix) {
		ent := strings.SplitN(strings.TrimPrefix(oid, enterprisePrefix), ".", 2)[0]
		id.Vendor = vendors[ent]
		if id.Vendor == "" {
			id.Vendor = "enterprise " + ent
		}
	}
	for _, m := range descrModels {
		if strings.Contains(descr, m.needle) {
			id.Model = m.model
			break
		}
	}
	if m := reVersion.FindStringSubmatch(descr); m != nil {
		id.Version = strings.TrimSuffix(m[1], ",")
	} else if m := reWindows.FindStringSubmatch(descr); m != nil {
		id.Version = m[1]
	} else if m := reKernel.FindStringSubmatch(descr); m != nil {
		id.Version = m[1]
	}
	if id.Model == "" {
		id.Model = id.Vendor
	}
	return id
}
