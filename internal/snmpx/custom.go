package snmpx

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/gosnmp/gosnmp"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// CategoryCustom is the device card section of user-added OIDs.
const CategoryCustom = "Custom"

var (
	customMu   sync.RWMutex
	customDefs []store.CustomOID
	customKind = map[string]Kind{}
)

// SetCustom replaces the user-added OIDs that discovery looks for and
// polling understands. Call it on start and after every change.
func SetCustom(defs []store.CustomOID) {
	kinds := make(map[string]Kind, len(defs))
	for _, c := range defs {
		m := Metric{Name: c.Name, Help: c.Help, Counter: c.Counter, Scale: c.Scale}
		if m.Help == "" {
			m.Help = "Custom OID " + c.OID + "."
		}
		k := Kind{Category: CategoryCustom}
		if c.Mode == store.CustomScalar {
			// The object index is the last arc of the OID, so polling asks for
			// exactly the OID the user entered.
			m.OID, _ = splitLastArc(c.OID)
			k.Labels = func(store.Object) []string { return nil }
		} else {
			m.OID = c.OID
			k.LabelKeys = []string{"index", "name"}
			k.Labels = idxName
		}
		k.Metrics = []Metric{m}
		kinds[c.Kind()] = k
	}
	customMu.Lock()
	customDefs = append([]store.CustomOID(nil), defs...)
	customKind = kinds
	customMu.Unlock()
}

func customOIDs() []store.CustomOID {
	customMu.RLock()
	defer customMu.RUnlock()
	return customDefs
}

// kindSpec returns the spec of a built-in or custom object kind.
func kindSpec(kind string) (Kind, bool) {
	if k, ok := Kinds[kind]; ok {
		return k, true
	}
	customMu.RLock()
	defer customMu.RUnlock()
	k, ok := customKind[kind]
	return k, ok
}

func splitLastArc(oid string) (base, last string) {
	i := strings.LastIndexByte(oid, '.')
	if i < 0 {
		return "", oid
	}
	return oid[:i], oid[i+1:]
}

var (
	oidRe        = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// NormalizeOID trims spaces and a leading dot and checks the OID is numeric.
func NormalizeOID(s string) (string, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), ".")
	if !oidRe.MatchString(s) {
		return "", fmt.Errorf("%q is not a numeric OID like 1.3.6.1.4.1.9.9.48.1.1.1.5", s)
	}
	return s, nil
}

// builtinMetric reports whether name is a metric Zirox already exports.
func builtinMetric(name string) bool {
	if strings.HasPrefix(name, "zirox_poll_") || name == "zirox_device_info" {
		return true
	}
	for _, k := range Kinds {
		for _, m := range k.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

// CheckCustom validates and normalizes a custom OID before it is saved.
func CheckCustom(c *store.CustomOID) error {
	c.Name = strings.TrimSpace(c.Name)
	c.Help = strings.TrimSpace(c.Help)
	switch {
	case c.Name == "":
		return errors.New("enter a metric name")
	case !metricNameRe.MatchString(c.Name):
		return fmt.Errorf("metric name %q may only contain letters, digits and _, and must not start with a digit", c.Name)
	case builtinMetric(c.Name):
		return fmt.Errorf("%s is already exported by Zirox; choose another name", c.Name)
	}
	var err error
	if c.OID, err = NormalizeOID(c.OID); err != nil {
		return err
	}
	switch c.Mode {
	case store.CustomScalar:
		c.NameOID = ""
	case store.CustomTable:
		if strings.TrimSpace(c.NameOID) != "" {
			if c.NameOID, err = NormalizeOID(c.NameOID); err != nil {
				return fmt.Errorf("name column: %w", err)
			}
		}
	default:
		return errors.New("choose single value or table column")
	}
	if c.Scale == 0 {
		c.Scale = 1
	}
	return nil
}

// discoverCustom finds the objects of user-added OIDs. Like the built-in
// health objects it is best effort: an OID the device lacks adds nothing.
func discoverCustom(s Session) []store.Object {
	var out []store.Object
	for _, c := range customOIDs() {
		descr := c.Name
		if c.Mode == store.CustomScalar {
			pdus, _ := s.Get([]string{c.OID})
			if len(pdus) == 0 {
				continue
			}
			_, idx := splitLastArc(c.OID)
			name := c.Help
			if name == "" {
				name = c.Name
			}
			out = append(out, store.Object{Kind: c.Kind(), Idx: idx, Name: name, Key: c.Name, Factor: 1, Descr: descr})
			continue
		}
		rows, _ := walkColumn(s, c.OID)
		if len(rows) == 0 {
			continue
		}
		var names map[string]gosnmp.SnmpPDU
		if c.NameOID != "" {
			names, _ = walkColumn(s, c.NameOID)
		}
		for idx := range rows {
			name := idx
			if p, ok := names[idx]; ok {
				if n := strings.TrimSpace(asString(p)); n != "" {
					name = n
				}
			}
			out = append(out, store.Object{Kind: c.Kind(), Idx: idx, Name: name, Key: name, Factor: 1, Descr: descr})
		}
	}
	return out
}

// autoSelectCustom reports whether newly found objects of kind start selected.
func autoSelectCustom(kind string) bool {
	for _, c := range customOIDs() {
		if c.Kind() == kind {
			return c.AutoSelect
		}
	}
	return false
}

// LookupRow is one value read by Lookup.
type LookupRow struct {
	OID     string
	Type    string
	Text    string
	Numeric bool
	Column  string // walk only: the table column this value belongs to, on its first row
}

// LookupLimit caps how many rows Lookup returns.
const LookupLimit = 200

// Lookup reads an OID (GET) or walks a subtree, for checking an OID by hand.
func Lookup(s Session, oid string, walk bool) (rows []LookupRow, more bool, err error) {
	var pdus []gosnmp.SnmpPDU
	if walk {
		pdus, err = s.Walk(oid)
	} else {
		pdus, err = s.Get([]string{oid})
	}
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	for _, p := range pdus {
		switch p.Type {
		case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView:
			continue
		}
		if len(rows) == LookupLimit {
			return rows, true, nil
		}
		r := LookupRow{OID: trimOID(p.Name), Type: p.Type.String(), Text: asString(p)}
		switch p.Type {
		case gosnmp.Integer, gosnmp.Counter32, gosnmp.Counter64, gosnmp.Gauge32, gosnmp.TimeTicks, gosnmp.Uinteger32,
			gosnmp.OpaqueFloat, gosnmp.OpaqueDouble:
			r.Numeric = true
		case gosnmp.OctetString:
			_, perr := strconv.ParseFloat(strings.TrimSpace(r.Text), 64)
			r.Numeric = perr == nil
		case gosnmp.ObjectIdentifier:
			r.Text = trimOID(r.Text)
		}
		if walk {
			if col := columnOf(oid, r.OID); col != "" && !seen[col] {
				seen[col] = true
				r.Column = col
			}
		}
		rows = append(rows, r)
	}
	return rows, false, nil
}

// columnOf guesses the table column of a walked value: the walked root when
// the value sits right under it (the user walked a column), otherwise the
// root plus the next arc (the user walked a table or its entry).
func columnOf(root, oid string) string {
	rest := strings.TrimPrefix(oid, root+".")
	if rest == oid {
		return ""
	}
	if !strings.Contains(rest, ".") {
		return root
	}
	return root + "." + rest[:strings.IndexByte(rest, '.')]
}
