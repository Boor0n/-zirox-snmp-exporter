package snmpx

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Step states of an access check.
const (
	StepOK   = "ok"
	StepFail = "fail"
	StepWarn = "warn"
	StepSkip = "skip"
)

// Snippet is ready configuration for one platform.
type Snippet struct {
	Platform string
	Text     string
}

// Step is one rung of the access ladder.
type Step struct {
	Name     string
	Status   string
	Detail   string
	Hint     string
	Snippets []Snippet
}

// Diagnosis is the result of an access check of one device.
type Diagnosis struct {
	Address string
	Source  string // address Zirox sends from
	Steps   []Step
	OK      bool   // credentials accepted and interfaces visible
	Failed  string // name of the first failed step
	Problem string // Problem* class of the failure
	Info    SystemInfo
	Took    time.Duration
}

// Summary is one line for tables and CSV exports.
func (d Diagnosis) Summary() string {
	for _, s := range d.Steps {
		if s.Status == StepFail {
			if s.Detail != "" {
				return s.Name + ": " + s.Detail
			}
			return s.Name
		}
	}
	for _, s := range d.Steps {
		if s.Status == StepWarn {
			return s.Name + ": " + s.Detail
		}
	}
	return "OK"
}

// Prober reports whether anything answers SNMP at address:port, whatever
// the credentials. The default sends an SNMPv3 discovery request.
type Prober func(address string, port int, timeout time.Duration) bool

// ProbeV3 sends an SNMPv3 engine discovery request with an unknown user.
// Agents with v3 enabled answer it even when the v2c community is wrong.
func ProbeV3(address string, port int, timeout time.Duration) bool {
	if port == 0 {
		port = 161
	}
	g := &gosnmp.GoSNMP{
		Target: address, Port: uint16(port), Transport: "udp", Timeout: timeout, Retries: 0,
		Version: gosnmp.Version3, SecurityModel: gosnmp.UserSecurityModel, MsgFlags: gosnmp.NoAuthNoPriv,
		SecurityParameters: &gosnmp.UsmSecurityParameters{UserName: "zirox-probe"},
	}
	if err := g.Connect(); err != nil {
		return false
	}
	defer g.Conn.Close()
	_, err := g.Get([]string{oidSysDescr})
	if err == nil {
		return true
	}
	low := strings.ToLower(err.Error())
	return !strings.Contains(low, "timeout") && !strings.Contains(low, "max retries") &&
		!strings.Contains(low, "refused") && !strings.Contains(low, "unreachable") && !strings.Contains(low, "no route")
}

// inDocker reports whether Zirox runs inside a container.
func inDocker() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// Diagnose climbs the access ladder for one device: name, route, SNMP
// answer, credentials, view. It stops at the first rung that fails.
func Diagnose(t Target, dial Dialer, probe Prober) Diagnosis {
	start := time.Now()
	d := Diagnosis{Address: t.Address}
	defer func() { d.Took = time.Since(start) }()
	fail := func(problem string) {
		if d.Failed == "" {
			d.Failed = d.Steps[len(d.Steps)-1].Name
			d.Problem = problem
		}
	}
	skip := func(names ...string) {
		for _, n := range names {
			d.Steps = append(d.Steps, Step{Name: n, Status: StepSkip})
		}
	}

	// 1. Name.
	ip := t.Address
	if net.ParseIP(t.Address) != nil {
		d.Steps = append(d.Steps, Step{Name: "Address", Status: StepOK, Detail: t.Address})
	} else if addrs, err := net.LookupHost(t.Address); err != nil || len(addrs) == 0 {
		d.Steps = append(d.Steps, Step{Name: "Address", Status: StepFail, Detail: "the name does not resolve",
			Hint: "Use the IP address or add the name to DNS / /etc/hosts on the Zirox server."})
		fail(ProblemDNS)
		skip("Route", "SNMP answer", "Credentials", "SNMP view")
		return d
	} else {
		ip = addrs[0]
		d.Steps = append(d.Steps, Step{Name: "Address", Status: StepOK, Detail: t.Address + " → " + ip})
	}

	// 2. Route.
	d.Source = SourceAddress(ip)
	if d.Source == "" {
		d.Steps = append(d.Steps, Step{Name: "Route", Status: StepFail, Detail: "no route from the Zirox server to " + ip,
			Hint: "Check the routing table and default gateway of the Zirox server."})
		fail(ProblemNoRoute)
		skip("SNMP answer", "Credentials", "SNMP view")
		return d
	}
	route := Step{Name: "Route", Status: StepOK, Detail: "Zirox sends from " + d.Source}
	if inDocker() && strings.HasPrefix(d.Source, "172.") {
		route.Status = StepWarn
		route.Hint = "Zirox runs in a Docker bridge network; devices see the host's address after NAT. If SNMP gets no answer while snmpget from the host works, reinstall with get.sh --host-network."
	}
	d.Steps = append(d.Steps, route)
	src := d.Source

	// 3 and 4. Answer and credentials, with the configured credentials.
	s, err := dial(t)
	var info SystemInfo
	if err == nil {
		var pdus []gosnmp.SnmpPDU
		pdus, err = s.Get([]string{oidSysDescr, oidSysObjectID, oidSysName})
		if err == nil && len(pdus) == 0 {
			err = ErrNoData
		}
		for _, p := range pdus {
			switch trimOID(p.Name) {
			case oidSysDescr:
				info.SysDescr = asString(p)
			case oidSysObjectID:
				info.SysObjectID = trimOID(asString(p))
			case oidSysName:
				info.SysName = asString(p)
			}
		}
	}
	if err != nil {
		ex := Explain(err, t.Cred.Version, src)
		switch ex.Problem {
		case ProblemTimeout:
			answers := t.Cred.Version != "3" && probe != nil && probe(ip, t.Port, max(t.Timeout, 2*time.Second))
			if answers {
				d.Steps = append(d.Steps, Step{Name: "SNMP answer", Status: StepOK, Detail: "the agent answers SNMPv3 discovery, so UDP 161 gets through"})
				d.Steps = append(d.Steps, Step{Name: "Credentials", Status: StepFail,
					Detail:   "the device ignores this community",
					Hint:     "Either the community is wrong, or the ACL attached to the community does not include " + src + ". Devices do not answer a refused community at all.",
					Snippets: aclSnippets(src)})
				fail(ProblemCredential)
				skip("SNMP view")
				return d
			}
			d.Steps = append(d.Steps, Step{Name: "SNMP answer", Status: StepFail, Detail: "no answer on UDP " + portText(t.Port),
				Hint:     ex.Hint + " If SNMP is not configured on the device at all, add it with the lines below.",
				Snippets: aclSnippets(src)})
			fail(ProblemTimeout)
			skip("Credentials", "SNMP view")
			return d
		case ProblemNoRoute, ProblemRefused, ProblemDNS:
			d.Steps = append(d.Steps, Step{Name: "SNMP answer", Status: StepFail, Detail: ex.Summary, Hint: ex.Hint})
			fail(ex.Problem)
			skip("Credentials", "SNMP view")
			return d
		}
		d.Steps = append(d.Steps, Step{Name: "SNMP answer", Status: StepOK, Detail: "the device answers"})
		st := Step{Name: "Credentials", Status: StepFail, Detail: ex.Summary, Hint: ex.Hint}
		if ex.Problem == ProblemAuthz || ex.Problem == ProblemNoData {
			st.Snippets = viewSnippets(src)
		}
		if w := CommunityWarning(t.Cred.Community); w != "" && t.Cred.Version != "3" {
			st.Hint = w + " " + st.Hint
		}
		d.Steps = append(d.Steps, st)
		fail(ex.Problem)
		skip("SNMP view")
		return d
	}
	defer s.Close()
	info.Identity = IdentifyFromSystem(info.SysDescr, info.SysObjectID)
	d.Info = info
	d.Steps = append(d.Steps, Step{Name: "SNMP answer", Status: StepOK, Detail: "the device answers"})
	who := info.SysName
	if info.Model != "" {
		who += " · " + info.Model
	}
	if info.Version != "" {
		who += " " + info.Version
	}
	d.Steps = append(d.Steps, Step{Name: "Credentials", Status: StepOK, Detail: strings.TrimPrefix(who, " · ")})

	// 5. View: interfaces and their names.
	descr, err := walkColumn(s, oidIfDescr)
	if err != nil || len(descr) == 0 {
		st := Step{Name: "SNMP view", Status: StepFail, Detail: "the interface table (ifTable) is not visible",
			Hint: "The SNMP view of this community or user hides 1.3.6.1.2.1.2. Give it a view that includes the whole tree (iso).", Snippets: viewSnippets(src)}
		if err != nil {
			st.Detail = Explain(err, t.Cred.Version, src).Summary
		}
		d.Steps = append(d.Steps, st)
		fail(ProblemAuthz)
		return d
	}
	var first string
	for idx := range descr {
		if first == "" || len(idx) < len(first) || (len(idx) == len(first) && idx < first) {
			first = idx
		}
	}
	view := Step{Name: "SNMP view", Status: StepOK, Detail: fmt.Sprintf("%d interfaces", len(descr))}
	if pdus, err := s.Get([]string{oidIfName + "." + first}); err != nil || len(pdus) == 0 {
		view.Status = StepWarn
		view.Detail += ", but ifXTable is hidden"
		view.Hint = "Without ifXTable (1.3.6.1.2.1.31) there are no 64-bit counters and no port names; widen the view."
		view.Snippets = viewSnippets(src)
	}
	d.Steps = append(d.Steps, view)
	d.OK = true
	return d
}

func portText(p int) string {
	if p == 0 {
		return "161"
	}
	return fmt.Sprint(p)
}

func aclSnippets(src string) []Snippet {
	return []Snippet{
		{"IOS / IOS-XE", "ip access-list standard SNMP-RO\n permit host " + src + "\nsnmp-server community <community> RO SNMP-RO"},
		{"NX-OS", "ip access-list SNMP-RO\n  permit udp host " + src + " any eq snmp\nsnmp-server community <community> group network-operator\nsnmp-server community <community> use-ipv4acl SNMP-RO"},
		{"IOS XR (NCS, ASR 9000)", "ipv4 access-list SNMP-RO\n 10 permit ipv4 host " + src + " any\nsnmp-server community <community> RO IPv4 SNMP-RO"},
		{"ASA / FTD", "snmp-server host <interface> " + src + " poll community <community> version 2c"},
	}
}

func viewSnippets(src string) []Snippet {
	return []Snippet{
		{"IOS / IOS-XE", "snmp-server view ZIROX iso included\nsnmp-server community <community> view ZIROX RO SNMP-RO"},
		{"NX-OS", "snmp-server community <community> group network-operator"},
		{"IOS XR (NCS, ASR 9000)", "snmp-server view ZIROX 1.3.6.1 included\nsnmp-server community <community> view ZIROX RO IPv4 SNMP-RO"},
		{"SNMPv3 (IOS)", "snmp-server view ZIROX iso included\nsnmp-server group ZIROX v3 priv read ZIROX access SNMP-RO\nsnmp-server user <user> ZIROX v3 auth sha <auth-pass> priv aes 128 <priv-pass>"},
	}
}
