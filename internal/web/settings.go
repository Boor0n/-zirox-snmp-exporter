package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// ---- poll profiles ----

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	profs, err := s.st.Profiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	devs, _ := s.st.Devices()
	uses := map[int64]int{}
	for _, d := range devs {
		uses[d.ProfileID]++
	}
	edit := store.PollProfile{IntervalSec: 60, TimeoutSec: 5, Retries: 1, AutoSelectUp: true}
	if id, _ := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64); id != 0 {
		if p, err := s.st.Profile(id); err == nil {
			edit = p
		}
	}
	s.render(w, r, "profiles", map[string]any{"Profiles": profs, "Uses": uses, "Edit": edit, "Global": s.st.Settings(), "Min": store.MinIntervalSec})
}

func (s *Server) profileSave(w http.ResponseWriter, r *http.Request) {
	p := store.PollProfile{
		Name:         strings.TrimSpace(r.FormValue("name")),
		IntervalSec:  formInt(r, "interval_sec"),
		TimeoutSec:   formInt(r, "timeout_sec"),
		Retries:      formInt(r, "retries"),
		AutoSelectUp: r.FormValue("auto_select_up") == "on",
	}
	p.ID, _ = strconv.ParseInt(r.FormValue("id"), 10, 64)
	if p.Name == "" {
		redirectErr(w, r, "/profiles", errors.New("enter a profile name"))
		return
	}
	isNew := p.ID == 0
	if err := s.st.SaveProfile(&p); err != nil {
		redirectErr(w, r, "/profiles", err)
		return
	}
	action := "profile changed"
	if isNew {
		action = "profile added"
	}
	s.audit(r, action, p.Name, fmt.Sprintf("interval %d s, timeout %d s, retries %d, auto-select up %v", p.IntervalSec, p.TimeoutSec, p.Retries, p.AutoSelectUp))
	_ = s.en.ReloadAll()
	redirect(w, r, "/profiles", "Profile saved and applied")
}

func (s *Server) profileDelete(w http.ResponseWriter, r *http.Request) {
	name := s.profileName(pathID(r))
	if err := s.st.DeleteProfile(pathID(r)); err != nil {
		redirectErr(w, r, "/profiles", err)
		return
	}
	s.audit(r, "profile deleted", name, "")
	_ = s.en.ReloadAll()
	redirect(w, r, "/profiles", "Profile deleted; its devices now use the global settings")
}

// ---- settings ----

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings", map[string]any{"Settings": s.st.Settings(), "Min": store.MinIntervalSec, "Admin": s.sessionUser(r), "MaxDiscovery": s.en.MaxConcurrentDiscovery()})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	st := store.Settings{
		IntervalSec:  formInt(r, "interval_sec"),
		TimeoutSec:   formInt(r, "timeout_sec"),
		Retries:      formInt(r, "retries"),
		AutoSelectUp: r.FormValue("auto_select_up") == "on",

		AutoRefreshHours: formInt(r, "auto_refresh_hours"),
		AutoRefreshFrom:  formInt(r, "auto_refresh_from"),
		AutoRefreshTo:    formInt(r, "auto_refresh_to"),
		FastIntervalSec:  formInt(r, "fast_interval_sec"),
		AutoUplink:       r.FormValue("auto_uplink") == "on",
	}
	if err := s.st.SaveSettings(st); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	st = s.st.Settings()
	refresh := "off"
	if st.AutoRefreshHours > 0 {
		refresh = fmt.Sprintf("every %d h, %02d:00-%02d:00", st.AutoRefreshHours, st.AutoRefreshFrom, st.AutoRefreshTo)
	}
	s.audit(r, "settings changed", "global", fmt.Sprintf("interval %d s, timeout %d s, retries %d, auto-select up %v, uplinks %d s, neighbours in Zirox are uplinks %v, scheduled refresh %s",
		st.IntervalSec, st.TimeoutSec, st.Retries, st.AutoSelectUp, st.FastIntervalSec, st.AutoUplink, refresh))
	_ = s.en.ReloadAll()
	redirect(w, r, "/settings", "Global settings applied")
}

func (s *Server) passwordSave(w http.ResponseWriter, r *http.Request) {
	u, ok := s.checkPassword(s.sessionUser(r), r.FormValue("current"))
	if !ok {
		redirectErr(w, r, "/settings", errors.New("current password is wrong"))
		return
	}
	pw := r.FormValue("new")
	if err := checkNewPassword(pw, r.FormValue("confirm")); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	if err := setPassword(&u, pw); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	if err := s.st.SaveUser(&u); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	s.setSession(w, u)
	s.audit(r, "password changed", u.Name, "")
	redirect(w, r, "/settings", "Password changed")
}

// ---- Prometheus help ----

func (s *Server) prometheusHelp(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	s.render(w, r, "prometheus", map[string]any{"Base": scheme + "://" + r.Host, "Host": r.Host,
		"HTTPS": r.TLS != nil, "SelfSigned": s.cfg.TLSSelfSigned, "Fingerprint": s.cfg.TLSFingerprint, "NotAfter": s.cfg.TLSNotAfter})
}

// selectionChanges lists the objects ticked and unticked by a selection save.
