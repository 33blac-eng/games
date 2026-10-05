package main

// S4: аудит сесій. Сама механіка (хеш-ланцюг, JSONL, ендпоінт) — hub/audit.go;
// тут лише вмикання за env і точки, де хаб знає «хто/коли/до якого ПК».
// Дефолт — вимкнено: auditLog == nil, і всі виклики нижче — no-op.

import (
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/organicoils/oo-screen/hub"
)

var auditLog *hub.AuditLog

// startAudit відкриває журнал за OO_SCREEN_AUDIT_LOG і вішає
// GET /admin/audit (Bearer OO_SCREEN_AUDIT_TOKEN) на mux. Зламаний ланцюг у
// наявному файлі — фатально: хаб не дописує до підробленого журналу.
func startAudit(mux *http.ServeMux) {
	path := os.Getenv("OO_SCREEN_AUDIT_LOG")
	if path == "" {
		return
	}
	a, err := hub.OpenAuditLog(path)
	if err != nil {
		log.Fatalf("hub-webrtc: audit log %s: %v", path, err)
	}
	auditLog = a
	tok := os.Getenv("OO_SCREEN_AUDIT_TOKEN")
	mux.HandleFunc("/admin/audit", a.Handler(tok))
	log.Printf("audit: журнал %s (ендпоінт /admin/audit %s)", path, map[bool]string{true: "увімкнено", false: "вимкнено — нема OO_SCREEN_AUDIT_TOKEN"}[tok != ""])
}

// auditViewerStart — початок viewer-сесії (нога зареєстрована на ноді).
func auditViewerStart(ns *nodeSession, vl *viewerLeg, claims *hub.TicketClaims) {
	if auditLog == nil {
		return
	}
	base := hub.AuditRecord{Node: ns.nodeID, User: vl.userID}
	if claims != nil {
		base.Grant = claims.Grant
	}
	// Префікс session_id, не сам секрет (F-11): ним можна переукласти SDP.
	if len(vl.sessionID) >= 8 {
		base.Session = vl.sessionID[:8]
	}
	vl.audit = auditLog.StartSession(base)
}

// auditControl — керівна дія глядача (не потік вводу), окремим записом.
func auditControl(ns *nodeSession, claims *hub.TicketClaims, action string, arg int) {
	if auditLog == nil {
		return
	}
	rec := hub.AuditRecord{Event: "control_" + action, Node: ns.nodeID, Detail: strconv.Itoa(arg)}
	if claims != nil {
		rec.User, rec.Grant = claims.UserID, claims.Grant
	}
	_ = auditLog.Append(rec)
}
