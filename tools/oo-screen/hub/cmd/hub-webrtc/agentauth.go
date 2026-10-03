// agentauth.go — SEC #17: автентифікація агентської ноги токеном НОДИ.
//
// Токен ноди = hex(HMAC-SHA256(master, node_id)) (hub.NodeToken; випускає
// hub/cmd/oo-node-token). Master — OO_SCREEN_AGENT_SECRET, а без нього — той
// самий OO_SCREEN_T1_TOKEN, що й раніше, тож для переходу на хабі нічого
// додавати не треба: досить випустити токени нодам.
//
// Легасі (один спільний токен на весь парк) за замовчуванням ще приймається —
// інакше розгорнуті агенти випадуть усі разом, — але з WARNING у журнал раз на
// ноду. OO_SCREEN_AGENT_AUTH=strict вимикає легасі; OO_SCREEN_LEGACY_AGENT_TOKEN=1
// примусово лишає його увімкненим навіть у strict (аварійний відкат).
package main

import (
	"log"
	"os"
	"strings"
	"sync"
	"unicode"

	"github.com/organicoils/oo-screen/hub"
)

// agentMaster — master-секрет для токенів нод.
func agentMaster() string {
	if v := os.Getenv("OO_SCREEN_AGENT_SECRET"); v != "" {
		return v
	}
	return token
}

// legacyAgentTokenAllowed — чи приймається спільний легасі-токен.
func legacyAgentTokenAllowed() bool {
	if os.Getenv("OO_SCREEN_LEGACY_AGENT_TOKEN") == "1" {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("OO_SCREEN_AGENT_AUTH")), "strict")
}

// legacyWarned — ноди, про чий легасі-токен уже попереджено (раз на ноду).
// Росте лише на успішній автентифікації спільним секретом, тож чужий без
// секрету її не роздує.
var legacyWarned sync.Map

// agentAuthorized — чи має право tok зареєструвати агента ноди node.
func agentAuthorized(node, tok string) bool {
	if !validAgentNodeID(node) {
		return false
	}
	if hub.NodeTokenValid(agentMaster(), node, tok) {
		return true
	}
	if !tokenMatches(tok) {
		return false
	}
	if !legacyAgentTokenAllowed() {
		log.Printf("offer/agent [node=%s]: спільний легасі-токен відхилено (OO_SCREEN_AGENT_AUTH=strict)", node)
		return false
	}
	if _, seen := legacyWarned.LoadOrStore(node, struct{}{}); !seen {
		log.Printf("WARNING offer/agent [node=%s]: агент на СПІЛЬНОМУ легасі-токені — випусти токен ноди (oo-node-token) і увімкни OO_SCREEN_AGENT_AUTH=strict", node)
	}
	return true
}

// maxAgentNodeID — стеля довжини node_id (байт).
const maxAgentNodeID = 256

// validAgentNodeID — повторний аудит #17: надто довгий id і керівні символи
// (ін'єкція рядків у журнал через "[node=%s]") відкидаються до перевірки
// токена. Порожній id лишається: це T1/одновузловий сумісний режим.
func validAgentNodeID(node string) bool {
	if len(node) > maxAgentNodeID {
		return false
	}
	for _, r := range node {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return false
		}
	}
	return true
}
