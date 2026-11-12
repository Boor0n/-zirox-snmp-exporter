// Package tlsx sets up HTTPS for Zirox: either a certificate the user
// provides (reloaded when the files change, so a renewal needs no restart)
// or a self-signed one Zirox creates and keeps in its data directory.
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Setup is what the server runs with.
type Setup struct {
	Config *tls.Config
	// CertFile is the certificate in PEM, served at /tls/cert.pem so
	// Prometheus and browsers can trust a self-signed one.
	CertFile string
	// SelfSigned is true when Zirox made the certificate.
	SelfSigned bool
	// Fingerprint is the SHA-256 of the certificate as AA:BB:..., to check
	// what the browser shows.
	Fingerprint string
	// NotAfter is when the certificate expires.
	NotAfter time.Time

	r *reloader
}

// Load returns nil when HTTPS is off. mode is ZIROX_TLS ("" or "off",
// "self-signed"); certFile and keyFile are ZIROX_TLS_CERT and ZIROX_TLS_KEY
// and win over mode. hosts are extra names for a self-signed certificate.
func Load(mode, certFile, keyFile, dataDir string, hosts []string) (*Setup, error) {
	switch {
	case certFile != "" || keyFile != "":
		if certFile == "" || keyFile == "" {
			return nil, errors.New("set both ZIROX_TLS_CERT and ZIROX_TLS_KEY")
		}
		return fromFiles(certFile, keyFile, false)
	case mode == "" || mode == "off" || mode == "0":
		return nil, nil
	case mode == "self-signed" || mode == "auto" || mode == "1":
		dir := filepath.Join(dataDir, "tls")
		cert, key := filepath.Join(dir, "zirox.crt"), filepath.Join(dir, "zirox.key")
		if err := ensureSelfSigned(dir, cert, key, hosts); err != nil {
			return nil, err
		}
		return fromFiles(cert, key, true)
	}
	return nil, fmt.Errorf("ZIROX_TLS=%q: use self-signed, or set ZIROX_TLS_CERT and ZIROX_TLS_KEY", mode)
}

func fromFiles(certFile, keyFile string, self bool) (*Setup, error) {
	r := &reloader{certFile: certFile, keyFile: keyFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	leaf := r.cert.Leaf
	sum := sha256.Sum256(leaf.Raw)
	return &Setup{
		Config: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: r.get,
		},
		CertFile:    certFile,
		SelfSigned:  self,
		Fingerprint: fingerprint(sum[:]),
		NotAfter:    leaf.NotAfter,
		r:           r,
	}, nil
}

func fingerprint(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02X", c)
	}
	return strings.Join(parts, ":")
}

// reloader reads the certificate again when its file changes, checking at
// most once a minute.
type reloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	mtime             time.Time
	checked           time.Time
}

func (r *reloader) load() error {
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("TLS certificate: %w", err)
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return err
		}
	}
	st, err := os.Stat(r.certFile)
	if err != nil {
		return err
	}
	r.cert, r.mtime = &c, st.ModTime()
	return nil
}

func (r *reloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) > time.Minute {
		r.checked = time.Now()
		if st, err := os.Stat(r.certFile); err == nil && !st.ModTime().Equal(r.mtime) {
			// A half-written renewal keeps the old certificate until it parses.
			old := r.cert
			if r.load() != nil {
				r.cert = old
			}
		}
	}
	return r.cert, nil
}

// ensureSelfSigned creates the certificate when it is missing, expires
// within 30 days, or does not cover all names.
func ensureSelfSigned(dir, certFile, keyFile string, extra []string) error {
	names, ips := localNames(extra)
	if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil &&
			time.Until(leaf.NotAfter) > 30*24*time.Hour && covers(leaf, names, ips) {
			return nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	cn := "zirox"
	if len(names) > 0 {
		cn = names[0]
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Zirox SNMP Exporter (self-signed)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // lets Prometheus use it as its own ca_file
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return writeFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// localNames returns the host names and addresses the certificate is for:
// the extra ones, the host name, localhost and every non-link-local address.
func localNames(extra []string) ([]string, []net.IP) {
	var names []string
	var ips []net.IP
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			return
		}
		seen[strings.ToLower(s)] = true
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
		} else {
			names = append(names, s)
		}
	}
	for _, h := range extra {
		add(h)
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLinkLocalUnicast() && !n.IP.IsLoopback() {
				add(n.IP.String())
			}
		}
	}
	return names, ips
}

func covers(c *x509.Certificate, names []string, ips []net.IP) bool {
	for _, n := range names {
		if c.VerifyHostname(n) != nil {
			return false
		}
	}
	for _, ip := range ips {
		if c.VerifyHostname(ip.String()) != nil {
			return false
		}
	}
	return true
}
