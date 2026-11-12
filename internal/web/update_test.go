package web

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{{"v0.6.0", "v0.5.0", true}, {"v0.5.1", "v0.5.0", true}, {"v0.5.0", "v0.5.0", false}, {"v0.4.9", "v0.5.0", false},
		{"v1.0.0", "v0.9.12", true}, {"v0.6.0", "dev", false}, {"", "v0.5.0", false}, {"v0.6.0-rc1", "v0.5.0", true}}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
