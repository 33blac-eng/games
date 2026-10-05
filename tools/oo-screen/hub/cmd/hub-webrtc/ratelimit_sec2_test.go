package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sec2Req(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/offer/viewer", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// Повторний аудит #21: розбір XFF.
func TestSec2XFFParsing(t *testing.T) {
	l := newIPLimiter(1, 1, "10.0.0.1, bogus, 300.1.1.1/8, fd00::/8")
	cases := []struct {
		remote string
		xff    []string
		want   string
	}{
		// Недовірений пір: XFF ігнорується повністю.
		{"203.0.113.5:1", []string{"1.2.3.4"}, "203.0.113.5"},
		// Клієнт підставляє лівий XFF, nginx дописує справжню адресу праворуч.
		{"10.0.0.1:1", []string{"6.6.6.6, 1.2.3.4"}, "1.2.3.4"},
		// Кілька заголовків XFF склеюються, береться найправіший недовірений.
		{"10.0.0.1:1", []string{"6.6.6.6", "1.2.3.4"}, "1.2.3.4"},
		// Сміття праворуч — лишаємось на проксі, а не на підробленому лівому.
		{"10.0.0.1:1", []string{"6.6.6.6, garbage"}, "10.0.0.1"},
		// IPv6-проксі з CIDR.
		{"[fd00::1]:1", []string{"2001:db8::7"}, "2001:db8::7"},
	}
	for _, c := range cases {
		if got := l.clientIP(sec2Req(c.remote, c.xff...)); got != c.want {
			t.Errorf("remote=%s xff=%v: %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	if len(l.trusted) != 2 {
		t.Fatalf("биті записи OO_SCREEN_TRUSTED_PROXIES мали відкинутись: %d", len(l.trusted))
	}
}

// Повторний аудит #21: IPv6-клієнт зі своїм /64 не обходить ліміт ротацією
// адрес і не роздуває мапу кошиків.
func TestSec2IPv6RotationSharesBucket(t *testing.T) {
	h := rateLimited(newIPLimiter(1, 2, ""), func(w http.ResponseWriter, r *http.Request) {})
	codes := map[int]int{}
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		h(rec, sec2Req(fmt.Sprintf("[2001:db8:1:2::%x]:5", i+1)))
		codes[rec.Code]++
	}
	if codes[http.StatusTooManyRequests] != 8 {
		t.Fatalf("ротація IPv6 в межах /64 обходить ліміт: %v", codes)
	}
	l := newIPLimiter(1, 2, "")
	now := time.Unix(1, 0)
	l.allow(rateKey(l.clientIP(sec2Req("[2001:db8:1:2::1]:5"))), now)
	l.allow(rateKey(l.clientIP(sec2Req("[2001:db8:1:2:ffff::9]:5"))), now)
	l.allow(rateKey(l.clientIP(sec2Req("[::ffff:198.51.100.1]:5"))), now)
	l.allow(rateKey(l.clientIP(sec2Req("198.51.100.1:5"))), now)
	if len(l.m) != 2 {
		t.Fatalf("кошиків %d, want 2 (/64 і v4-mapped = v4): %v", len(l.m), l.m)
	}
}
