package web

import (
	"net/http"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// ---- credentials ----

func (s *Server) credentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.st.Credentials()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	devs, _ := s.st.Devices()
	uses := map[int64]int{}
	for _, d := range devs {
		uses[d.CredentialID]++
	}
	s.render(w, r, "credentials", map[string]any{"Credentials": creds, "Uses": uses})
}

func (s *Server) credentialForm(w http.ResponseWriter, r *http.Request) {
	c := store.Credential{Version: "2c", V3Level: "authPriv", V3AuthProto: "SHA", V3PrivProto: "AES"}
	if id := pathID(r); id != 0 {
		var err error
		if c, err = s.st.Credential(id); err != nil {
			http.NotFound(w, r)
			return
		}
		c.Community, c.V3AuthPass, c.V3PrivPass = "", "", ""
	}
	s.render(w, r, "credential_form", map[string]any{"Cred": c})
}

func (s *Server) credentialSave(w http.ResponseWriter, r *http.Request) {
	c := credFromForm(r)
	c.ID = pathID(r)
	if c.Name == "" {
		s.render(w, r, "credential_form", map[string]any{"Cred": c, "Error": "enter a name"})
		return
	}
	if c.ID != 0 {
		// Empty secret fields keep the stored value.
		old, err := s.st.Credential(c.ID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		plain, err := s.en.DecryptCredential(old)
		if err != nil {
			s.render(w, r, "credential_form", map[string]any{"Cred": c, "Error": err.Error()})
			return
		}
		if c.Community == "" {
			c.Community = plain.Community
		}
		if c.V3AuthPass == "" {
			c.V3AuthPass = plain.V3AuthPass
		}
		if c.V3PrivPass == "" {
			c.V3PrivPass = plain.V3PrivPass
		}
	}
	if err := validateCred(c); err != nil {
		c.Community, c.V3AuthPass, c.V3PrivPass = "", "", ""
		s.render(w, r, "credential_form", map[string]any{"Cred": c, "Error": err.Error()})
		return
	}
	if err := s.encrypt(&c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.st.SaveCredential(&c); err != nil {
		c.Community, c.V3AuthPass, c.V3PrivPass = "", "", ""
		s.render(w, r, "credential_form", map[string]any{"Cred": c, "Error": err.Error()})
		return
	}
	if pathID(r) == 0 {
		s.audit(r, "credentials added", c.Name, "SNMP v"+c.Version)
	} else {
		s.audit(r, "credentials changed", c.Name, "SNMP v"+c.Version)
	}
	_ = s.en.ReloadAll()
	redirectWarn(w, r, "/credentials", "Credentials saved", communityWarning(c, s.en))
}

func (s *Server) credentialDelete(w http.ResponseWriter, r *http.Request) {
	name := s.credName(pathID(r))
	if err := s.st.DeleteCredential(pathID(r)); err != nil {
		redirectErr(w, r, "/credentials", err)
		return
	}
	s.audit(r, "credentials deleted", name, "")
	redirect(w, r, "/credentials", "Deleted")
}
