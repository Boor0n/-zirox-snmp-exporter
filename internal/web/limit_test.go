package web

import (
	"testing"
	"time"
)

func TestLoginLimiter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < loginMaxFails; i++ {
		if b, _ := l.blocked("10.0.0.1", "admin"); b {
			t.Fatalf("blocked after %d failures", i)
		}
		l.fail("10.0.0.1", "admin")
		now = now.Add(time.Second)
	}
	if b, wait := l.blocked("10.0.0.1", "admin"); !b || wait <= 0 || wait > loginWindow {
		t.Fatalf("not blocked: %v %v", b, wait)
	}
	// The name is blocked from other addresses, the address for other names.
	if b, _ := l.blocked("10.0.0.2", "ADMIN"); !b {
		t.Error("user name not blocked from another address")
	}
	if b, _ := l.blocked("10.0.0.1", "bob"); !b {
		t.Error("address not blocked for another name")
	}
	if b, _ := l.blocked("10.0.0.2", "bob"); b {
		t.Error("unrelated login blocked")
	}
	// After the window everything is allowed again and the map is cleaned.
	now = now.Add(loginWindow + time.Second)
	if b, _ := l.blocked("10.0.0.1", "admin"); b {
		t.Error("still blocked after the window")
	}
	if len(l.fails) != 0 {
		t.Errorf("stale entries: %v", l.fails)
	}
	// A success clears the name but not the address.
	for i := 0; i < loginMaxFails; i++ {
		l.fail("10.0.0.3", "carol")
	}
	l.success("carol")
	if b, _ := l.blocked("10.0.0.9", "carol"); b {
		t.Error("success did not clear the name")
	}
	if b, _ := l.blocked("10.0.0.3", "dave"); !b {
		t.Error("success cleared the address")
	}
}
