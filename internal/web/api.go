package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

type actorKey struct{}

// actor names who made a request: the UI user, or "api:<token name>".
func (s *Server) actor(r *http.Request) string {
	if a, ok := r.Context().Value(actorKey{}).(string); ok {
		return a
	}
	return s.sessionUser(r)
}

// registerAPI adds the REST API. Every call needs "Authorization: Bearer
// <token>"; viewer tokens may only read.
func (s *Server) registerAPI(mux *http.ServeMux) {
	read := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.apiAuth(false, h)) }
	write := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.apiAuth(true, h)) }
	read("GET /api/v1/devices", s.apiDevices)
	read("GET /api/v1/devices/{id}", s.apiDevice)
	write("POST /api/v1/devices", s.apiDeviceCreate)
	write("PATCH /api/v1/devices/{id}", s.apiDeviceUpdate)
	write("DELETE /api/v1/devices/{id}", s.apiDeviceDelete)
	write("POST /api/v1/devices/{id}/refresh", s.apiDeviceRefresh)
	write("POST /api/v1/devices/{id}/selection", s.apiDeviceSelection)
	read("GET /api/v1/profiles", s.apiProfiles)
	read("GET /api/v1/credentials", s.apiCredentials)
}

func (s *Server) apiAuth(write bool, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(tok, tokenPrefix) {
			apiError(w, http.StatusUnauthorized, "missing API token: send Authorization: Bearer zx_...")
			return
		}
		t, err := s.st.APITokenByHash(hashToken(strings.TrimSpace(tok)))
		if err != nil {
			apiError(w, http.StatusUnauthorized, "unknown or revoked API token")
			return
		}
		if write && t.Role != store.RoleAdmin {
			apiError(w, http.StatusForbidden, "read-only token")
			return
		}
		s.st.TouchAPIToken(t.ID)
		h(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, "api:"+t.Name)))
	})
}

func apiJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	apiJSON(w, code, map[string]string{"error": msg})
}

// APIPoll is the last poll result of a device.
type APIPoll struct {
	OK         bool      `json:"ok"`
	At         time.Time `json:"at"`
	DurationMS int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
	Samples    int       `json:"samples"`
}

// APIDevice is a device as the API shows it.
type APIDevice struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	Address           string     `json:"address"`
	Port              int        `json:"port"`
	Group             string     `json:"group"`
	Enabled           bool       `json:"enabled"`
	Profile           string     `json:"profile"`
	Credential        string     `json:"credential"`
	IntervalSec       int        `json:"interval_sec"`
	EffectiveInterval int        `json:"effective_interval_sec"`
	SysName           string     `json:"sys_name"`
	SysDescr          string     `json:"sys_descr"`
	LastDiscovery     *time.Time `json:"last_discovery"`
	DiscoveryError    string     `json:"discovery_error,omitempty"`
	Discovering       bool       `json:"discovering"`
	Selected          int        `json:"selected_objects"`
	Total             int        `json:"total_objects"`
	Poll              *APIPoll   `json:"last_poll"`
	Objects           []APIObj   `json:"objects,omitempty"`
}

// APIObj is a discovered port or health object.
type APIObj struct {
	ID         int64  `json:"id"`
	Category   string `json:"category"`
	Kind       string `json:"kind"`
	Key        string `json:"key"`
	Index      string `json:"index"`
	Name       string `json:"name"`
	Descr      string `json:"descr,omitempty"`
	Alias      string `json:"alias,omitempty"`
	SpeedMbps  int64  `json:"speed_mbps,omitempty"`
	OperStatus int    `json:"oper_status,omitempty"`
	Selected   bool   `json:"selected"`
	State      string `json:"state"`
}

func apiDevice(row DeviceRow) APIDevice {
	d := APIDevice{
		ID: row.ID, Name: row.Name, Address: row.Address, Port: row.Port, Group: row.Group, Enabled: row.Enabled,
		Profile: row.Profile, Credential: row.Credential, IntervalSec: row.IntervalSec,
		EffectiveInterval: int(row.Eff.Interval / time.Second), SysName: row.SysName, SysDescr: row.SysDescr,
		DiscoveryError: row.DiscoveryError, Discovering: row.Discovering, Selected: row.Selected, Total: row.Total,
	}
	if d.Profile == "" {
		d.Profile = "global"
	}
	if !row.LastDiscovery.IsZero() {
		t := row.LastDiscovery
		d.LastDiscovery = &t
	}
	if sn := row.Snap; sn != nil {
		d.Poll = &APIPoll{OK: sn.OK, At: sn.At, DurationMS: sn.Duration.Milliseconds(), Error: sn.Err, Samples: len(sn.Samples)}
	}
	return d
}

func (s *Server) apiDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deviceRows()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	group := r.URL.Query().Get("group")
	out := make([]APIDevice, 0, len(rows))
	for _, row := range rows {
		if group == "" || row.Group == group {
			out = append(out, apiDevice(row))
		}
	}
	apiJSON(w, http.StatusOK, out)
}

func (s *Server) apiRow(id int64) (DeviceRow, bool) {
	rows, err := s.deviceRows()
	if err != nil {
		return DeviceRow{}, false
	}
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return DeviceRow{}, false
}

func (s *Server) apiDevice(w http.ResponseWriter, r *http.Request) {
	row, ok := s.apiRow(pathID(r))
	if !ok {
		apiError(w, http.StatusNotFound, "no such device")
		return
	}
	d := apiDevice(row)
	objs, err := s.st.Objects(row.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.Objects = make([]APIObj, 0, len(objs))
	for _, o := range objs {
		d.Objects = append(d.Objects, APIObj{
			ID: o.ID, Category: snmpx.CategoryOf(o.Kind), Kind: o.Kind, Key: o.Key, Index: o.Idx, Name: o.Name,
			Descr: o.Descr, Alias: o.Alias, SpeedMbps: o.SpeedMbps, OperStatus: o.OperStatus, Selected: o.Selected, State: o.State,
		})
	}
	apiJSON(w, http.StatusOK, d)
}

// apiDeviceInput is the body of create and update calls. Pointers tell
// "not given" from zero values on update.
type apiDeviceInput struct {
	Name        *string `json:"name"`
	Address     *string `json:"address"`
	Port        *int    `json:"port"`
	Group       *string `json:"group"`
	Enabled     *bool   `json:"enabled"`
	Profile     *string `json:"profile"`
	Credential  *string `json:"credential"`
	Version     string  `json:"version"`
	Community   string  `json:"community"`
	IntervalSec *int    `json:"interval_sec"`
}

func decodeInput(r *http.Request) (apiDeviceInput, error) {
	var in apiDeviceInput
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("bad JSON: %w", err)
	}
	return in, nil
}

// apply copies the given fields onto d and resolves profile and credentials by name.
func (s *Server) apply(in apiDeviceInput, d *store.Device) error {
	if in.Name != nil {
		d.Name = strings.TrimSpace(*in.Name)
	}
	if in.Address != nil {
		d.Address = strings.TrimSpace(*in.Address)
	}
	if in.Port != nil {
		d.Port = *in.Port
	}
	if in.Group != nil {
		d.Group = strings.TrimSpace(*in.Group)
	}
	if in.Enabled != nil {
		d.Enabled = *in.Enabled
	}
	if in.IntervalSec != nil {
		d.IntervalSec = max(*in.IntervalSec, 0)
	}
	if in.Profile != nil {
		switch p := strings.TrimSpace(*in.Profile); p {
		case "", "global":
			d.ProfileID = 0
		default:
			prof, err := s.st.ProfileByName(p)
			if err != nil {
				return fmt.Errorf("no profile %q", p)
			}
			d.ProfileID = prof.ID
		}
	}
	if (in.Credential != nil && *in.Credential != "") || in.Community != "" {
		name := ""
		if in.Credential != nil {
			name = *in.Credential
		}
		c, err := s.importCredential(name, in.Version, in.Community, "", d.Name)
		if err != nil {
			return err
		}
		d.CredentialID = c.ID
	}
	if d.Address == "" {
		return errors.New("address is required")
	}
	if d.CredentialID == 0 {
		return errors.New("give credential (a saved name) or version + community")
	}
	if d.Name == "" {
		d.Name = d.Address
	}
	return nil
}

func saveErr(err error) string {
	if strings.Contains(err.Error(), "UNIQUE") {
		return "a device with this name already exists"
	}
	return err.Error()
}

func (s *Server) apiDeviceCreate(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	d := store.Device{Enabled: true}
	if err := s.apply(in, &d); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.SaveDevice(&d); err != nil {
		apiError(w, http.StatusConflict, saveErr(err))
		return
	}
	s.audit(r, "device added", d.Name, d.Address)
	if d.Enabled {
		s.en.Refresh(d.ID)
	}
	row, _ := s.apiRow(d.ID)
	apiJSON(w, http.StatusCreated, apiDevice(row))
}

func (s *Server) apiDeviceUpdate(w http.ResponseWriter, r *http.Request) {
	before, err := s.st.Device(pathID(r))
	if err != nil {
		apiError(w, http.StatusNotFound, "no such device")
		return
	}
	in, err := decodeInput(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	d := before
	if err := s.apply(in, &d); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.SaveDevice(&d); err != nil {
		apiError(w, http.StatusConflict, saveErr(err))
		return
	}
	if ch := s.deviceChanges(before, d); ch != "" || before.Enabled != d.Enabled {
		if before.Enabled != d.Enabled {
			ch = strings.TrimPrefix(ch+fmt.Sprintf("; enabled: %v → %v", before.Enabled, d.Enabled), "; ")
		}
		s.audit(r, "device changed", d.Name, ch)
	}
	if d.Enabled && (before.Address != d.Address || before.Port != d.Port || before.CredentialID != d.CredentialID || d.LastDiscovery.IsZero()) {
		s.en.Refresh(d.ID)
	} else {
		s.en.Reload(d.ID)
	}
	row, _ := s.apiRow(d.ID)
	apiJSON(w, http.StatusOK, apiDevice(row))
}

func (s *Server) apiDeviceDelete(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		apiError(w, http.StatusNotFound, "no such device")
		return
	}
	if err := s.st.DeleteDevice(d.ID); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "device deleted", d.Name, d.Address)
	s.en.Reload(d.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiDeviceRefresh(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		apiError(w, http.StatusNotFound, "no such device")
		return
	}
	if !s.en.Refresh(d.ID) {
		apiJSON(w, http.StatusAccepted, map[string]string{"status": "already running"})
		return
	}
	s.audit(r, "refresh", d.Name, "")
	apiJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// apiDeviceSelection ticks and unticks objects by key (port name, sensor
// name) or by id. Objects not named keep their state.
func (s *Server) apiDeviceSelection(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		apiError(w, http.StatusNotFound, "no such device")
		return
	}
	var in struct {
		Select   []string `json:"select"`
		Unselect []string `json:"unselect"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	objs, err := s.st.Objects(d.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	match := func(o store.Object, names []string) bool {
		for _, n := range names {
			if n == o.Key || n == fmt.Sprint(o.ID) {
				return true
			}
		}
		return false
	}
	used := map[string]bool{}
	sel := map[int64]bool{}
	for _, o := range objs {
		on := o.Selected
		if match(o, in.Select) {
			on = true
			used[o.Key], used[fmt.Sprint(o.ID)] = true, true
		}
		if match(o, in.Unselect) {
			on = false
			used[o.Key], used[fmt.Sprint(o.ID)] = true, true
		}
		sel[o.ID] = on
	}
	var unknown []string
	for _, n := range append(in.Select, in.Unselect...) {
		if !used[n] {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		apiError(w, http.StatusBadRequest, "no such objects: "+strings.Join(unknown, ", "))
		return
	}
	if err := s.st.SaveSelection(d.ID, sel); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "selection changed", d.Name, selectionChanges(objs, sel))
	s.en.Reload(d.ID)
	n := 0
	for _, on := range sel {
		if on {
			n++
		}
	}
	apiJSON(w, http.StatusOK, map[string]int{"selected_objects": n})
}

func (s *Server) apiProfiles(w http.ResponseWriter, r *http.Request) {
	profs, err := s.st.Profiles()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type p struct {
		Name         string `json:"name"`
		IntervalSec  int    `json:"interval_sec"`
		TimeoutSec   int    `json:"timeout_sec"`
		Retries      int    `json:"retries"`
		AutoSelectUp bool   `json:"auto_select_up"`
	}
	out := []p{}
	for _, x := range profs {
		out = append(out, p{x.Name, x.IntervalSec, x.TimeoutSec, x.Retries, x.AutoSelectUp})
	}
	apiJSON(w, http.StatusOK, out)
}

// apiCredentials lists credential names and versions; secrets never leave the server.
func (s *Server) apiCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.st.Credentials()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type c struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		V3User  string `json:"v3_user,omitempty"`
	}
	out := []c{}
	for _, x := range creds {
		out = append(out, c{x.Name, x.Version, x.V3User})
	}
	apiJSON(w, http.StatusOK, out)
}
