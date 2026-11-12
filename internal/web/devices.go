package web

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Boor0n/zirox-snmp-exporter/internal/engine"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// ---- devices ----

// DeviceRow is one line of the device list.
type DeviceRow struct {
	store.Device
	Selected    int
	Total       int
	Snap        *engine.Snapshot
	Eff         engine.Effective
	Discovering bool
	Credential  string
	Profile     string
	History     []engine.Bucket
}

func (s *Server) deviceRows() ([]DeviceRow, error) {
	devs, err := s.st.Devices()
	if err != nil {
		return nil, err
	}
	counts, err := s.st.ObjectCounts()
	if err != nil {
		return nil, err
	}
	creds, _ := s.st.Credentials()
	credName := map[int64]string{}
	for _, c := range creds {
		credName[c.ID] = c.Name
	}
	profs, _ := s.st.Profiles()
	profName := map[int64]string{}
	for _, p := range profs {
		profName[p.ID] = p.Name
	}
	rows := make([]DeviceRow, 0, len(devs))
	for _, d := range devs {
		c := counts[d.ID]
		rows = append(rows, DeviceRow{
			Device: d, Selected: c[0], Total: c[1], Snap: s.en.Snapshot(d.ID), Eff: s.en.Resolve(d),
			Discovering: s.en.Discovering(d.ID), Credential: credName[d.CredentialID], Profile: profName[d.ProfileID],
			History: s.en.History(d.ID),
		})
	}
	return rows, nil
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deviceRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	group := r.URL.Query().Get("group")
	status := r.URL.Query().Get("status")
	groups := map[string]bool{}
	var shown []DeviceRow
	stats := map[string]int{}
	for _, d := range rows {
		if d.Group != "" {
			groups[d.Group] = true
		}
		st := rowStatus(d)
		stats[st]++
		if q != "" && !strings.Contains(strings.ToLower(d.Name+" "+d.Address+" "+d.SysName+" "+d.Group), q) {
			continue
		}
		if group != "" && d.Group != group {
			continue
		}
		if status != "" && st != status {
			continue
		}
		shown = append(shown, d)
	}
	var groupList []string
	for g := range groups {
		groupList = append(groupList, g)
	}
	sort.Strings(groupList)
	creds, _ := s.st.Credentials()
	profs, _ := s.st.Profiles()
	s.render(w, r, "devices", map[string]any{
		"Creds": creds, "Profiles": profs, "Rows": shown, "Total": len(rows), "Groups": groupList, "Q": r.URL.Query().Get("q"),
		"Group": group, "Status": status, "Stats": stats, "AnyDiscovering": s.en.AnyDiscovering(),
	})
}

func rowStatus(d DeviceRow) string {
	switch {
	case !d.Enabled:
		return "disabled"
	case d.Discovering:
		return "discovering"
	case d.LastDiscovery.IsZero() && d.DiscoveryError != "":
		return "failing"
	case d.LastDiscovery.IsZero():
		return "pending"
	case d.Snap == nil:
		return "pending"
	case d.Snap.OK:
		return "ok"
	}
	return "failing"
}

func (s *Server) formLists() (map[string]any, error) {
	creds, err := s.st.Credentials()
	if err != nil {
		return nil, err
	}
	profs, err := s.st.Profiles()
	if err != nil {
		return nil, err
	}
	return map[string]any{"Credentials": creds, "Profiles": profs, "Global": s.st.Settings()}, nil
}

func (s *Server) deviceForm(w http.ResponseWriter, r *http.Request) {
	data, err := s.formLists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := store.Device{Port: 161, Enabled: true}
	if id := pathID(r); id != 0 {
		if d, err = s.st.Device(id); err != nil {
			http.NotFound(w, r)
			return
		}
	} else if creds := data["Credentials"].([]store.Credential); len(creds) > 0 {
		d.CredentialID = creds[0].ID
	}
	data["Device"] = d
	data["Cred"] = store.Credential{Version: "2c", V3Level: "authPriv", V3AuthProto: "SHA", V3PrivProto: "AES"}
	if d.CredentialID == 0 {
		data["NewCred"] = true
	}
	s.render(w, r, "device_form", data)
}

func deviceFromForm(r *http.Request) store.Device {
	d := store.Device{
		ID:          pathID(r),
		Name:        strings.TrimSpace(r.FormValue("name")),
		Address:     strings.TrimSpace(r.FormValue("address")),
		Port:        formInt(r, "port"),
		IntervalSec: formInt(r, "interval_sec"),
		Group:       strings.TrimSpace(r.FormValue("group")),
		Enabled:     r.FormValue("enabled") == "on",
	}
	d.ProfileID, _ = strconv.ParseInt(r.FormValue("profile_id"), 10, 64)
	d.CredentialID, _ = strconv.ParseInt(r.FormValue("credential_id"), 10, 64)
	if d.ID == 0 {
		d.ID, _ = strconv.ParseInt(r.FormValue("id"), 10, 64)
	}
	if d.Name == "" {
		d.Name = d.Address
	}
	return d
}

func credFromForm(r *http.Request) store.Credential {
	return store.Credential{
		Name:        strings.TrimSpace(r.FormValue("cred_name")),
		Version:     r.FormValue("version"),
		Community:   r.FormValue("community"),
		V3User:      strings.TrimSpace(r.FormValue("v3_user")),
		V3Level:     r.FormValue("v3_level"),
		V3AuthProto: r.FormValue("v3_auth_proto"),
		V3AuthPass:  r.FormValue("v3_auth_pass"),
		V3PrivProto: r.FormValue("v3_priv_proto"),
		V3PrivPass:  r.FormValue("v3_priv_pass"),
		V3Context:   strings.TrimSpace(r.FormValue("v3_context")),
	}
}

func validateCred(c store.Credential) error {
	switch c.Version {
	case "1", "2c":
		if c.Community == "" {
			return errors.New("enter a community")
		}
	case "3":
		if c.V3User == "" {
			return errors.New("enter an SNMPv3 user")
		}
		if c.V3Level != "noAuthNoPriv" && len(c.V3AuthPass) < 8 {
			return errors.New("SNMPv3 auth password must be at least 8 characters")
		}
		if c.V3Level == "authPriv" && len(c.V3PrivPass) < 8 {
			return errors.New("SNMPv3 privacy password must be at least 8 characters")
		}
	default:
		return errors.New("choose an SNMP version")
	}
	return nil
}

// resolveCredential returns the credential chosen on the device form: an
// existing one, or one typed in place (reusing an identical saved one).
func (s *Server) resolveCredential(r *http.Request, devName string) (store.Credential, error) {
	if r.FormValue("credential_id") != "new" {
		id, _ := strconv.ParseInt(r.FormValue("credential_id"), 10, 64)
		c, err := s.st.Credential(id)
		if err != nil {
			return c, errors.New("choose credentials")
		}
		return c, nil
	}
	c := credFromForm(r)
	if err := validateCred(c); err != nil {
		return c, err
	}
	return s.findOrCreateCredential(c, devName)
}

func (s *Server) findOrCreateCredential(c store.Credential, hint string) (store.Credential, error) {
	all, err := s.st.Credentials()
	if err != nil {
		return c, err
	}
	for _, ex := range all {
		plain, err := s.en.DecryptCredential(ex)
		if err != nil {
			continue
		}
		plain.ID, plain.Name = 0, ""
		cmp := c
		cmp.Name = ""
		if plain == cmp {
			return ex, nil
		}
	}
	if c.Name == "" {
		c.Name = "SNMPv" + c.Version + " (" + hint + ")"
	}
	if err := s.encrypt(&c); err != nil {
		return c, err
	}
	return c, s.st.SaveCredential(&c)
}

func (s *Server) encrypt(c *store.Credential) error {
	var err error
	for _, f := range []*string{&c.Community, &c.V3AuthPass, &c.V3PrivPass} {
		if *f, err = s.box.Encrypt(*f); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) rerenderDeviceForm(w http.ResponseWriter, r *http.Request, d store.Device, msg string, ok bool) {
	data, err := s.formLists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data["Device"] = d
	c := credFromForm(r)
	c.Community, c.V3AuthPass, c.V3PrivPass = "", "", "" // never echo secrets back
	data["Cred"] = c
	data["NewCred"] = r.FormValue("credential_id") == "new"
	if ok {
		data["TestOK"] = msg
	} else {
		data["Error"] = msg
	}
	s.render(w, r, "device_form", data)
}

func (s *Server) deviceSave(w http.ResponseWriter, r *http.Request) {
	d := deviceFromForm(r)
	if d.Address == "" {
		s.rerenderDeviceForm(w, r, d, "enter an IP address or DNS name", false)
		return
	}
	c, err := s.resolveCredential(r, d.Name)
	if err != nil {
		s.rerenderDeviceForm(w, r, d, err.Error(), false)
		return
	}
	d.CredentialID = c.ID
	isNew := d.ID == 0
	var before store.Device
	if !isNew {
		if before, err = s.st.Device(d.ID); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	if err := s.st.SaveDevice(&d); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE") {
			msg = "a device with this name already exists"
		}
		s.rerenderDeviceForm(w, r, d, msg, false)
		return
	}
	if isNew {
		s.audit(r, "device added", d.Name, d.Address)
	} else {
		s.audit(r, "device changed", d.Name, s.deviceChanges(before, d))
	}
	// A new device, or one whose address or credentials changed, gets a full discovery.
	warn := communityWarning(c, s.en)
	if isNew || before.Address != d.Address || before.Port != d.Port || before.CredentialID != d.CredentialID || before.LastDiscovery.IsZero() {
		s.en.Refresh(d.ID)
		path := fmt.Sprintf("/devices/%d", d.ID)
		if isNew {
			path += "?wizard=1"
		}
		redirectWarn(w, r, path, "Device saved, full discovery running", warn)
		return
	}
	s.en.Reload(d.ID)
	redirectWarn(w, r, fmt.Sprintf("/devices/%d", d.ID), "Device saved", warn)
}

func (s *Server) deviceTest(w http.ResponseWriter, r *http.Request) {
	d := deviceFromForm(r)
	c, err := s.resolveCredentialNoSave(r)
	if err != nil {
		s.rerenderDeviceForm(w, r, d, err.Error(), false)
		return
	}
	eff := s.en.Resolve(d)
	diag := s.en.DiagnoseTarget(snmpx.Target{Address: d.Address, Port: d.Port, Cred: c, Timeout: eff.Timeout, Retries: 1})
	data, err := s.formLists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d.Name == "" && diag.Info.SysName != "" && d.ID == 0 {
		d.Name = deviceNameFor(scanRow{Address: d.Address, Info: diag.Info})
	}
	data["Device"] = d
	// Keep what the user typed so the device can be saved right after the check.
	data["Cred"] = credFromForm(r)
	data["NewCred"] = r.FormValue("credential_id") == "new"
	data["Diag"] = diag
	data["Step"] = 2
	s.render(w, r, "device_form", data)
}

func (s *Server) resolveCredentialNoSave(r *http.Request) (store.Credential, error) {
	if r.FormValue("credential_id") == "new" {
		c := credFromForm(r)
		return c, validateCred(c)
	}
	id, _ := strconv.ParseInt(r.FormValue("credential_id"), 10, 64)
	c, err := s.st.Credential(id)
	if err != nil {
		return c, errors.New("choose credentials")
	}
	return s.en.DecryptCredential(c)
}

// communityWarning checks the plain community of c (encrypted ones are
// decrypted first) for characters devices misread.
func communityWarning(c store.Credential, en *engine.Engine) string {
	if c.Version == "3" {
		return ""
	}
	if plain, err := en.DecryptCredential(c); err == nil {
		c = plain
	}
	return snmpx.CommunityWarning(c.Community)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// ObjectGroup is one section of the device card.
type ObjectGroup struct {
	Category string
	Tab      string // ports, health or other
	Objects  []store.Object
	Selected int
	HasDescr bool // false hides the empty Description column
	// Modules splits a long port list by module or stack member.
	Modules []PortModule
}

// PortModule is the ports of one module ("GigabitEthernet1/0").
type PortModule struct {
	Name    string
	Objects []store.Object
}

// tabOf puts a category on a tab of the device page.
func tabOf(cat string) string {
	switch cat {
	case "Ports":
		return "ports"
	case "CPU", "Memory", "Sensors", "Storage":
		return "health"
	}
	return "other"
}

// moduleOf is the part of a port name before the last "/", or the leading
// letters for names without one (Vlan10 -> Vlan).
func moduleOf(name string) string {
	if i := strings.LastIndexByte(name, '/'); i > 0 {
		return name[:i]
	}
	i := strings.IndexFunc(name, func(r rune) bool { return r >= '0' && r <= '9' })
	if i > 0 {
		return strings.TrimRight(name[:i], " ")
	}
	return name
}

// splitModules groups ports by module when a device has many of them.
func splitModules(objs []store.Object) []PortModule {
	if len(objs) < 24 {
		return nil
	}
	var out []PortModule
	idx := map[string]int{}
	for _, o := range objs {
		m := moduleOf(o.Name)
		i, ok := idx[m]
		if !ok {
			i = len(out)
			idx[m] = i
			out = append(out, PortModule{Name: m})
		}
		out[i].Objects = append(out[i].Objects, o)
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

func (s *Server) deviceCard(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	objs, err := s.st.Objects(d.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byCat := map[string]*ObjectGroup{}
	var nNew, nMissing int
	for _, o := range objs {
		cat := snmpx.CategoryOf(o.Kind)
		g, ok := byCat[cat]
		if !ok {
			g = &ObjectGroup{Category: cat, Tab: tabOf(cat)}
			byCat[cat] = g
		}
		g.Objects = append(g.Objects, o)
		if o.Alias != "" || (o.Descr != "" && o.Descr != o.Name) {
			g.HasDescr = true
		}
		if o.Selected && o.State != store.StateMissing {
			g.Selected++
		}
		switch o.State {
		case store.StateNew:
			nNew++
		case store.StateMissing:
			nMissing++
		}
	}
	var groups []ObjectGroup
	tabs := map[string]int{}
	for _, c := range append(snmpx.Categories, "Other") {
		if g, ok := byCat[c]; ok {
			if c == "Ports" {
				g.Modules = splitModules(g.Objects)
			}
			groups = append(groups, *g)
			tabs[g.Tab] += len(g.Objects)
		}
	}
	cred, _ := s.st.Credential(d.CredentialID)
	snap := s.en.Snapshot(d.ID)
	s.render(w, r, "device", map[string]any{
		"Device": d, "Groups": groups, "Snap": snap, "Eff": s.en.Resolve(d), "Discovering": s.en.Discovering(d.ID),
		"New": nNew, "Missing": nMissing, "Credential": cred, "SampleCount": sampleCount(snap),
		"Wizard": r.URL.Query().Get("wizard") == "1", "Rates": s.en.DeviceRates(d.ID), "Tabs": tabs,
		"History": s.en.History(d.ID), "Known": s.knownDevices(d.ID),
	})
}

// knownDevices maps the HostKey of every other device's name, sysName and
// address to its ID, to link CDP/LLDP neighbours to their pages.
func (s *Server) knownDevices(self int64) map[string]int64 {
	devs, _ := s.st.Devices()
	m := map[string]int64{}
	for _, d := range devs {
		if d.ID == self {
			continue
		}
		for _, n := range []string{d.Name, d.SysName, d.Address} {
			if k := store.HostKey(n); k != "" {
				m[k] = d.ID
			}
		}
	}
	return m
}

func sampleCount(s *engine.Snapshot) int {
	if s == nil {
		return 0
	}
	return len(s.Samples)
}

func (s *Server) deviceStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"discovering":%t}`, s.en.Discovering(pathID(r)))
}

func (s *Server) deviceRefresh(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	back := r.FormValue("back")
	if back != "list" {
		back = fmt.Sprintf("/devices/%d", id)
	} else {
		back = "/devices"
	}
	if s.en.Refresh(id) {
		if d, err := s.st.Device(id); err == nil {
			s.audit(r, "refresh", d.Name, "")
		}
		redirect(w, r, back, "Full discovery started")
		return
	}
	redirect(w, r, back, "Discovery is already running")
}

func (s *Server) deviceToggle(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.st.SetDeviceEnabled(d.ID, !d.Enabled); err != nil {
		redirectErr(w, r, "/devices", err)
		return
	}
	if d.Enabled {
		s.audit(r, "device disabled", d.Name, "")
	} else {
		s.audit(r, "device enabled", d.Name, "")
	}
	s.en.Reload(d.ID)
	if !d.Enabled && d.LastDiscovery.IsZero() {
		s.en.Refresh(d.ID)
	}
	redirect(w, r, "/devices", "")
}

func (s *Server) deviceDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	d, err := s.st.Device(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.st.DeleteDevice(id); err != nil {
		redirectErr(w, r, "/devices", err)
		return
	}
	s.audit(r, "device deleted", d.Name, d.Address)
	s.en.Reload(id)
	redirect(w, r, "/devices", "Device deleted")
}

func (s *Server) deviceSelection(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, fmt.Sprintf("/devices/%d", id), err)
		return
	}
	sel := map[int64]bool{}
	for _, v := range r.Form["obj"] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			sel[n] = true
		}
	}
	before, _ := s.st.Objects(id)
	if err := s.st.SaveSelection(id, sel); err != nil {
		redirectErr(w, r, fmt.Sprintf("/devices/%d", id), err)
		return
	}
	// Roles come as role_<id> fields; only changed ones are written.
	roles := map[int64]string{}
	var roleNotes []string
	for _, o := range before {
		v, ok := r.Form[fmt.Sprintf("role_%d", o.ID)]
		if !ok || len(v) == 0 {
			continue
		}
		if nr := store.NormRole(v[0]); nr != o.Role {
			roles[o.ID] = nr
			roleNotes = append(roleNotes, fmt.Sprintf("%s=%s", o.Name, orNone(nr)))
		}
	}
	if len(roles) > 0 {
		if err := s.st.SetObjectRoles(id, roles); err != nil {
			redirectErr(w, r, fmt.Sprintf("/devices/%d", id), err)
			return
		}
	}
	if d, err := s.st.Device(id); err == nil {
		s.audit(r, "selection changed", d.Name, selectionChanges(before, sel))
		if len(roleNotes) > 0 {
			s.audit(r, "port roles changed", d.Name, shortList(roleNotes))
		}
	}
	s.en.Reload(id)
	redirect(w, r, fmt.Sprintf("/devices/%d", id), fmt.Sprintf("Applied: polling %d objects", len(sel)))
}

func (s *Server) deviceBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, "/devices", err)
		return
	}
	var ids []int64
	for _, v := range r.Form["ids"] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	if len(ids) == 0 {
		redirect(w, r, "/devices", "Nothing selected")
		return
	}
	action := r.FormValue("action")
	if action == "check" {
		http.Redirect(w, r, "/checks/"+s.StartCheck(ids, fmt.Sprintf("%d selected devices", len(ids))), http.StatusSeeOther)
		return
	}
	var names []string
	for _, id := range ids {
		if d, err := s.st.Device(id); err == nil {
			names = append(names, d.Name)
		}
		switch action {
		case "refresh":
			s.en.Refresh(id)
		case "enable", "disable":
			if err := s.st.SetDeviceEnabled(id, action == "enable"); err == nil {
				s.en.Reload(id)
			}
		case "delete":
			if err := s.st.DeleteDevice(id); err == nil {
				s.en.Reload(id)
			}
		case "profile", "edit":
			d, err := s.st.Device(id)
			if err != nil {
				continue
			}
			if v := r.FormValue("profile_id"); v != "" && v != "keep" {
				d.ProfileID, _ = strconv.ParseInt(v, 10, 64)
			}
			if v := r.FormValue("credential_id"); v != "" && v != "keep" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
					d.CredentialID = n
				}
			}
			if r.FormValue("set_group") == "1" {
				d.Group = strings.TrimSpace(r.FormValue("group"))
			}
			if err := s.st.SaveDevice(&d); err == nil {
				s.en.Reload(id)
			}
		}
	}
	details := strings.Join(names, ", ")
	if action == "profile" || action == "edit" {
		details = s.bulkEditSummary(r) + ": " + details
	}
	s.audit(r, "bulk "+action, fmt.Sprintf("%d devices", len(names)), details)
	msgs := map[string]string{"refresh": "Full discovery started", "enable": "Enabled", "disable": "Disabled", "delete": "Deleted", "profile": "Profile assigned", "edit": "Changed"}
	redirect(w, r, "/devices", fmt.Sprintf("%s: %d devices", msgs[action], len(ids)))
}

func selectionChanges(before []store.Object, sel map[int64]bool) string {
	var on, off []string
	for _, o := range before {
		switch {
		case sel[o.ID] && !o.Selected:
			on = append(on, o.Name)
		case !sel[o.ID] && o.Selected:
			off = append(off, o.Name)
		}
	}
	var parts []string
	if len(on) > 0 {
		parts = append(parts, "+ "+shortList(on))
	}
	if len(off) > 0 {
		parts = append(parts, "− "+shortList(off))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, "; ")
}

func shortList(names []string) string {
	const maxNames = 20
	if len(names) <= maxNames {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxNames], ", ") + fmt.Sprintf(" and %d more", len(names)-maxNames)
}

// bulkEditSummary names the fields a bulk edit changes, for the change log.
func (s *Server) bulkEditSummary(r *http.Request) string {
	var parts []string
	if v := r.FormValue("profile_id"); v != "" && v != "keep" {
		id, _ := strconv.ParseInt(v, 10, 64)
		parts = append(parts, "profile "+s.profileName(id))
	}
	if v := r.FormValue("credential_id"); v != "" && v != "keep" {
		id, _ := strconv.ParseInt(v, 10, 64)
		name := fmt.Sprint(id)
		if creds, err := s.st.Credentials(); err == nil {
			for _, c := range creds {
				if c.ID == id {
					name = c.Name
				}
			}
		}
		parts = append(parts, "credentials "+name)
	}
	if r.FormValue("set_group") == "1" {
		g := strings.TrimSpace(r.FormValue("group"))
		if g == "" {
			g = "(none)"
		}
		parts = append(parts, "group "+g)
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
