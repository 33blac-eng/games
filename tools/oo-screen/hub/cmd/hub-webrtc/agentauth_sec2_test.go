package main

import (
	"strings"
	"testing"

	"github.com/organicoils/oo-screen/hub"
)

// Повторний аудит #17: агент не реєструється під битим node_id —
// керівні символи
// дають ін'єкцію в журнал (log.Printf("[node=%s]")).
func TestSec2AgentNodeIDValidation(t *testing.T) {
	t.Setenv("OO_SCREEN_AGENT_AUTH", "")
	t.Setenv("OO_SCREEN_LEGACY_AGENT_TOKEN", "")
	if !agentAuthorized("pc1", token) {
		t.Fatal("легасі для нормальної ноди зламано")
	}
	for _, bad := range []string{"pc1\nWARNING forged", "pc1\r", "a\x00b", strings.Repeat("x", 257)} {
		if agentAuthorized(bad, token) {
			t.Errorf("легасі-токен прийнято для node=%q", bad)
		}
		if agentAuthorized(bad, hub.NodeToken(agentMaster(), bad)) {
			t.Errorf("токен ноди прийнято для node=%q", bad)
		}
	}
	// Токен pc1 не годиться для варіантів регістру/пробілів.
	tok := hub.NodeToken(agentMaster(), "pc1")
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	for _, n := range []string{"PC1", "pc1 ", "pс1"} {
		if agentAuthorized(n, tok) {
			t.Errorf("токен pc1 прийнято для %q", n)
		}
	}
}
