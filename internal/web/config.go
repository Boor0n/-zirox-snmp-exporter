package web

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// configFormat is the version of the exported file layout.
const configFormat = 1

// Setup is everything a Zirox installation is set up with, without the
// metrics and, unless asked, without secrets. It can be kept in git and
// imported on another server.
type Setup struct {
	Format      int            `yaml:"zirox_config"`
	ExportedAt  string         `yaml:"exported_at,omitempty"`
	Version     string         `yaml:"zirox_version,omitempty"`
	Settings    *cfgSettings   `yaml:"settings,omitempty"`
	Credentials []cfgCred      `yaml:"credentials,omitempty"`
	Profiles    []cfgProfile   `yaml:"profiles,omitempty"`
	PortRules   *[]cfgRule     `yaml:"port_rules,omitempty"`
	CustomOIDs  []cfgCustomOID `yaml:"custom_oids,omitempty"`
	Devices     []cfgDevice    `yaml:"devices,omitempty"`
}

type cfgSettings struct {
	IntervalSec      int  `yaml:"interval_sec"`
	TimeoutSec       int  `yaml:"timeout_sec"`
	Retries          int  `yaml:"retries"`
	AutoSelectUp     bool `yaml:"auto_select_up"`
	UplinkIntervalS  int  `yaml:"uplink_interval_sec"`
	AutoRefreshHours int  `yaml:"auto_refresh_hours"`
	AutoRefreshFrom  int  `yaml:"auto_refresh_from"`
	AutoRefreshTo    int  `yaml:"auto_refresh_to"`
	// AutoUplink is a pointer so files from before v1.0 keep it on.
	AutoUplink *bool `yaml:"neighbors_in_zirox_are_uplinks,omitempty"`
}

type cfgCred struct {
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`
	Community string `yaml:"community,omitempty"`
	V3User    string `yaml:"v3_user,omitempty"`
	V3Level   string `yaml:"v3_level,omitempty"`
	V3Auth    string `yaml:"v3_auth_protocol,omitempty"`
	V3AuthPw  string `yaml:"v3_auth_password,omitempty"`
	V3Priv    string `yaml:"v3_priv_protocol,omitempty"`
	V3PrivPw  string `yaml:"v3_priv_password,omitempty"`
	V3Context string `yaml:"v3_context,omitempty"`
}

type cfgProfile struct {
	Name         string `yaml:"name"`
	IntervalSec  int    `yaml:"interval_sec"`
	TimeoutSec   int    `yaml:"timeout_sec"`
	Retries      int    `yaml:"retries"`
	AutoSelectUp bool   `yaml:"auto_select_up"`
}

type cfgRule struct {
	Field   string `yaml:"field"`
	Pattern string `yaml:"pattern"`
	Group   string `yaml:"group,omitempty"`
	Action  string `yaml:"action,omitempty"`
	Role    string `yaml:"role,omitempty"`
}

type cfgCustomOID struct {
	Name       string  `yaml:"name"`
	Help       string  `yaml:"help,omitempty"`
	OID        string  `yaml:"oid"`
	Mode       string  `yaml:"mode"`
	NameOID    string  `yaml:"name_oid,omitempty"`
	Counter    bool    `yaml:"counter,omitempty"`
	Scale      float64 `yaml:"scale,omitempty"`
	AutoSelect bool    `yaml:"auto_select,omitempty"`
}

type cfgDevice struct {
	Name        string `yaml:"name"`
	Address     string `yaml:"address"`
	Port        int    `yaml:"port,omitempty"`
	Group       string `yaml:"group,omitempty"`
	Enabled     *bool  `yaml:"enabled,omitempty"`
	Credential  string `yaml:"credential"`
	Profile     string `yaml:"profile,omitempty"`
	IntervalSec int    `yaml:"interval_sec,omitempty"`
	// Polled lists the ticked objects as "kind:key"; nil leaves ticks alone.
	Polled []string `yaml:"polled,omitempty"`
	// Roles maps "kind:key" to a port role.
	Roles map[string]string `yaml:"roles,omitempty"`
}

// objRef names an object across installations: custom OID objects use the
// OID's name, since IDs differ between servers.
func objRef(o store.Object, customName map[string]string) string {
	kind := o.Kind
	if n, ok := customName[o.Kind]; ok {
		kind = "custom:" + n
	}
	return kind + ":" + o.Key
}

// parseRef splits "kind:key"; kinds never contain ':' except "custom:<name>".
func parseRef(ref string, customKind map[string]string) (kind, key string, ok bool) {
	if strings.HasPrefix(ref, "custom:") {
		rest := strings.TrimPrefix(ref, "custom:")
		i := strings.Index(rest, ":")
		if i < 0 {
			return "", "", false
		}
		k, found := customKind[rest[:i]]
		return k, rest[i+1:], found
	}
	i := strings.Index(ref, ":")
	if i <= 0 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// exportConfig builds the configuration; withSecrets adds plain-text
// communities and v3 passwords.
func (s *Server) exportConfig(withSecrets bool) (*Setup, error) {
	c := &Setup{Format: configFormat, ExportedAt: time.Now().UTC().Format(time.RFC3339), Version: s.cfg.Version}
	g := s.st.Settings()
	c.Settings = &cfgSettings{g.IntervalSec, g.TimeoutSec, g.Retries, g.AutoSelectUp, g.FastIntervalSec, g.AutoRefreshHours, g.AutoRefreshFrom, g.AutoRefreshTo, &g.AutoUplink}

	creds, err := s.st.Credentials()
	if err != nil {
		return nil, err
	}
	credName := map[int64]string{}
	for _, cr := range creds {
		credName[cr.ID] = cr.Name
		x := cfgCred{Name: cr.Name, Version: cr.Version, V3User: cr.V3User, V3Level: cr.V3Level, V3Auth: cr.V3AuthProto, V3Priv: cr.V3PrivProto, V3Context: cr.V3Context}
		if withSecrets {
			p, err := s.en.DecryptCredential(cr)
			if err != nil {
				return nil, fmt.Errorf("credentials %s: %w", cr.Name, err)
			}
			x.Community, x.V3AuthPw, x.V3PrivPw = p.Community, p.V3AuthPass, p.V3PrivPass
		}
		c.Credentials = append(c.Credentials, x)
	}
	profs, err := s.st.Profiles()
	if err != nil {
		return nil, err
	}
	profName := map[int64]string{}
	for _, p := range profs {
		profName[p.ID] = p.Name
		c.Profiles = append(c.Profiles, cfgProfile{p.Name, p.IntervalSec, p.TimeoutSec, p.Retries, p.AutoSelectUp})
	}
	rules, err := s.st.PortRules()
	if err != nil {
		return nil, err
	}
	rl := []cfgRule{}
	for _, r := range rules {
		rl = append(rl, cfgRule{r.Field, r.Pattern, r.Group, r.Action, r.Role})
	}
	c.PortRules = &rl
	customs, err := s.st.CustomOIDs()
	if err != nil {
		return nil, err
	}
	customName := map[string]string{}
	for _, x := range customs {
		customName[x.Kind()] = x.Name
		c.CustomOIDs = append(c.CustomOIDs, cfgCustomOID{x.Name, x.Help, x.OID, x.Mode, x.NameOID, x.Counter, x.Scale, x.AutoSelect})
	}
	devs, err := s.st.Devices()
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		en := d.Enabled
		x := cfgDevice{Name: d.Name, Address: d.Address, Port: d.Port, Group: d.Group, Enabled: &en,
			Credential: credName[d.CredentialID], Profile: profName[d.ProfileID], IntervalSec: d.IntervalSec}
		if x.Port == 161 {
			x.Port = 0
		}
		objs, err := s.st.Objects(d.ID)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			if o.State == store.StateMissing {
				continue
			}
			ref := objRef(o, customName)
			if o.Selected {
				x.Polled = append(x.Polled, ref)
			}
			if o.Role != "" {
				if x.Roles == nil {
					x.Roles = map[string]string{}
				}
				x.Roles[ref] = o.Role
			}
		}
		c.Devices = append(c.Devices, x)
	}
	return c, nil
}

func (s *Server) configExport(w http.ResponseWriter, r *http.Request) {
	withSecrets := r.URL.Query().Get("secrets") == "1"
	c, err := s.exportConfig(withSecrets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	note := "secrets left out"
	if withSecrets {
		note = "WITH plain-text secrets"
	}
	s.audit(r, "configuration exported", "configuration", note)
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="zirox-config-%s.yaml"`, time.Now().Format("20060102-1504")))
	fmt.Fprintf(w, "# Zirox configuration, %s.\n# Import it under Settings → Configuration. Devices match by name, the rest by name;\n# port_rules replace the existing rules. 'polled' lists the ticked objects.\n", note)
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	_ = enc.Encode(c)
}

// importResult describes what an import did or would do.
type importResult struct {
	Lines    []string // one line per change
	Warnings []string
	Errors   []string
	Refresh  []int64 // devices to discover
	Reload   []int64
}

func (ir *importResult) add(format string, a ...any) {
	ir.Lines = append(ir.Lines, fmt.Sprintf(format, a...))
}
func (ir *importResult) warn(format string, a ...any) {
	ir.Warnings = append(ir.Warnings, fmt.Sprintf(format, a...))
}
func (ir *importResult) fail(format string, a ...any) {
	ir.Errors = append(ir.Errors, fmt.Sprintf(format, a...))
}

// importConfig applies a configuration; with apply false it only reports
// what would change.
func (s *Server) importConfig(c *Setup, apply bool) (*importResult, error) {
	ir := &importResult{}
	if c.Format != configFormat {
		return nil, fmt.Errorf("not a Zirox configuration (zirox_config: %d, want %d)", c.Format, configFormat)
	}
	if c.Settings != nil {
		ir.add("Global settings: interval %d s, uplinks %d s", c.Settings.IntervalSec, c.Settings.UplinkIntervalS)
		if apply {
			x := c.Settings
			if err := s.st.SaveSettings(store.Settings{IntervalSec: x.IntervalSec, TimeoutSec: x.TimeoutSec, Retries: x.Retries, AutoSelectUp: x.AutoSelectUp,
				FastIntervalSec: x.UplinkIntervalS, AutoRefreshHours: x.AutoRefreshHours, AutoRefreshFrom: x.AutoRefreshFrom, AutoRefreshTo: x.AutoRefreshTo,
				AutoUplink: x.AutoUplink == nil || *x.AutoUplink}); err != nil {
				return nil, err
			}
		}
	}

	// Credentials by name; secrets only when the file has them.
	credID := map[string]int64{}
	if creds, err := s.st.Credentials(); err == nil {
		for _, cr := range creds {
			credID[cr.Name] = cr.ID
		}
	}
	for _, x := range c.Credentials {
		if x.Name == "" {
			continue
		}
		id, exists := credID[x.Name]
		cr := store.Credential{ID: id, Name: x.Name, Version: x.Version, V3User: x.V3User, V3Level: x.V3Level,
			V3AuthProto: x.V3Auth, V3PrivProto: x.V3Priv, V3Context: x.V3Context,
			Community: x.Community, V3AuthPass: x.V3AuthPw, V3PrivPass: x.V3PrivPw}
		hasSecret := x.Community != "" || x.V3AuthPw != "" || x.V3PrivPw != ""
		if exists {
			if !hasSecret {
				continue // keep the stored credentials as they are
			}
			old, err := s.st.Credential(id)
			if err != nil {
				return nil, err
			}
			plain, err := s.en.DecryptCredential(old)
			if err != nil {
				return nil, err
			}
			if cr.Community == "" {
				cr.Community = plain.Community
			}
			if cr.V3AuthPass == "" {
				cr.V3AuthPass = plain.V3AuthPass
			}
			if cr.V3PrivPass == "" {
				cr.V3PrivPass = plain.V3PrivPass
			}
			ir.add("Credentials %s: updated with the secrets from the file", x.Name)
		} else {
			ir.add("Credentials %s: new (SNMP v%s)", x.Name, x.Version)
			if !hasSecret {
				ir.warn("Credentials %s have no community or password in the file; fill them in under Credentials, the devices using them will not answer until then", x.Name)
			}
		}
		if hasSecret {
			if err := validateCred(cr); err != nil {
				ir.fail("Credentials %s: %v", x.Name, err)
				continue
			}
		}
		if apply {
			if err := s.encrypt(&cr); err != nil {
				return nil, err
			}
			if err := s.st.SaveCredential(&cr); err != nil {
				ir.fail("Credentials %s: %v", x.Name, err)
				continue
			}
			credID[x.Name] = cr.ID
		} else if !exists {
			credID[x.Name] = -1
		}
	}

	// Profiles by name.
	profID := map[string]int64{}
	if profs, err := s.st.Profiles(); err == nil {
		for _, p := range profs {
			profID[p.Name] = p.ID
		}
	}
	for _, x := range c.Profiles {
		if x.Name == "" {
			continue
		}
		id, exists := profID[x.Name]
		p := store.PollProfile{ID: id, Name: x.Name, IntervalSec: x.IntervalSec, TimeoutSec: x.TimeoutSec, Retries: x.Retries, AutoSelectUp: x.AutoSelectUp}
		if exists {
			ir.add("Profile %s: %d s", x.Name, x.IntervalSec)
		} else {
			ir.add("Profile %s: new, %d s", x.Name, x.IntervalSec)
		}
		if apply {
			if err := s.st.SaveProfile(&p); err != nil {
				ir.fail("Profile %s: %v", x.Name, err)
				continue
			}
			profID[x.Name] = p.ID
		} else if !exists {
			profID[x.Name] = -1
		}
	}

	// Port rules replace the current list when the file has the key.
	if c.PortRules != nil {
		var rs []store.PortRule
		for _, x := range *c.PortRules {
			r := store.PortRule{Field: x.Field, Pattern: x.Pattern, Group: x.Group, Action: x.Action, Role: x.Role}
			if err := r.Compile(); err != nil {
				ir.fail("Port rule /%s/: %v", x.Pattern, err)
				continue
			}
			rs = append(rs, r)
		}
		ir.add("Port rules: replaced by the %d rules of the file", len(rs))
		if apply && len(ir.Errors) == 0 {
			old, _ := s.st.PortRules()
			for _, r := range old {
				if err := s.st.DeletePortRule(r.ID); err != nil {
					return nil, err
				}
			}
			for i := range rs {
				if err := s.st.SavePortRule(&rs[i]); err != nil {
					ir.fail("Port rule /%s/: %v", rs[i].Pattern, err)
				}
			}
		}
	}

	// Custom OIDs by metric name.
	customKind := map[string]string{}
	if cs, err := s.st.CustomOIDs(); err == nil {
		for _, x := range cs {
			customKind[x.Name] = x.Kind()
		}
	}
	existingCustom := map[string]int64{}
	if cs, err := s.st.CustomOIDs(); err == nil {
		for _, x := range cs {
			existingCustom[x.Name] = x.ID
		}
	}
	for _, x := range c.CustomOIDs {
		id, exists := existingCustom[x.Name]
		co := store.CustomOID{ID: id, Name: x.Name, Help: x.Help, OID: x.OID, Mode: x.Mode, NameOID: x.NameOID, Counter: x.Counter, Scale: x.Scale, AutoSelect: x.AutoSelect}
		if exists {
			ir.add("Custom OID %s: updated", x.Name)
		} else {
			ir.add("Custom OID %s: new", x.Name)
		}
		if apply {
			if err := s.st.SaveCustomOID(&co); err != nil {
				ir.fail("Custom OID %s: %v", x.Name, err)
				continue
			}
			customKind[x.Name] = co.Kind()
		}
	}

	// Devices by name, then by address.
	devs, err := s.st.Devices()
	if err != nil {
		return nil, err
	}
	byName, byAddr := map[string]store.Device{}, map[string]store.Device{}
	for _, d := range devs {
		byName[strings.ToLower(d.Name)] = d
		byAddr[d.Address] = d
	}
	for _, x := range c.Devices {
		if x.Name == "" || x.Address == "" {
			ir.fail("Device %q: name and address are required", x.Name)
			continue
		}
		cid, ok := credID[x.Credential]
		if !ok {
			ir.fail("Device %s: credentials %q are neither on this server nor in the file", x.Name, x.Credential)
			continue
		}
		var pid int64
		if x.Profile != "" {
			if pid, ok = profID[x.Profile]; !ok {
				ir.fail("Device %s: profile %q is neither on this server nor in the file", x.Name, x.Profile)
				continue
			}
		}
		d, exists := byName[strings.ToLower(x.Name)]
		if !exists {
			d, exists = byAddr[x.Address]
		}
		d.Name, d.Address, d.Group, d.CredentialID, d.ProfileID, d.IntervalSec = x.Name, x.Address, x.Group, cid, pid, x.IntervalSec
		d.Port = x.Port
		if d.Port == 0 {
			d.Port = 161
		}
		d.Enabled = x.Enabled == nil || *x.Enabled
		if exists {
			ir.add("Device %s (%s): updated%s", x.Name, x.Address, tickNote(x))
		} else {
			ir.add("Device %s (%s): new, full discovery after import%s", x.Name, x.Address, tickNote(x))
		}
		if !apply {
			continue
		}
		if err := s.st.SaveDevice(&d); err != nil {
			ir.fail("Device %s: %v", x.Name, err)
			continue
		}
		if err := s.applyDeviceTicks(d, x, customKind); err != nil {
			ir.fail("Device %s: %v", x.Name, err)
			continue
		}
		if exists && !d.LastDiscovery.IsZero() {
			ir.Reload = append(ir.Reload, d.ID)
		} else if d.Enabled {
			ir.Refresh = append(ir.Refresh, d.ID)
		}
	}
	return ir, nil
}

func tickNote(x cfgDevice) string {
	if x.Polled == nil && len(x.Roles) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d objects ticked, %d roles", len(x.Polled), len(x.Roles))
}

// applyDeviceTicks sets the ticks and roles of a device's known objects to
// the file's and keeps the rest as presets for its next discovery.
func (s *Server) applyDeviceTicks(d store.Device, x cfgDevice, customKind map[string]string) error {
	if x.Polled == nil && len(x.Roles) == 0 {
		return nil
	}
	want := map[string]*store.Preset{}
	get := func(ref string) *store.Preset {
		kind, key, ok := parseRef(ref, customKind)
		if !ok {
			return nil
		}
		k := store.PresetKey(kind, key)
		if want[k] == nil {
			want[k] = &store.Preset{Kind: kind, Key: key}
		}
		return want[k]
	}
	for _, ref := range x.Polled {
		if p := get(ref); p != nil {
			p.Selected = true
		}
	}
	for ref, role := range x.Roles {
		if p := get(ref); p != nil {
			p.Role = role
		}
	}
	objs, err := s.st.Objects(d.ID)
	if err != nil {
		return err
	}
	sel := map[int64]bool{}
	roles := map[int64]string{}
	for _, o := range objs {
		k := store.PresetKey(o.Kind, o.Key)
		p := want[k]
		if x.Polled != nil {
			sel[o.ID] = p != nil && p.Selected
		} else {
			sel[o.ID] = o.Selected
		}
		if p != nil && p.Role != o.Role {
			roles[o.ID] = p.Role
		}
		delete(want, k)
	}
	if len(objs) > 0 {
		if err := s.st.SaveSelection(d.ID, sel); err != nil {
			return err
		}
		if err := s.st.SetObjectRoles(d.ID, roles); err != nil {
			return err
		}
	}
	var rest []store.Preset
	for _, p := range want {
		rest = append(rest, *p)
	}
	return s.st.SetPresets(d.ID, rest)
}

func (s *Server) configPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "config", s.configData(nil))
}

func (s *Server) configData(m map[string]any) map[string]any {
	if m == nil {
		m = map[string]any{}
	}
	m["KeyInEnv"] = len(s.cfg.SecretKey) == 0
	return m
}

// configImport previews or applies an uploaded or pasted configuration.
func (s *Server) configImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		redirectErr(w, r, "/config", err)
		return
	}
	text := r.FormValue("yaml")
	if f, _, err := r.FormFile("file"); err == nil {
		b, err := io.ReadAll(io.LimitReader(f, 8<<20))
		f.Close()
		if err != nil {
			redirectErr(w, r, "/config", err)
			return
		}
		if len(b) > 0 {
			text = string(b)
		}
	}
	var c Setup
	if err := yaml.Unmarshal([]byte(text), &c); err != nil {
		s.render(w, r, "config", s.configData(map[string]any{"YAML": text, "ImportErr": "The file is not valid YAML: " + err.Error()}))
		return
	}
	apply := r.FormValue("action") == "apply"
	ir, err := s.importConfig(&c, false)
	if err != nil {
		s.render(w, r, "config", s.configData(map[string]any{"YAML": text, "ImportErr": err.Error()}))
		return
	}
	if !apply || len(ir.Errors) > 0 {
		s.render(w, r, "config", s.configData(map[string]any{"YAML": text, "Preview": ir}))
		return
	}
	if _, err := s.st.Backup("before-import"); err != nil {
		s.log.Warn("backup before import", "err", err)
	}
	ir, err = s.importConfig(&c, true)
	if err != nil {
		s.render(w, r, "config", s.configData(map[string]any{"YAML": text, "ImportErr": err.Error()}))
		return
	}
	for _, id := range ir.Reload {
		s.en.Reload(id)
	}
	for _, id := range ir.Refresh {
		s.en.Refresh(id)
	}
	_ = s.en.ReloadAll()
	s.audit(r, "configuration imported", "configuration", fmt.Sprintf("%d changes, %d warnings", len(ir.Lines), len(ir.Warnings)))
	msg := fmt.Sprintf("Imported: %d changes; %d devices are being discovered", len(ir.Lines), len(ir.Refresh))
	if len(ir.Warnings) > 0 {
		redirectWarn(w, r, "/config", msg, strings.Join(ir.Warnings, " "))
		return
	}
	redirect(w, r, "/config", msg)
}

// configBackup downloads the database and the secret key as one .tar.gz,
// everything needed to move Zirox to another server.
func (s *Server) configBackup(w http.ResponseWriter, r *http.Request) {
	tmp, err := os.MkdirTemp("", "zirox-backup-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmp)
	dbPath := filepath.Join(tmp, "zirox.db")
	if err := s.st.BackupTo(dbPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	db, err := os.ReadFile(dbPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r, "backup downloaded", "configuration", strconv.Itoa(len(db))+" bytes")
	now := time.Now()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="zirox-backup-%s.tar.gz"`, now.Format("20060102-1504")))
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	put := func(name string, data []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: now})
		_, _ = tw.Write(data)
	}
	put("zirox.db", db)
	if len(s.cfg.SecretKey) > 0 {
		put("secret.key", []byte(base64.StdEncoding.EncodeToString(s.cfg.SecretKey)+"\n"))
	}
	put("RESTORE.txt", []byte(restoreText(len(s.cfg.SecretKey) > 0)))
	_ = tw.Close()
	_ = gz.Close()
}

func restoreText(withKey bool) string {
	t := `Zirox backup

To restore on this or another server installed with get.sh:

  sudo docker stop zirox 2>/dev/null; sudo systemctl stop zirox 2>/dev/null
  sudo rm -f /var/lib/zirox/zirox.db-wal /var/lib/zirox/zirox.db-shm
  sudo tar -xzf zirox-backup-*.tar.gz -C /var/lib/zirox zirox.db secret.key
  sudo chown 10001:10001 /var/lib/zirox/zirox.db /var/lib/zirox/secret.key   # Docker install
  # (systemd install: sudo chown zirox:zirox ... instead)
  sudo docker start zirox 2>/dev/null || sudo systemctl start zirox

The data directory is /var/lib/zirox for get.sh installs (ZIROX_DATA_DIR otherwise).
`
	if !withKey {
		t += `
The secret key is set by ZIROX_SECRET_KEY on this server, so it is not in
this archive: set the same ZIROX_SECRET_KEY on the new server, or the stored
communities and passwords cannot be read.
`
	} else {
		t += `
secret.key decrypts the stored communities and passwords: keep this archive
as safe as the passwords themselves.
`
	}
	return t
}
