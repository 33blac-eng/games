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
	// Токен pc1 не годиться для варіантів регістру/пробілів (окремий master,
	// бо strict без нього закритий — див. TestSec2StrictRequiresSeparateMaster).
	t.Setenv("OO_SCREEN_AGENT_SECRET", "separate-master-0123456789abcdef")
	tok := hub.NodeToken(agentMaster(), "pc1")
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	for _, n := range []string{"PC1", "pc1 ", "pс1"} {
		if agentAuthorized(n, tok) {
			t.Errorf("токен pc1 прийнято для %q", n)
		}
	}
	if !agentAuthorized("pc1", tok) {
		t.Error("власний токен pc1 відхилено")
	}
}

// Повторний аудит #17: без окремого OO_SCREEN_AGENT_SECRET master = спільний
// T1-токен, який лежить на КОЖНОМУ ПК парку. Тоді будь-який ПК обчислює токен
// чужої ноди сам: HMAC(token, "victim"). strict мусить це закривати.
func TestSec2StrictRequiresSeparateMaster(t *testing.T) {
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	t.Setenv("OO_SCREEN_LEGACY_AGENT_TOKEN", "")
	t.Setenv("OO_SCREEN_AGENT_SECRET", "")
	if agentAuthorized("victim", hub.NodeToken(token, "victim")) {
		t.Fatal("strict без OO_SCREEN_AGENT_SECRET: токен ноди, викований зі спільного T1-токена, прийнято")
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET", token)
	if agentAuthorized("victim", hub.NodeToken(token, "victim")) {
		t.Fatal("strict з OO_SCREEN_AGENT_SECRET == T1-токен: виковано")
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET", "separate-master-0123456789abcdef")
	if !agentAuthorized("victim", hub.NodeToken("separate-master-0123456789abcdef", "victim")) {
		t.Fatal("strict з окремим master: власний токен ноди відхилено")
	}
	if agentAuthorized("victim", hub.NodeToken(token, "victim")) {
		t.Fatal("strict з окремим master: токен від T1 прийнято")
	}
	// Перехідний (не strict) режим не змінено: master = T1 дозволено.
	t.Setenv("OO_SCREEN_AGENT_AUTH", "")
	t.Setenv("OO_SCREEN_AGENT_SECRET", "")
	if !agentAuthorized("victim", hub.NodeToken(token, "victim")) {
		t.Fatal("перехідний режим зламано")
	}
}
