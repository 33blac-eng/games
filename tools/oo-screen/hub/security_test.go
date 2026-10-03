package hub

// security_test.go — тести безпекового аудиту для ticket.go / revoke.go
// (звіт: tools/oo-screen/SECURITY-AUDIT.md, ідентифікатори SecTxx).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SecT01: квиток їде в ТІЛІ POST (не в URL/query, які осідають у access-логах
// nginx), разом із X-OO-Hub-Key.
func TestSecT01TicketInBodyWithHubKey(t *testing.T) {
	const tk = "jti-secret-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || strings.Contains(r.URL.String(), tk) {
			t.Errorf("method=%s url=%s — квиток не в тілі POST", r.Method, r.URL)
		}
		if r.Header.Get("X-OO-Hub-Key") != "k" {
			t.Errorf("X-OO-Hub-Key=%q", r.Header.Get("X-OO-Hub-Key"))
		}
		b, _ := io.ReadAll(r.Body)
		var req consumeTicketRequest
		_ = json.Unmarshal(b, &req)
		if req.Ticket != tk {
			t.Errorf("ticket у тілі = %q", req.Ticket)
		}
		_, _ = w.Write([]byte(`{"user_id":"u","node_id":"n","grant":"view"}`))
	}))
	defer srv.Close()
	c, err := ConsumeTicket(srv.URL, "k", tk)
	if err != nil || c.NodeID != "n" || c.Grant != "view" {
		t.Fatalf("claims=%+v err=%v", c, err)
	}
}

// SecT02: fail-closed — не-2xx, битий JSON, 2xx без claims, порожній квиток.
func TestSecT02ConsumeFailClosed(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{401, ``}, {409, `{"error":"already consumed"}`}, {410, `expired`}, {500, ``},
		{200, `not json`}, {200, `{}`}, {200, `{"node_id":"n"}`}, {302, ``},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		if cl, err := ConsumeTicket(srv.URL, "k", "t"); err == nil {
			t.Errorf("status=%d body=%q: claims=%+v, want помилку", c.status, c.body, cl)
		}
		srv.Close()
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	if _, err := ConsumeTicket(srv.URL, "k", "  "); err == nil || calls.Load() != 0 {
		t.Fatalf("порожній квиток: err=%v calls=%d", err, calls.Load())
	}
	if _, err := ConsumeTicket("", "k", "t"); err == nil {
		t.Fatal("порожній erpBase не дав помилки")
	}
}

// SecT03: replay — hub НЕ кешує claims: повторний consume того самого jti
// щоразу йде в ERP, і відмова ERP (single-use) = відмова хаба. Сам single-use,
// підпис, expiry і clock-skew живуть в ERP (поза репо) — UNVERIFIED.
func TestSecT03ReplayDelegatedToERP(t *testing.T) {
	used := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req consumeTicketRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if used[req.Ticket] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		used[req.Ticket] = true
		_, _ = w.Write([]byte(`{"user_id":"u","node_id":"n"}`))
	}))
	defer srv.Close()
	if _, err := ConsumeTicket(srv.URL, "k", "j"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsumeTicket(srv.URL, "k", "j"); err == nil {
		t.Fatal("повторний consume пройшов")
	}
}

// SecT04: відповідь ERP читається з LimitReader (1 МБ) — гігантське тіло не
// з'їдає памʼять і не дає claims.
func TestSecT04ERPBodyBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"u","node_id":"n","pad":"`))
		chunk := []byte(strings.Repeat("a", 64<<10))
		for i := 0; i < 64; i++ { // 4 МБ
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	if _, err := ConsumeTicket(srv.URL, "k", "t"); err == nil {
		t.Fatal("обрізане 1-МБ тіло дало claims")
	}
}

// SecT05 (FIXED, SEC #28): тіло помилки ERP у помилці consume (а далі в
// журналі хаба) обрізане до 200 символів і без квитка/токеноподібних рядків.
func TestSecT05ERPErrorBodyRedacted(t *testing.T) {
	long := "echo:jti-SECRET " + strings.Repeat("x", 1000) + " eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJlLXNpZw"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(long))
	}))
	defer srv.Close()
	_, err := ConsumeTicket(srv.URL, "k", "jti-SECRET")
	if err == nil || strings.Contains(err.Error(), "jti-SECRET") {
		t.Fatalf("err=%v — квиток у помилці", err)
	}
	if len(err.Error()) > 300 {
		t.Fatalf("помилка не обрізана: %d байт", len(err.Error()))
	}
	if got := redactBody("a eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJlLXNpZw b", ""); strings.Contains(got, "eyJ") {
		t.Fatalf("JWT не вирізано: %q", got)
	}
}

// SecT06: поллінг відкликань — node/user доходять до onRevoke, курсор since
// рухається вперед, на помилці лишається на місці (fail-soft), ключ у заголовку.
func TestSecT06RevokePoll(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-OO-Hub-Key") != "k" {
			w.WriteHeader(401)
			return
		}
		if fail {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(`[{"node_id":"n1","at":200},{"user_id":"u1","at":300},{"at":999}]`))
	}))
	defer srv.Close()
	var got []string
	on := func(k, v string) { got = append(got, k+"="+v) }
	next, ok := pollRevocationsOnce(context.Background(), srv.Client(), srv.URL, "k", 100, on)
	if !ok || next != 300 || strings.Join(got, ",") != "node=n1,user=u1" {
		t.Fatalf("next=%d ok=%v got=%v", next, ok, got)
	}
	fail = true
	if n, ok := pollRevocationsOnce(context.Background(), srv.Client(), srv.URL, "k", 300, on); ok || n != 300 {
		t.Fatalf("на 500: next=%d ok=%v, want 300/false", n, ok)
	}
	if n, ok := pollRevocationsOnce(context.Background(), srv.Client(), srv.URL, "wrong", 300, on); ok || n != 300 {
		t.Fatalf("чужий ключ: next=%d ok=%v", n, ok)
	}
}

// SecT07: staleGate — після staleAfter без успіху рве ВСЕ рівно один раз;
// невалідний env не вимикає рубильник.
func TestSecT07StaleGateFailClosed(t *testing.T) {
	var fired []string
	g := &staleGate{after: 90 * time.Second, url: "u", onRevoke: func(k, v string) { fired = append(fired, k) }}
	t0 := time.Unix(1000, 0)
	g.observe(t0, true)
	g.observe(t0.Add(89*time.Second), false)
	if len(fired) != 0 {
		t.Fatal("спрацювало до порогу")
	}
	g.observe(t0.Add(91*time.Second), false)
	g.observe(t0.Add(120*time.Second), false)
	if len(fired) != 1 || fired[0] != RevokeKindStale {
		t.Fatalf("fired=%v, want рівно [stale]", fired)
	}
	t.Setenv(envStaleAfter, "0")
	if staleAfterFromEnv() != defaultStaleAfter {
		t.Fatal("OO_SCREEN_REVOKE_STALE_AFTER=0 вимкнув рубильник")
	}
	t.Setenv(envStaleAfter, "garbage")
	if staleAfterFromEnv() != defaultStaleAfter {
		t.Fatal("сміття в env змінило поріг")
	}
}
