package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/engine"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// Attention is one line of "Needs attention" on the overview.
type Attention struct {
	Severity int // 0 = most urgent
	Device   store.Device
	What     string
	Detail   string
	Since    time.Time
	Link     string
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deviceRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	newCounts, _ := s.st.NewObjectCounts()
	var items []Attention
	counts := map[string]int{}
	now := time.Now()
	for _, d := range rows {
		if !d.Enabled {
			counts["disabled"]++
			continue
		}
		link := fmt.Sprintf("/devices/%d", d.ID)
		switch st := rowStatus(d); {
		case d.LastDiscovery.IsZero() && d.DiscoveryError != "":
			counts["discovery"]++
		default:
			counts[st]++
		}
		switch {
		case d.LastDiscovery.IsZero() && d.DiscoveryError != "":
			items = append(items, Attention{1, d.Device, "Never discovered", d.DiscoveryError, time.Time{}, link + "/diagnose"})
		case d.Snap != nil && !d.Snap.OK:
			items = append(items, Attention{0, d.Device, "Not answering", d.Snap.Err, d.Snap.DownSince, link + "/diagnose"})
		}
		if d.Snap == nil {
			continue
		}
		if d.Snap.OK && d.Snap.Duration > d.Snap.Interval*8/10 {
			counts["slow"]++
			items = append(items, Attention{2, d.Device, "Poll does not fit the interval",
				fmt.Sprintf("took %s of %s", d.Snap.Duration.Round(100*time.Millisecond), d.Snap.Interval), time.Time{}, link})
		}
		if d.Snap.Partial {
			items = append(items, Attention{2, d.Device, "SNMP packets lost", d.Snap.Err, time.Time{}, link})
		}
		if len(d.Snap.Shifted) > 0 {
			items = append(items, Attention{1, d.Device, "Port numbering changed", strings.Join(d.Snap.Shifted, ", "), time.Time{}, link})
		}
		if !d.Snap.LastReboot.IsZero() && now.Sub(d.Snap.LastReboot) < 24*time.Hour {
			items = append(items, Attention{3, d.Device, "Restarted", "", d.Snap.LastReboot, link})
		}
		if d.DiscoveryError != "" && !d.LastDiscovery.IsZero() {
			items = append(items, Attention{2, d.Device, "Last Refresh failed", d.DiscoveryError, time.Time{}, link})
		}
		if n := newCounts[d.ID]; n > 0 {
			items = append(items, Attention{3, d.Device, "New ports or sensors to review", fmt.Sprintf("%d found by the last Refresh", n), d.LastDiscovery, link})
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Severity != items[b].Severity {
			return items[a].Severity < items[b].Severity
		}
		return items[a].Device.Name < items[b].Device.Name
	})
	events, _ := s.st.AuditLog("", 25)
	s.render(w, r, "overview", map[string]any{
		"Total": len(rows), "Counts": counts, "Items": items, "Events": events,
	})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	data := map[string]any{"SearchQ": q}
	if q != "" {
		devs, _ := s.st.Devices()
		lq := strings.ToLower(q)
		var hits []store.Device
		for _, d := range devs {
			if strings.Contains(strings.ToLower(strings.Join([]string{d.Name, d.Address, d.SysName, d.Group, d.Model, d.Serial}, " ")), lq) {
				hits = append(hits, d)
			}
		}
		// A single device match by name or address opens it right away.
		if len(hits) == 1 && (strings.EqualFold(hits[0].Name, q) || hits[0].Address == q) {
			http.Redirect(w, r, fmt.Sprintf("/devices/%d", hits[0].ID), http.StatusSeeOther)
			return
		}
		objs, _ := s.st.SearchObjects(q, 200)
		data["Devices"], data["Objects"] = hits, objs
	}
	s.render(w, r, "search", data)
}

// liveJSON is what the device page polls to refresh its numbers.
type liveJSON struct {
	OK       bool                `json:"ok"`
	Err      string              `json:"err,omitempty"`
	At       int64               `json:"at"`
	Interval int                 `json:"interval"`
	Ports    map[string]livePort `json:"ports"`
}

type livePort struct {
	In   string  `json:"in"`
	Out  string  `json:"out"`
	Util float64 `json:"util"`
	Err  string  `json:"err"`
}

func (s *Server) deviceLive(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	snap := s.en.Snapshot(d.ID)
	out := liveJSON{Interval: int(s.en.Resolve(d).Interval.Seconds()), Ports: map[string]livePort{}}
	if snap != nil {
		out.OK, out.Err, out.At = snap.OK, snap.Err, snap.At.Unix()
		for id, pr := range engine.PortRates(snap) {
			out.Ports[fmt.Sprint(id)] = livePort{In: bitsText(pr.InBps), Out: bitsText(pr.OutBps), Util: pr.Util, Err: errText(pr.ErrPerMin)}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// bitsText shows a rate in Mbit/s, switching to Gbit/s from 1000 Mbit/s.
func bitsText(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.2f Gbit/s", bps/1e9)
	case bps >= 10e6:
		return fmt.Sprintf("%.0f Mbit/s", bps/1e6)
	case bps >= 1e6:
		return fmt.Sprintf("%.1f Mbit/s", bps/1e6)
	case bps > 0:
		return fmt.Sprintf("%.2f Mbit/s", bps/1e6)
	}
	return "0"
}

func errText(perMin float64) string {
	if perMin <= 0 {
		return ""
	}
	if perMin < 10 {
		return fmt.Sprintf("%.1f/min", perMin)
	}
	return fmt.Sprintf("%.0f/min", perMin)
}
