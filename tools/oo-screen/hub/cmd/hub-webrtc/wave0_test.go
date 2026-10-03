package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// H-03: тіло понад стелю не читається в памʼять цілком — 400 від декодера,
// а не 200 після мегабайтів JSON.
func TestOfferRejectsOversizedBody(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxOfferBody+1)
	req := httptest.NewRequest(http.MethodPost, "/offer/agent", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	handleOffer("agent")(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: want 400, got %d", rr.Code)
	}
}

// H-20: токен порівнюється сталим часом і не пропускає префікси/суфікси.
func TestTokenMatches(t *testing.T) {
	old := token
	token = "secret-1"
	t.Cleanup(func() { token = old })
	for _, bad := range []string{"", "secret", "secret-10", "Secret-1"} {
		if tokenMatches(bad) {
			t.Fatalf("%q must not match", bad)
		}
	}
	if !tokenMatches("secret-1") {
		t.Fatal("exact token must match")
	}
}

// H-13: stale-ERP знімає глядачів і НЕ чіпає агента: publisher лишається,
// парк не йде в масовий реконект.
func TestStaleDropsViewersKeepsAgent(t *testing.T) {
	ns := readyNode(t, "n-stale")
	addReadyViewer(t, ns)
	if n := dropAllViewers(ns, "test stale"); n != 2 {
		t.Fatalf("dropped %d viewer legs, want 2", n)
	}
	ns.mu.Lock()
	left := len(ns.viewers)
	ns.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d viewer legs left, want 0", left)
	}
	if !ns.hasAgent() {
		t.Fatal("agent leg must survive a stale-ERP revoke")
	}
}
