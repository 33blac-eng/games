package main

// N6: пряме медіа агент↔браузер. Механіка — internal/p2p; тут лише вмикання
// за env і хуки, якими брокер спирається на ТІ САМІ перевірки, що й relay:
// authorizeViewer (одноразовий ERP-квиток, node з claims, живий publisher),
// agentAuthorized (токен ноди) і auditLog (S4). Дефолт — вимкнено:
// p2pBroker == nil, маршрутів /p2p/* нема, хаб поводиться як раніше.

import (
	"log"
	"net/http"
	"strings"

	"github.com/organicoils/oo-screen/hub"
	"github.com/organicoils/oo-screen/internal/p2p"
)

var p2pBroker *p2p.Broker

func startP2P(mux *http.ServeMux) {
	cfg := p2p.ConfigFromEnv(nil)
	if !cfg.Enabled {
		return
	}
	p2pBroker = p2p.NewBroker(cfg, p2p.Hooks{
		Authorize:  p2pAuthorize,
		AgentAuth:  p2pAgentAuth,
		AuditStart: p2pAuditStart,
	})
	p2pBroker.Register(mux, func(h http.HandlerFunc) http.HandlerFunc { return rateLimited(offerLimiter, h) })
	log.Printf("p2p: увімкнено (stun=%d, turn=%v) — один глядач на пряму ногу, решта через fan-out хаба", len(cfg.STUN), cfg.TURNURL != "")
}

// p2pAuthorize — лише ticket-режим: static-token глядач не має grant і не
// має права на ввід, а прямій нозі хаб мусить передати grant агентові.
func p2pAuthorize(_ *http.Request, ticket string) (p2p.Grant, int, int, string) {
	if !ticketModeEnabled() {
		return p2p.Grant{}, 0, http.StatusForbidden, "p2p requires ticket mode"
	}
	ns, claims, status, msg := authorizeViewer(offerReq{Ticket: ticket})
	if status != 0 {
		return p2p.Grant{}, 0, status, msg
	}
	ns.mu.Lock()
	viewers := len(ns.viewers)
	ns.mu.Unlock()
	return p2p.Grant{User: claims.UserID, Org: claims.OrgID, Node: ns.nodeID, Grant: claims.Grant}, viewers, 0, ""
}

func p2pAgentAuth(r *http.Request, node string) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && agentAuthorized(node, tok)
}

func p2pAuditStart(g p2p.Grant, id string) func(string) {
	if auditLog == nil {
		return func(string) {}
	}
	s := auditLog.StartSession(hub.AuditRecord{Node: g.Node, User: g.User, Grant: g.Grant, Session: id[:8], Detail: "transport=p2p"})
	return s.End
}

// p2pRevoke — S2: відкликання рве і прямі сесії (агент отримує "close").
func p2pRevoke(match func(p2p.Grant) bool) {
	if p2pBroker != nil {
		if n := p2pBroker.RevokeWhere(match); n > 0 {
			log.Printf("p2p: відкликано %d прямих сесій", n)
		}
	}
}
