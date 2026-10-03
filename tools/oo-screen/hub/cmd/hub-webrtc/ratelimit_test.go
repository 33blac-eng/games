package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// SEC #21: per-IP token bucket; XFF лише від довіреного проксі.
func TestSecRateLimitPerIP(t *testing.T) {
	l := newIPLimiter(1, 3, "10.0.0.1, 192.168.0.0/16")
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if !l.allow("a", now) {
			t.Fatalf("burst %d відхилено", i)
		}
	}
	if l.allow("a", now) {
		t.Fatal("понад burst пропущено")
	}
	if !l.allow("b", now) {
		t.Fatal("інший IP постраждав")
	}
	if !l.allow("a", now.Add(1100*time.Millisecond)) {
		t.Fatal("кошик не поповнився")
	}

	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/offer/viewer", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	if got := l.clientIP(req("203.0.113.5:1", "1.2.3.4")); got != "203.0.113.5" {
		t.Fatalf("XFF від недовіреного прийнято: %s", got)
	}
	if got := l.clientIP(req("10.0.0.1:1", "6.6.6.6, 1.2.3.4, 192.168.1.1")); got != "1.2.3.4" {
		t.Fatalf("довірений ланцюг: %s, want 1.2.3.4", got)
	}

	h := rateLimited(newIPLimiter(1, 2, ""), func(w http.ResponseWriter, r *http.Request) {})
	codes := ""
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h(rec, req("198.51.100.1:5", ""))
		codes += fmt.Sprint(rec.Code, " ")
	}
	if codes != "200 200 429 " {
		t.Fatalf("codes=%s", codes)
	}
	if newIPLimiter(0, 1, "").allow("x", now) != true {
		t.Fatal("RATE=0 має вимикати ліміт")
	}
}

// SEC #21: стеля нод — нова нода понад maxNodes не створюється.
func TestSecMaxNodes(t *testing.T) {
	prevReg, prevMax := reg, maxNodes
	reg, maxNodes = newRegistry(), 2
	t.Cleanup(func() { reg, maxNodes = prevReg, prevMax })
	reg.getOrCreate("a")
	reg.getOrCreate("b")
	if ns, _ := reg.getOrCreateNew("c"); ns != nil {
		t.Fatal("нода понад стелю створена")
	}
	if reg.getOrCreate("a") == nil {
		t.Fatal("наявна нода недоступна на стелі")
	}
}
