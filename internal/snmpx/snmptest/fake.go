// Package snmptest provides an in-memory SNMP agent for tests.
package snmptest

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Agent is a fake SNMP agent holding OID -> value.
type Agent struct {
	mu      sync.Mutex
	vals    map[string]gosnmp.SnmpPDU
	Down    bool
	Ver     string
	Gets    int           // number of GET calls
	Walks   int           // number of walk calls
	Delay   time.Duration // simulated network round trip per Get or Walk
	GotOIDs int           // total OIDs requested by GET
}

// New returns an empty agent speaking SNMP v2c.
func New() *Agent { return &Agent{vals: map[string]gosnmp.SnmpPDU{}, Ver: "2c"} }

// Set stores a value. v may be string, int, uint, uint64.
func (a *Agent) Set(oid string, v any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := gosnmp.SnmpPDU{Name: "." + oid}
	switch x := v.(type) {
	case string:
		p.Type, p.Value = gosnmp.OctetString, []byte(x)
	case int:
		p.Type, p.Value = gosnmp.Integer, x
	case uint:
		p.Type, p.Value = gosnmp.Counter32, x
	case uint64:
		p.Type, p.Value = gosnmp.Counter64, x
	default:
		panic("unsupported value type")
	}
	a.vals[oid] = p
}

// Delete removes every OID under prefix.
func (a *Agent) Delete(prefix string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.vals {
		if k == prefix || strings.HasPrefix(k, prefix+".") {
			delete(a.vals, k)
		}
	}
}

// Session implements snmpx.Session against the agent.
type Session struct{ A *Agent }

// Dial returns a session; it fails when the agent is down.
func (a *Agent) Dial() (*Session, error) {
	if a.Down {
		return nil, errors.New("timeout")
	}
	return &Session{A: a}, nil
}

func (s *Session) Version() string { return s.A.Ver }
func (s *Session) Close()          {}

func (s *Session) Walk(root string) ([]gosnmp.SnmpPDU, error) {
	a := s.A
	time.Sleep(a.Delay)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Down {
		return nil, errors.New("timeout")
	}
	a.Walks++
	var keys []string
	for k := range a.vals {
		if strings.HasPrefix(k, root+".") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]gosnmp.SnmpPDU, 0, len(keys))
	for _, k := range keys {
		out = append(out, a.vals[k])
	}
	return out, nil
}

func (s *Session) Get(oids []string) ([]gosnmp.SnmpPDU, error) {
	a := s.A
	time.Sleep(a.Delay)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Down {
		return nil, errors.New("timeout")
	}
	a.Gets++
	a.GotOIDs += len(oids)
	var out []gosnmp.SnmpPDU
	for _, o := range oids {
		if p, ok := a.vals[strings.TrimPrefix(o, ".")]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// Counts returns (gets, walks, oids requested) and resets them.
func (a *Agent) Counts() (int, int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, w, o := a.Gets, a.Walks, a.GotOIDs
	a.Gets, a.Walks, a.GotOIDs = 0, 0, 0
	return g, w, o
}

// Switch builds an agent that looks like a small Cisco switch:
// ports Eth1/1..Eth1/n (odd ones up), Vlan10, a CPU and a temperature sensor.
func Switch(n int) *Agent {
	a := New()
	a.Set("1.3.6.1.2.1.1.1.0", "Cisco NX-OS(tm) n9000, Software (n9000-dk9)")
	a.Set("1.3.6.1.2.1.1.2.0", ".1.3.6.1.4.1.9.12.3.1.3.1812")
	a.Set("1.3.6.1.2.1.1.5.0", "core-nx-01")
	for i := 1; i <= n; i++ {
		AddPort(a, i, "Ethernet1/"+itoa(i), 6, i%2 == 1)
	}
	AddPort(a, 1000, "Vlan10", 136, true)
	a.Set("1.3.6.1.4.1.9.9.109.1.1.1.1.2.1", 22)
	a.Set("1.3.6.1.4.1.9.9.109.1.1.1.1.7.1", 12)
	a.Set("1.3.6.1.4.1.9.9.109.1.1.1.1.8.1", 10)
	a.Set("1.3.6.1.2.1.47.1.1.1.1.7.22", "Supervisor CPU")
	a.Set("1.3.6.1.2.1.47.1.1.1.1.7.21590", "Module 1 Inlet Temp")
	a.Set("1.3.6.1.2.1.99.1.1.1.1.21590", 8) // celsius
	a.Set("1.3.6.1.2.1.99.1.1.1.2.21590", 9) // units
	a.Set("1.3.6.1.2.1.99.1.1.1.3.21590", 1) // one decimal
	a.Set("1.3.6.1.2.1.99.1.1.1.4.21590", 315)
	a.Set("1.3.6.1.2.1.99.1.1.1.5.21590", 1)
	return a
}

// AddPort adds one interface with counters.
func AddPort(a *Agent, idx int, name string, ifType int, up bool) {
	i := "." + itoa(idx)
	oper := 2
	if up {
		oper = 1
	}
	a.Set("1.3.6.1.2.1.2.2.1.2"+i, name)
	a.Set("1.3.6.1.2.1.2.2.1.3"+i, ifType)
	a.Set("1.3.6.1.2.1.2.2.1.5"+i, uint(4294967295))
	a.Set("1.3.6.1.2.1.2.2.1.7"+i, 1)
	a.Set("1.3.6.1.2.1.2.2.1.8"+i, oper)
	a.Set("1.3.6.1.2.1.2.2.1.13"+i, uint(0))
	a.Set("1.3.6.1.2.1.2.2.1.14"+i, uint(3))
	a.Set("1.3.6.1.2.1.2.2.1.19"+i, uint(0))
	a.Set("1.3.6.1.2.1.2.2.1.20"+i, uint(0))
	a.Set("1.3.6.1.2.1.31.1.1.1.1"+i, name)
	a.Set("1.3.6.1.2.1.31.1.1.1.6"+i, uint64(1000*idx))
	a.Set("1.3.6.1.2.1.31.1.1.1.7"+i, uint64(10*idx))
	a.Set("1.3.6.1.2.1.31.1.1.1.10"+i, uint64(2000*idx))
	a.Set("1.3.6.1.2.1.31.1.1.1.11"+i, uint64(20*idx))
	a.Set("1.3.6.1.2.1.31.1.1.1.15"+i, 10000)
	a.Set("1.3.6.1.2.1.31.1.1.1.18"+i, "to "+name)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
