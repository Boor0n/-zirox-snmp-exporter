package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// reloadCustom hands the current custom OIDs to discovery and polling.
func (s *Server) reloadCustom() error {
	defs, err := s.st.CustomOIDs()
	if err != nil {
		return err
	}
	snmpx.SetCustom(defs)
	return nil
}

// customOIDs lists the user's OIDs, the add/edit form and the OID checker.
func (s *Server) customOIDs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	edit := store.CustomOID{Mode: store.CustomTable, Scale: 1, AutoSelect: true}
	if id, _ := strconv.ParseInt(q.Get("edit"), 10, 64); id != 0 {
		if c, err := s.st.CustomOID(id); err == nil {
			edit = c
		}
	} else if oid := q.Get("use"); oid != "" {
		// "Add" from the checker: prefill the form.
		edit.OID = oid
		if q.Get("mode") == store.CustomScalar {
			edit.Mode = store.CustomScalar
		}
	}
	s.customPage(w, r, edit, "")
}

// customPage renders the page; formErr keeps the user's input on a failed save.
func (s *Server) customPage(w http.ResponseWriter, r *http.Request, edit store.CustomOID, formErr string) {
	defs, err := s.st.CustomOIDs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	uses := map[int64]int{}
	for _, c := range defs {
		uses[c.ID] = s.st.CustomObjectCount(c.ID)
	}
	q := r.URL.Query()
	devs, _ := s.st.Devices()
	data := map[string]any{"Defs": defs, "Uses": uses, "Edit": edit, "Devices": devs, "FormErr": formErr,
		"CheckDevice": int64(0), "CheckOID": "", "CheckWalk": false,
		"Scalar": store.CustomScalar, "Table": store.CustomTable, "Limit": snmpx.LookupLimit}

	// OID checker: read a value or walk a subtree on one device.
	if q.Get("check") != "" {
		devID, _ := strconv.ParseInt(q.Get("device"), 10, 64)
		walk := q.Get("walk") == "1"
		data["CheckDevice"], data["CheckOID"], data["CheckWalk"] = devID, q.Get("oid"), walk
		oid, err := snmpx.NormalizeOID(q.Get("oid"))
		var d store.Device
		if err == nil {
			d, err = s.st.Device(devID)
			if err != nil {
				err = fmt.Errorf("choose a device")
			}
		}
		if err == nil {
			data["CheckOID"] = oid
			var rows []snmpx.LookupRow
			var more bool
			rows, more, err = s.en.Lookup(d, oid, walk)
			data["Rows"], data["More"] = rows, more
			if err == nil && len(rows) == 0 {
				err = fmt.Errorf("%s returned nothing for %s; try \"Walk\" for a table column or a subtree", d.Name, oid)
			}
		}
		if err != nil {
			data["CheckErr"] = err.Error()
		}
	}
	s.render(w, r, "custom_oids", data)
}

func (s *Server) customOIDSave(w http.ResponseWriter, r *http.Request) {
	c := store.CustomOID{
		Name:       r.FormValue("name"),
		Help:       r.FormValue("help"),
		OID:        r.FormValue("oid"),
		Mode:       r.FormValue("mode"),
		NameOID:    r.FormValue("name_oid"),
		Counter:    r.FormValue("type") == "counter",
		AutoSelect: r.FormValue("auto_select") == "on",
		Scale:      1,
	}
	c.ID, _ = strconv.ParseInt(r.FormValue("id"), 10, 64)
	if v := strings.TrimSpace(r.FormValue("scale")); v != "" {
		f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64)
		if err != nil || f == 0 {
			s.customPage(w, r, c, fmt.Sprintf("multiplier %q is not a non-zero number", v))
			return
		}
		c.Scale = f
	}
	if err := snmpx.CheckCustom(&c); err != nil {
		s.customPage(w, r, c, err.Error())
		return
	}
	isNew := c.ID == 0
	if err := s.st.SaveCustomOID(&c); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			err = fmt.Errorf("metric %s already exists", c.Name)
		}
		s.customPage(w, r, c, err.Error())
		return
	}
	if err := s.reloadCustom(); err != nil {
		redirectErr(w, r, "/custom-oids", err)
		return
	}
	action := "custom OID changed"
	if isNew {
		action = "custom OID added"
	}
	details := c.OID + ", " + c.Mode
	if c.NameOID != "" {
		details += ", names from " + c.NameOID
	}
	if c.Counter {
		details += ", counter"
	}
	if c.Scale != 1 {
		details += ", × " + strconv.FormatFloat(c.Scale, 'g', -1, 64)
	}
	s.audit(r, action, c.Name, details)
	redirect(w, r, "/custom-oids", "Saved. Devices pick it up on their next Refresh; use \"Refresh all devices\" to do it now.")
}

func (s *Server) customOIDDelete(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.CustomOID(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.st.DeleteCustomOID(c.ID); err != nil {
		redirectErr(w, r, "/custom-oids", err)
		return
	}
	if err := s.reloadCustom(); err != nil {
		redirectErr(w, r, "/custom-oids", err)
		return
	}
	s.audit(r, "custom OID deleted", c.Name, c.OID)
	_ = s.en.ReloadAll()
	redirect(w, r, "/custom-oids", "Deleted; it is no longer polled on any device")
}

// customRefreshAll starts a full discovery of every enabled device.
func (s *Server) customRefreshAll(w http.ResponseWriter, r *http.Request) {
	devs, err := s.st.Devices()
	if err != nil {
		redirectErr(w, r, "/custom-oids", err)
		return
	}
	n := 0
	for _, d := range devs {
		if d.Enabled && s.en.Refresh(d.ID) {
			n++
		}
	}
	s.audit(r, "refresh all devices", "", fmt.Sprintf("%d devices", n))
	redirect(w, r, "/custom-oids", fmt.Sprintf("Full discovery started on %d devices (at most %d at a time)", n, s.en.MaxConcurrentDiscovery()))
}
