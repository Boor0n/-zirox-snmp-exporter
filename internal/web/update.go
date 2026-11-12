package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// releasesURL is where Zirox looks for a newer release. The request carries
// no data about the installation.
const releasesURL = "https://api.github.com/repos/Boor0n/-zirox-snmp-exporter/releases/latest"

// updateChecker asks GitHub for the latest release twice a day. It uses
// HTTPS_PROXY from the environment, like the installer.
type updateChecker struct {
	mu      sync.Mutex
	latest  string
	url     string
	checked time.Time
}

// Latest returns the newest release tag and its page, or "" when unknown.
func (u *updateChecker) Latest() (string, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.latest, u.url
}

func (u *updateChecker) run(ctx context.Context, url string) {
	check := func() {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "zirox-snmp-exporter")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		var rel struct {
			Tag  string `json:"tag_name"`
			HTML string `json:"html_url"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&rel) != nil || rel.Tag == "" {
			return
		}
		u.mu.Lock()
		u.latest, u.url, u.checked = rel.Tag, rel.HTML, time.Now()
		u.mu.Unlock()
	}
	check()
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// StartUpdateCheck begins the background release check.
func (s *Server) StartUpdateCheck(ctx context.Context) { go s.upd.run(ctx, releasesURL) }

// newer reports whether release tag a is newer than b ("v0.6.0" > "v0.5.1").
// Non-release builds ("dev") never report an update.
func newer(a, b string) bool {
	pa, oka := semver(a)
	pb, okb := semver(b)
	if !oka || !okb {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func semver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
