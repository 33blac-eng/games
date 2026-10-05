package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/organicoils/oo-screen/hub"
	"github.com/pion/webrtc/v4"
)

// S4: нога, знята з ноди БУДЬ-яким шляхом (removeViewer), лишає рівно один
// session_end з підсумком вводу; ланцюг цілий.
func TestAuditViewerLifecycle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := hub.OpenAuditLog(p)
	if err != nil {
		t.Fatal(err)
	}
	old := auditLog
	auditLog = a
	defer func() { auditLog = old; a.Close() }()

	ns := &nodeSession{nodeID: "pc-42", viewers: map[*webrtc.PeerConnection]*viewerLeg{}}
	vl := addViewerLimit(ns, &webrtc.PeerConnection{}, nil, "user-7", 0)
	if vl == nil {
		t.Fatal("addViewerLimit nil")
	}
	vl.sessionID = "abcdef0123456789secret"
	auditViewerStart(ns, vl, &hub.TicketClaims{UserID: "user-7", Grant: "control"})
	vl.audit.Count("input_accepted", 5)
	auditControl(ns, &hub.TicketClaims{UserID: "user-7", Grant: "control"}, "select_output", 1)
	removeViewer(ns, vl)
	removeViewer(ns, vl) // повтор — без другого запису

	if seq, _, err := hub.VerifyAuditFile(p); err != nil || seq != 3 {
		t.Fatalf("seq=%d err=%v", seq, err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{`"event":"session_start"`, `"event":"control_select_output"`, `"event":"session_end"`, `"node":"pc-42"`, `"user":"user-7"`, `"input_accepted":5`, `"session":"abcdef01"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in\n%s", want, s)
		}
	}
	if strings.Contains(s, "secret") {
		t.Error("full session secret leaked into audit log")
	}
}

func TestAuditDisabledNoop(t *testing.T) {
	if auditLog != nil {
		t.Skip("audit enabled in env")
	}
	ns := &nodeSession{nodeID: "n", viewers: map[*webrtc.PeerConnection]*viewerLeg{}}
	vl := addViewerLimit(ns, &webrtc.PeerConnection{}, nil, "u", 0)
	auditViewerStart(ns, vl, nil)
	vl.audit.Count("x", 1)
	auditControl(ns, nil, "select_output", 0)
	removeViewer(ns, vl)
}
