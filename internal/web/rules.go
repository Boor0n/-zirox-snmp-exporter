package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// ruleRow is a port rule with the ports it matches today.
type ruleRow struct {
	store.PortRule
	Matches  int
	Devices  int
	Examples []string
}

// rulePreview counts, for each rule, the existing ports it is the first
// match for, so the user sees what a pattern catches before relying on it.
func (s *Server) rulePreview(rs store.Rules) ([]ruleRow, int, error) {
	rows := make([]ruleRow, len(rs))
	for i, r := range rs {
		rows[i].PortRule = r
	}
	devs, err := s.st.Devices()
	if err != nil {
		return nil, 0, err
	}
	ports := 0
	devSeen := make([]map[int64]bool, len(rs))
	for _, d := range devs {
		objs, err := s.st.Objects(d.ID)
		if err != nil {
			return nil, 0, err
		}
		for _, o := range objs {
			if o.Kind != "if" {
				continue
			}
			ports++
			for i := range rs {
				if !rs[i].Matches(o, d.Group) {
					continue
				}
				rows[i].Matches++
				if devSeen[i] == nil {
					devSeen[i] = map[int64]bool{}
				}
				devSeen[i][d.ID] = true
				if len(rows[i].Examples) < 4 {
					ex := d.Name + " " + o.Name
					if o.Alias != "" {
						ex += " (" + o.Alias + ")"
					}
					rows[i].Examples = append(rows[i].Examples, ex)
				}
				break
			}
		}
	}
	for i := range rows {
		rows[i].Devices = len(devSeen[i])
	}
	return rows, ports, nil
}

func (s *Server) rules(w http.ResponseWriter, r *http.Request) {
	rs, err := s.st.PortRules()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, ports, err := s.rulePreview(rs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	edit := store.PortRule{Field: store.FieldAny, Action: store.RulePoll}
	if id, _ := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64); id > 0 {
		for _, x := range rs {
			if x.ID == id {
				edit = x
			}
		}
	}
	devs, _ := s.st.Devices()
	groups := map[string]bool{}
	for _, d := range devs {
		if d.Group != "" {
			groups[d.Group] = true
		}
	}
	var gl []string
	for g := range groups {
		gl = append(gl, g)
	}
	sort.Strings(gl)
	s.render(w, r, "rules", map[string]any{
		"Rules": rows, "Ports": ports, "Edit": edit, "Groups": gl, "Roles": store.Roles,
		"Fast": s.st.Settings().FastIntervalSec,
	})
}

func ruleText(r store.PortRule) string {
	what := map[string]string{store.RulePoll: "tick", store.RuleSkip: "untick", "": "keep tick"}[r.Action]
	if r.Role != "" {
		what += ", role " + r.Role
	}
	where := map[string]string{store.FieldName: "name", store.FieldAlias: "description"}[r.Field]
	if where == "" {
		where = "name or description"
	}
	t := fmt.Sprintf("%s ~ /%s/ → %s", where, r.Pattern, what)
	if r.Group != "" {
		t += " (group " + r.Group + ")"
	}
	return t
}

func (s *Server) ruleSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	rule := store.PortRule{
		ID: id, Field: r.FormValue("field"), Pattern: r.FormValue("pattern"),
		Group: r.FormValue("group"), Action: r.FormValue("action"), Role: r.FormValue("role"),
	}
	if err := s.st.SavePortRule(&rule); err != nil {
		redirectErr(w, r, "/rules", err)
		return
	}
	action := "port rule added"
	if id > 0 {
		action = "port rule changed"
	}
	s.audit(r, action, "port rules", ruleText(rule))
	redirect(w, r, "/rules", "Rule saved. It applies to ports found by the next Refresh; use \"Apply to existing ports\" to change ports already known")
}

func (s *Server) ruleDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	rs, _ := s.st.PortRules()
	text := fmt.Sprint(id)
	for _, x := range rs {
		if x.ID == id {
			text = ruleText(x)
		}
	}
	if err := s.st.DeletePortRule(id); err != nil {
		redirectErr(w, r, "/rules", err)
		return
	}
	s.audit(r, "port rule deleted", "port rules", text)
	redirect(w, r, "/rules", "Rule deleted")
}

func (s *Server) ruleMove(w http.ResponseWriter, r *http.Request) {
	dir := 1
	if r.FormValue("dir") == "up" {
		dir = -1
	}
	if err := s.st.MovePortRule(pathID(r), dir); err != nil {
		redirectErr(w, r, "/rules", err)
		return
	}
	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

// rulesApply runs the rules over every known port, overriding ticks and
// roles set by hand on the ports they match.
func (s *Server) rulesApply(w http.ResponseWriter, r *http.Request) {
	rs, err := s.st.PortRules()
	if err != nil {
		redirectErr(w, r, "/rules", err)
		return
	}
	changed, err := s.st.ApplyRulesAll(rs)
	if err != nil {
		redirectErr(w, r, "/rules", err)
		return
	}
	total := 0
	var names []string
	for id, n := range changed {
		total += n
		if d, err := s.st.Device(id); err == nil {
			names = append(names, fmt.Sprintf("%s (%d)", d.Name, n))
		}
		s.en.Reload(id)
	}
	sort.Strings(names)
	s.audit(r, "port rules applied", "port rules", fmt.Sprintf("%d ports changed: %s", total, shortList(names)))
	if total == 0 {
		redirect(w, r, "/rules", "Nothing to change: every matching port already has the tick and role of its rule")
		return
	}
	redirect(w, r, "/rules", fmt.Sprintf("Changed %d ports on %d devices", total, len(changed)))
}
