package store

import (
	"fmt"
	"regexp"
	"strings"
)

// Port roles offered in the UI. Any short word is accepted; these are the
// ones the dashboards and alerts know.
var Roles = []string{"uplink", "core", "server", "access"}

// RoleUplink ports are polled at the fast interval when it is on.
const RoleUplink = "uplink"

// NormRole trims and lower-cases a role and keeps it to a label-safe word.
func NormRole(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	var b strings.Builder
	for _, c := range r {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteRune(c)
		}
		if b.Len() >= 32 {
			break
		}
	}
	return b.String()
}

// Port rule actions.
const (
	RulePoll = "poll" // tick the port
	RuleSkip = "skip" // leave it unticked
	// "" keeps the default choice (up physical ports ticked) and only sets the role.
)

// Rule fields a pattern is matched against.
const (
	FieldAny   = "any" // name, description or ifAlias
	FieldName  = "name"
	FieldAlias = "alias"
)

// PortRule ticks or unticks new ports and sets their role, by a regular
// expression on the port name or description. Rules are checked in order;
// the first match wins. They apply to ports a discovery finds for the first
// time, so ticks and roles set by hand are never overwritten.
type PortRule struct {
	ID      int64
	Pos     int
	Field   string
	Pattern string
	Group   string // only devices of this group ("" = all)
	Action  string
	Role    string

	re *regexp.Regexp
}

// Compile checks the pattern; matching is case-insensitive.
func (r *PortRule) Compile() error {
	re, err := regexp.Compile("(?i)" + r.Pattern)
	if err != nil {
		return fmt.Errorf("pattern %q: %w", r.Pattern, err)
	}
	r.re = re
	return nil
}

// Matches reports whether the rule applies to a port of a device in group.
func (r *PortRule) Matches(o Object, group string) bool {
	if o.Kind != "if" || r.Pattern == "" {
		return false
	}
	if r.Group != "" && !strings.EqualFold(r.Group, group) {
		return false
	}
	if r.re == nil && r.Compile() != nil {
		return false
	}
	switch r.Field {
	case FieldName:
		return r.re.MatchString(o.Name)
	case FieldAlias:
		return r.re.MatchString(o.Alias)
	}
	return r.re.MatchString(o.Name) || r.re.MatchString(o.Alias) || (o.Descr != o.Name && r.re.MatchString(o.Descr))
}

// Rules is an ordered rule list.
type Rules []PortRule

// Apply sets o.Selected and o.Role from the first matching rule and returns
// it, or nil when none matches. def is the tick the port would get without rules.
func (rs Rules) Apply(o *Object, group string, def bool) *PortRule {
	o.Selected = def
	for i := range rs {
		r := &rs[i]
		if !r.Matches(*o, group) {
			continue
		}
		switch r.Action {
		case RulePoll:
			o.Selected = true
		case RuleSkip:
			o.Selected = false
		}
		if r.Role != "" {
			o.Role = r.Role
		}
		return r
	}
	return nil
}

// PortRules returns all rules in order, compiled.
func (s *Store) PortRules() (Rules, error) {
	rows, err := s.db.Query(`SELECT id,pos,field,pattern,grp,action,role FROM port_rules ORDER BY pos, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out Rules
	for rows.Next() {
		var r PortRule
		if err := rows.Scan(&r.ID, &r.Pos, &r.Field, &r.Pattern, &r.Group, &r.Action, &r.Role); err != nil {
			return nil, err
		}
		_ = r.Compile()
		out = append(out, r)
	}
	return out, rows.Err()
}

// SavePortRule inserts (ID 0, at the end) or updates a rule.
func (s *Store) SavePortRule(r *PortRule) error {
	r.Pattern = strings.TrimSpace(r.Pattern)
	if r.Pattern == "" {
		return fmt.Errorf("the pattern is empty")
	}
	if err := r.Compile(); err != nil {
		return err
	}
	switch r.Field {
	case FieldName, FieldAlias, FieldAny:
	default:
		r.Field = FieldAny
	}
	switch r.Action {
	case RulePoll, RuleSkip, "":
	default:
		return fmt.Errorf("unknown action %q", r.Action)
	}
	r.Role = NormRole(r.Role)
	r.Group = strings.TrimSpace(r.Group)
	if r.Action == "" && r.Role == "" {
		return fmt.Errorf("the rule neither ticks, unticks nor sets a role")
	}
	if r.ID == 0 {
		_ = s.db.QueryRow(`SELECT COALESCE(MAX(pos),0)+1 FROM port_rules`).Scan(&r.Pos)
		res, err := s.db.Exec(`INSERT INTO port_rules(pos,field,pattern,grp,action,role) VALUES(?,?,?,?,?,?)`, r.Pos, r.Field, r.Pattern, r.Group, r.Action, r.Role)
		if err != nil {
			return err
		}
		r.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.db.Exec(`UPDATE port_rules SET field=?,pattern=?,grp=?,action=?,role=? WHERE id=?`, r.Field, r.Pattern, r.Group, r.Action, r.Role, r.ID)
	return err
}

// DeletePortRule removes a rule.
func (s *Store) DeletePortRule(id int64) error {
	_, err := s.db.Exec(`DELETE FROM port_rules WHERE id=?`, id)
	return err
}

// MovePortRule swaps a rule with its neighbour (dir -1 = up, +1 = down).
func (s *Store) MovePortRule(id int64, dir int) error {
	rs, err := s.PortRules()
	if err != nil {
		return err
	}
	for i := range rs {
		if rs[i].ID != id {
			continue
		}
		j := i + dir
		if j < 0 || j >= len(rs) {
			return nil
		}
		rs[i], rs[j] = rs[j], rs[i]
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for k, r := range rs {
			if _, err := tx.Exec(`UPDATE port_rules SET pos=? WHERE id=?`, k+1, r.ID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	return ErrNotFound
}

// ApplyRulesAll applies the rules to every existing port of every device,
// overriding ticks and roles set by hand on matching ports. Ports no rule
// matches are left alone. It returns the number of ports changed per device.
func (s *Store) ApplyRulesAll(rs Rules) (map[int64]int, error) {
	devs, err := s.Devices()
	if err != nil {
		return nil, err
	}
	type upd struct {
		id  int64
		sel bool
		r   string
	}
	var ups []upd
	changed := map[int64]int{}
	for _, d := range devs {
		objs, err := s.Objects(d.ID)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			n := o
			if rs.Apply(&n, d.Group, o.Selected) == nil || (n.Selected == o.Selected && n.Role == o.Role) {
				continue
			}
			ups = append(ups, upd{o.ID, n.Selected, n.Role})
			changed[d.ID]++
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, u := range ups {
		if _, err := tx.Exec(`UPDATE objects SET selected=?, role=? WHERE id=?`, u.sel, u.r, u.id); err != nil {
			return nil, err
		}
	}
	return changed, tx.Commit()
}
