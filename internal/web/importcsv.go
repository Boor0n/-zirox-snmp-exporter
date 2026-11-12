package web

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// ---- CSV import ----

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.formLists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "import", data)
}

// ImportResult reports one CSV line.
type ImportResult struct {
	ID   int64
	Line int
	Name string
	OK   bool
	Msg  string
}

func (s *Server) importSave(w http.ResponseWriter, r *http.Request) {
	var src io.Reader = strings.NewReader(r.FormValue("csv"))
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		src = f
	}
	results, err := s.ImportCSV(src, r.FormValue("default_credential"), r.FormValue("default_profile"))
	data, _ := s.formLists()
	if data == nil {
		data = map[string]any{}
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	data["Results"] = results
	ok := 0
	for _, res := range results {
		if res.OK {
			ok++
		}
	}
	data["Imported"] = ok
	var ids []string
	for _, res := range results {
		if res.OK {
			ids = append(ids, strconv.FormatInt(res.ID, 10))
		}
	}
	data["ImportedIDs"] = ids
	if ok > 0 {
		s.audit(r, "import", fmt.Sprintf("%d devices", ok), fmt.Sprintf("%d lines, %d failed", len(results), len(results)-ok))
	}
	s.render(w, r, "import", data)
}

// ImportCSV adds devices from CSV with a header row. Known columns:
// name, address, port, group, profile, credential, version, community.
func (s *Server) ImportCSV(src io.Reader, defaultCred, defaultProfile string) ([]ImportResult, error) {
	cr := csv.NewReader(src)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	cr.Comment = '#'
	head, err := cr.Read()
	if err != nil {
		return nil, errors.New("empty CSV")
	}
	if len(head) == 1 && strings.Contains(head[0], ";") {
		return nil, errors.New("use a comma as the separator")
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	if _, ok := col["address"]; !ok {
		return nil, errors.New("no address column")
	}
	get := func(rec []string, k string) string {
		if i, ok := col[k]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []ImportResult
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			out = append(out, ImportResult{Line: line, Msg: err.Error()})
			continue
		}
		d := store.Device{Name: get(rec, "name"), Address: get(rec, "address"), Group: get(rec, "group"), Enabled: true}
		d.Port, _ = strconv.Atoi(get(rec, "port"))
		if d.Name == "" {
			d.Name = d.Address
		}
		res := ImportResult{Line: line, Name: d.Name}
		if d.Address == "" {
			res.Msg = "no address"
			out = append(out, res)
			continue
		}
		cred, err := s.importCredential(get(rec, "credential"), get(rec, "version"), get(rec, "community"), defaultCred, d.Name)
		if err != nil {
			res.Msg = err.Error()
			out = append(out, res)
			continue
		}
		d.CredentialID = cred.ID
		if p := get(rec, "profile"); p != "" {
			prof, err := s.st.ProfileByName(p)
			if err != nil {
				res.Msg = "no profile \"" + p + "\""
				out = append(out, res)
				continue
			}
			d.ProfileID = prof.ID
		} else if defaultProfile != "" {
			d.ProfileID, _ = strconv.ParseInt(defaultProfile, 10, 64)
		}
		if err := s.st.SaveDevice(&d); err != nil {
			res.Msg = err.Error()
			if strings.Contains(res.Msg, "UNIQUE") {
				res.Msg = "already exists"
			}
			out = append(out, res)
			continue
		}
		s.en.Refresh(d.ID)
		res.ID, res.OK, res.Msg = d.ID, true, "added, full discovery running"
		out = append(out, res)
	}
	return out, nil
}

func (s *Server) importCredential(name, version, community, def, hint string) (store.Credential, error) {
	switch {
	case name != "":
		c, err := s.st.CredentialByName(name)
		if err != nil {
			return c, errors.New("no credentials \"" + name + "\"")
		}
		return c, nil
	case community != "":
		if version == "" {
			version = "2c"
		}
		if version != "1" && version != "2c" {
			return store.Credential{}, errors.New("a community can be given only for v1/v2c; for v3 use saved credentials by name")
		}
		return s.findOrCreateCredential(store.Credential{Version: version, Community: community}, hint)
	case def != "":
		id, _ := strconv.ParseInt(def, 10, 64)
		return s.st.Credential(id)
	}
	return store.Credential{}, errors.New("no credentials given")
}
