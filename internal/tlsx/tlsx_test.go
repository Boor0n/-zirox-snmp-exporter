package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelfSigned(t *testing.T) {
	dir := t.TempDir()
	if s, err := Load("", "", "", dir, nil); s != nil || err != nil {
		t.Fatalf("off: %v %v", s, err)
	}
	if _, err := Load("maybe", "", "", dir, nil); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := Load("", "only-cert.pem", "", dir, nil); err == nil {
		t.Fatal("cert without key accepted")
	}

	s, err := Load("self-signed", "", "", dir, []string{"zirox.example.net", "10.210.136.17"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.SelfSigned || len(s.Fingerprint) != 95 || time.Until(s.NotAfter) < 9*365*24*time.Hour {
		t.Fatalf("setup = %+v", s)
	}
	pemBytes, err := os.ReadFile(s.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "tls", "zirox.key")); st == nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode: %v", st)
	}

	// Started again, it keeps the same certificate.
	s2, err := Load("self-signed", "", "", dir, []string{"zirox.example.net"})
	if err != nil || s2.Fingerprint != s.Fingerprint {
		t.Fatalf("certificate not reused: %v", err)
	}
	// A new name makes a new certificate.
	s3, err := Load("self-signed", "", "", dir, []string{"other.example.net"})
	if err != nil || s3.Fingerprint == s.Fingerprint {
		t.Fatalf("certificate not renewed for a new name: %v", err)
	}
	pemBytes, _ = os.ReadFile(s3.CertFile)

	// A client that trusts the downloaded certificate connects by name and by IP.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{TLSConfig: s3.Config, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	for _, name := range []string{"other.example.net", "localhost", "127.0.0.1"} {
		c := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: name},
		}}
		r, err := c.Get("https://" + ln.Addr().String() + "/")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if string(b) != "ok" {
			t.Errorf("%s: %q", name, b)
		}
	}
}

func TestOwnCertificateReload(t *testing.T) {
	dir := t.TempDir()
	// Make two certificates with the self-signed code and swap them in place.
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := ensureSelfSigned(a, filepath.Join(a, "c.pem"), filepath.Join(a, "k.pem"), []string{"one.example"}); err != nil {
		t.Fatal(err)
	}
	if err := ensureSelfSigned(b, filepath.Join(b, "c.pem"), filepath.Join(b, "k.pem"), []string{"two.example"}); err != nil {
		t.Fatal(err)
	}
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cp := func(from, to string) {
		bs, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, bs, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cp(filepath.Join(a, "c.pem"), cert)
	cp(filepath.Join(a, "k.pem"), key)
	s, err := Load("off", cert, key, dir, nil) // files win over the mode
	if err != nil || s.SelfSigned {
		t.Fatalf("own certificate: %+v %v", s, err)
	}
	c1, _ := s.Config.GetCertificate(nil)
	if c1.Leaf.DNSNames[0] != "one.example" {
		t.Fatalf("first cert = %v", c1.Leaf.DNSNames)
	}
	// Renewal: new files with a later mtime; the next check (forced here) picks them up.
	cp(filepath.Join(b, "c.pem"), cert)
	cp(filepath.Join(b, "k.pem"), key)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(cert, future, future)
	s.r.checked = time.Time{}
	c2, _ := s.Config.GetCertificate(nil)
	if c2.Leaf.DNSNames[0] != "two.example" {
		t.Errorf("renewed cert not loaded: %v", c2.Leaf.DNSNames)
	}
}
