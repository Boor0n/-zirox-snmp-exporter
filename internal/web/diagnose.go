package web

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// ---- single device ----

func (s *Server) deviceDiagnose(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	diag := s.en.Diagnose(d)
	s.render(w, r, "diagnose", map[string]any{"Device": d, "Diag": diag})
}

// ---- background jobs: bulk access check and subnet scan ----

// jobsKept is how many finished jobs stay viewable.
const jobsKept = 20

// checkRow is one device of a bulk access check.
type checkRow struct {
	Device store.Device
	Diag   snmpx.Diagnosis
}

// scanRow is one address that answered a subnet scan.
type scanRow struct {
	Address    string
	Info       snmpx.SystemInfo
	Credential store.Credential
	Existing   string // name of the device that already has this address
}

type job struct {
	ID      string
	Kind    string // "check" or "scan"
	Title   string
	Started time.Time
	Took    time.Duration
	Total   int

	mu    sync.Mutex
	done  int
	fin   bool
	check []checkRow
	scan  []scanRow
	// scan settings, reused when adding the found devices
	Group     string
	ProfileID int64
}

func (j *job) progress() (int, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done, j.fin
}

type jobs struct {
	mu   sync.Mutex
	list []*job
}

func (js *jobs) add(j *job) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.list = append(js.list, j)
	if len(js.list) > jobsKept {
		js.list = js.list[len(js.list)-jobsKept:]
	}
}

func (js *jobs) get(id string) *job {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, j := range js.list {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func newJobID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// parallel runs fn for 0..n-1 with at most limit at once.
func parallel(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// StartCheck runs the access ladder for devices in the background and
// returns the job ID.
func (s *Server) StartCheck(ids []int64, title string) string {
	var devs []store.Device
	for _, id := range ids {
		if d, err := s.st.Device(id); err == nil {
			devs = append(devs, d)
		}
	}
	j := &job{ID: newJobID(), Kind: "check", Title: title, Started: time.Now(), Total: len(devs)}
	s.jobs.add(j)
	go func() {
		rows := make([]checkRow, len(devs))
		parallel(len(devs), 32, func(i int) {
			rows[i] = checkRow{Device: devs[i], Diag: s.en.Diagnose(devs[i])}
			j.mu.Lock()
			j.done++
			j.mu.Unlock()
		})
		// Failures first, grouped by the failed step.
		sort.SliceStable(rows, func(a, b int) bool {
			ra, rb := rows[a].Diag, rows[b].Diag
			if ra.OK != rb.OK {
				return !ra.OK
			}
			if ra.Failed != rb.Failed {
				return ra.Failed < rb.Failed
			}
			return rows[a].Device.Name < rows[b].Device.Name
		})
		j.mu.Lock()
		j.check, j.fin, j.Took = rows, true, time.Since(j.Started)
		j.mu.Unlock()
	}()
	return j.ID
}

func (s *Server) checkPage(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.get(r.PathValue("job"))
	if j == nil || j.Kind != "check" {
		redirect(w, r, "/devices", "That check has expired; run it again")
		return
	}
	done, fin := j.progress()
	data := map[string]any{"Job": j, "Done": done, "Finished": fin}
	if fin {
		ok, failed := 0, 0
		bySource := map[string][]string{}
		for _, row := range j.check {
			if row.Diag.OK {
				ok++
				continue
			}
			failed++
			if row.Diag.Problem == snmpx.ProblemTimeout || row.Diag.Problem == snmpx.ProblemCredential {
				bySource[row.Diag.Source] = append(bySource[row.Diag.Source], row.Device.Address)
			}
		}
		data["Rows"], data["OK"], data["Failed"], data["ACL"] = j.check, ok, failed, bySource
	}
	s.render(w, r, "check", data)
}

// checkCSV exports a bulk check for the network team: one line per device.
func (s *Server) checkCSV(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.get(r.PathValue("job"))
	if j == nil || j.Kind != "check" {
		http.NotFound(w, r)
		return
	}
	if _, fin := j.progress(); !fin {
		http.Error(w, "the check is still running", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="zirox-access-check.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"device", "address", "group", "result", "failed_step", "problem", "zirox_source_address", "details"})
	for _, row := range j.check {
		res := "ok"
		if !row.Diag.OK {
			res = "fail"
		}
		_ = cw.Write([]string{row.Device.Name, row.Device.Address, row.Device.Group, res, row.Diag.Failed, row.Diag.Problem, row.Diag.Source, row.Diag.Summary()})
	}
	cw.Flush()
}

// ---- subnet scan ----

// maxScanHosts limits one scan to a /20.
const maxScanHosts = 4096

// hostsOf lists the usable addresses of a CIDR (or one address).
func hostsOf(cidr string) ([]string, error) {
	cidr = strings.TrimSpace(cidr)
	if !strings.Contains(cidr, "/") {
		a, err := netip.ParseAddr(cidr)
		if err != nil || !a.Is4() {
			return nil, errors.New("enter an IPv4 subnet such as 10.214.16.0/24")
		}
		return []string{a.String()}, nil
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return nil, errors.New("enter an IPv4 subnet such as 10.214.16.0/24")
	}
	p = p.Masked()
	if p.Bits() < 20 {
		return nil, fmt.Errorf("the subnet is too large: at most /20 (%d addresses) per scan", maxScanHosts)
	}
	var out []string
	for a := p.Addr(); p.Contains(a); a = a.Next() {
		out = append(out, a.String())
	}
	if p.Bits() <= 30 && len(out) > 2 {
		out = out[1 : len(out)-1] // network and broadcast
	}
	return out, nil
}

func (s *Server) scanPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.formLists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if id := r.PathValue("job"); id != "" {
		j := s.jobs.get(id)
		if j == nil || j.Kind != "scan" {
			redirect(w, r, "/scan", "That scan has expired; run it again")
			return
		}
		done, fin := j.progress()
		data["Job"], data["Done"], data["Finished"] = j, done, fin
		if fin {
			data["Found"] = j.scan
			n := 0
			for _, f := range j.scan {
				if f.Existing == "" {
					n++
				}
			}
			data["NewCount"] = n
		}
	}
	s.render(w, r, "scan", data)
}

func (s *Server) scanStart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, "/scan", err)
		return
	}
	hosts, err := hostsOf(r.FormValue("subnet"))
	if err != nil {
		redirectErr(w, r, "/scan", err)
		return
	}
	var creds []store.Credential
	for _, v := range r.Form["cred"] {
		id, _ := strconv.ParseInt(v, 10, 64)
		c, err := s.st.Credential(id)
		if err != nil {
			continue
		}
		plain, err := s.en.DecryptCredential(c)
		if err != nil {
			continue
		}
		plain.ID, plain.Name = c.ID, c.Name
		creds = append(creds, plain)
	}
	if len(creds) == 0 {
		redirectErr(w, r, "/scan", errors.New("choose at least one set of credentials to try"))
		return
	}
	existing := map[string]string{}
	if devs, err := s.st.Devices(); err == nil {
		for _, d := range devs {
			existing[d.Address] = d.Name
		}
	}
	prof, _ := strconv.ParseInt(r.FormValue("profile_id"), 10, 64)
	j := &job{ID: newJobID(), Kind: "scan", Title: strings.TrimSpace(r.FormValue("subnet")), Started: time.Now(), Total: len(hosts),
		Group: strings.TrimSpace(r.FormValue("group")), ProfileID: prof}
	s.jobs.add(j)
	s.audit(r, "subnet scan", j.Title, fmt.Sprintf("%d addresses, %d credentials", len(hosts), len(creds)))
	go func() {
		found := make([]*scanRow, len(hosts))
		parallel(len(hosts), 128, func(i int) {
			for _, c := range creds {
				t := snmpx.Target{Address: hosts[i], Port: 161, Cred: c, Timeout: 1500 * time.Millisecond, Retries: 0}
				info, err := s.en.Identify(t)
				if err == nil {
					found[i] = &scanRow{Address: hosts[i], Info: info, Credential: c, Existing: existing[hosts[i]]}
					break
				}
			}
			j.mu.Lock()
			j.done++
			j.mu.Unlock()
		})
		var rows []scanRow
		for _, f := range found {
			if f != nil {
				f.Credential.Community, f.Credential.V3AuthPass, f.Credential.V3PrivPass = "", "", ""
				rows = append(rows, *f)
			}
		}
		j.mu.Lock()
		j.scan, j.fin, j.Took = rows, true, time.Since(j.Started)
		j.mu.Unlock()
	}()
	http.Redirect(w, r, "/scan/"+j.ID, http.StatusSeeOther)
}

// scanAdd creates devices for the ticked scan results and discovers them.
func (s *Server) scanAdd(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.get(r.PathValue("job"))
	if j == nil || j.Kind != "scan" {
		redirect(w, r, "/scan", "That scan has expired; run it again")
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, "/scan/"+j.ID, err)
		return
	}
	want := map[string]bool{}
	for _, a := range r.Form["addr"] {
		want[a] = true
	}
	group := strings.TrimSpace(r.FormValue("group"))
	var ids []int64
	var names, failed []string
	j.mu.Lock()
	rows := append([]scanRow(nil), j.scan...)
	j.mu.Unlock()
	for _, f := range rows {
		if !want[f.Address] || f.Existing != "" {
			continue
		}
		name := deviceNameFor(f)
		d := store.Device{Name: name, Address: f.Address, Port: 161, CredentialID: f.Credential.ID, ProfileID: j.ProfileID, Group: group, Enabled: true}
		err := s.st.SaveDevice(&d)
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			d.ID, d.Name = 0, name+" "+f.Address
			err = s.st.SaveDevice(&d)
		}
		if err != nil {
			failed = append(failed, f.Address+": "+err.Error())
			continue
		}
		ids = append(ids, d.ID)
		names = append(names, d.Name)
		s.en.Refresh(d.ID)
	}
	if len(names) > 0 {
		s.audit(r, "devices added from scan", fmt.Sprintf("%d devices", len(names)), shortList(names))
	}
	if len(failed) > 0 {
		redirectErr(w, r, "/devices", fmt.Errorf("added %d, failed: %s", len(names), strings.Join(failed, "; ")))
		return
	}
	redirect(w, r, "/devices", fmt.Sprintf("Added %d devices; full discovery is running", len(ids)))
}

// deviceNameFor names a scanned device after sysName without the domain.
func deviceNameFor(f scanRow) string {
	n := strings.TrimSpace(f.Info.SysName)
	if n == "" || net.ParseIP(n) != nil {
		return f.Address
	}
	if i := strings.IndexByte(n, '.'); i > 0 {
		n = n[:i]
	}
	return n
}
