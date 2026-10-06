package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/hub"
	"github.com/organicoils/oo-screen/internal/p2p"
	"github.com/pion/webrtc/v4"
)

// TestP2PFallbackRelayTicket — /p2p/offer споживає ОДНОРАЗОВИЙ ERP-квиток. Після
// відмови прямої ноги (тут agent-timeout) глядач мусить зайти на relay — з
// relay-квитком хаба, бо ERP-квиток уже спожито. Relay-квиток одноразовий.
func TestP2PFallbackRelayTicket(t *testing.T) {
	var mu sync.Mutex
	used := map[string]bool{}
	erp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ticket string `json:"ticket"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		again := used[body.Ticket]
		used[body.Ticket] = true
		mu.Unlock()
		if again {
			http.Error(w, "used", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hub.TicketClaims{UserID: "u1", OrgID: "o1", NodeID: strings.TrimPrefix(body.Ticket, "t-"), Grant: "control"})
	}))
	defer erp.Close()

	prevBase, prevKey, prevReg, prevBroker := erpBase, hubKey, reg, p2pBroker
	erpBase, hubKey = erp.URL, "test-key"
	reg = newRegistry()
	defer func() { erpBase, hubKey, reg, p2pBroker = prevBase, prevKey, prevReg, prevBroker }()
	ns := reg.getOrCreate("nodeA")
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{}
	ns.mu.Unlock()

	cfg := p2p.DefaultConfig()
	cfg.Enabled = true
	cfg.SignalTimeout = 150 * time.Millisecond // агент не полить — agent-timeout
	p2pBroker = p2p.NewBroker(cfg, p2p.Hooks{Authorize: p2pAuthorize, AgentAuth: p2pAgentAuth, AuditStart: p2pAuditStart})

	body, _ := json.Marshal(p2p.ViewerOfferReq{Ticket: "t-nodeA", SDP: "v=0"})
	rec := httptest.NewRecorder()
	p2pBroker.HandleOffer(rec, httptest.NewRequest(http.MethodPost, "/p2p/offer", bytes.NewReader(body)))
	var resp p2p.ViewerOfferResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if rec.Code != http.StatusConflict || resp.Reason != "agent-timeout" || resp.RelayTicket == "" {
		t.Fatalf("offer: %d %+v", rec.Code, resp)
	}
	// Старий шлях повтору — той самий ERP-квиток — справді мертвий.
	if _, _, st, _ := authorizeViewer(offerReq{Ticket: "t-nodeA"}); st != http.StatusForbidden {
		t.Fatalf("spent ERP ticket: %d", st)
	}
	gotNS, claims, st, msg := authorizeViewer(offerReq{Ticket: resp.RelayTicket})
	if st != 0 || gotNS != ns || claims == nil || claims.UserID != "u1" || claims.Grant != "control" {
		t.Fatalf("relay ticket: %d %s %+v", st, msg, claims)
	}
	if _, _, st, _ := authorizeViewer(offerReq{Ticket: resp.RelayTicket}); st != http.StatusForbidden {
		t.Fatalf("relay ticket reused: %d", st)
	}
	if _, _, st, _ := authorizeViewer(offerReq{Ticket: p2p.RelayTicketPrefix + "forged"}); st != http.StatusForbidden {
		t.Fatalf("forged relay ticket: %d", st)
	}
}
