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
//
// Хвиля 5 (S1, готовність до проду):
//   - master можна дати файлом OO_SCREEN_AGENT_SECRET_FILE (не світиться в
//     /proc/<pid>/environ і юніт-файлі); env OO_SCREEN_AGENT_SECRET має пріоритет;
//   - ротація master без одночасного перевипуску всього парку:
//     OO_SCREEN_AGENT_SECRET_PREV — старий master, токени якого ще приймаються
//     (з WARNING раз на ноду), поки агентам роздають нові;
//   - хаб на старті друкує режим автентифікації агентів і НЕ стартує, якщо
//     strict налаштовано так, що жоден агент не пройде (strict без окремого
//     master) — замість тихого відхилення всього парку.
package main

import (
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"unicode"

	"github.com/organicoils/oo-screen/hub"
)

// agentMaster — master-секрет для токенів нод: env OO_SCREEN_AGENT_SECRET,
// далі файл OO_SCREEN_AGENT_SECRET_FILE, далі спільний T1-токен.
func agentMaster() string {
	if v := os.Getenv("OO_SCREEN_AGENT_SECRET"); v != "" {
		return v
	}
	if p := os.Getenv("OO_SCREEN_AGENT_SECRET_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return token
}

// agentMasterPrev — попередній master на час ротації (порожньо = ротації нема).
// Спільний T1-токен як «попередній» не приймається: це був би легасі в обхід strict.
func agentMasterPrev() string {
	v := strings.TrimSpace(os.Getenv("OO_SCREEN_AGENT_SECRET_PREV"))
	if v == token {
		return ""
	}
	return v
}

// agentAuthStrict — чи увімкнено OO_SCREEN_AGENT_AUTH=strict.
func agentAuthStrict() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OO_SCREEN_AGENT_AUTH")), "strict")
}

// errStrictNoMaster — strict без окремого master: жоден агент не пройде.
var errStrictNoMaster = errors.New("OO_SCREEN_AGENT_AUTH=strict, але окремий master не задано (OO_SCREEN_AGENT_SECRET / OO_SCREEN_AGENT_SECRET_FILE порожні, нечитні або == OO_SCREEN_T1_TOKEN) — у strict токени нод відхилялися б УСІ. Згенеруй master (oo-node-token -gen-secret), випусти токени нод і лише тоді вмикай strict")

// errSecretFileUnreadable — файл master задано, але прочитати не вдалося.
var errSecretFileUnreadable = errors.New("OO_SCREEN_AGENT_SECRET_FILE задано, але файл не читається або порожній")

// agentAuthConfigError — фатальні помилки конфігурації автентифікації агентів
// (перевіряється на старті хаба).
func agentAuthConfigError() error {
	if os.Getenv("OO_SCREEN_AGENT_SECRET") == "" {
		if p := os.Getenv("OO_SCREEN_AGENT_SECRET_FILE"); p != "" {
			b, err := os.ReadFile(p)
			if err != nil || strings.TrimSpace(string(b)) == "" {
				return errSecretFileUnreadable
			}
		}
	}
	if agentAuthStrict() && !legacyAgentTokenAllowed() && agentMaster() == token {
		return errStrictNoMaster
	}
	return nil
}

// agentAuthModeSummary — один рядок для журналу старту: як зараз
// автентифікуються агенти.
func agentAuthModeSummary() string {
	mode := "legacy-compatible (спільний токен приймається з WARNING; для проду: OO_SCREEN_AGENT_AUTH=strict)"
	if agentAuthStrict() {
		mode = "strict (лише токени нод)"
		if legacyAgentTokenAllowed() {
			mode = "strict, АЛЕ OO_SCREEN_LEGACY_AGENT_TOKEN=1 (аварійний відкат: спільний токен приймається)"
		}
	}
	master := "master=спільний T1-токен (небезпечно: є на кожному ПК)"
	if agentMaster() != token {
		master = "master=окремий"
	}
	if agentMasterPrev() != "" {
		master += ", ротація: старий master ще приймається (OO_SCREEN_AGENT_SECRET_PREV)"
	}
	return "agent-auth: " + mode + "; " + master
}

// legacyAgentTokenAllowed — чи приймається спільний легасі-токен.
func legacyAgentTokenAllowed() bool {
	if os.Getenv("OO_SCREEN_LEGACY_AGENT_TOKEN") == "1" {
		return true
	}
	return !agentAuthStrict()
}

// legacyWarned — ноди, про чий легасі-токен уже попереджено (раз на ноду).
// Росте лише на успішній автентифікації спільним секретом, тож чужий без
// секрету її не роздує.
var legacyWarned sync.Map

// strictNoMasterWarned — одноразове ERROR про strict без окремого master.
var strictNoMasterWarned sync.Map

// prevWarned — ноди, що зайшли токеном старого master (раз на ноду).
var prevWarned sync.Map

// agentAuthorized — чи має право tok зареєструвати агента ноди node.
func agentAuthorized(node, tok string) bool {
	if !validAgentNodeID(node) {
		return false
	}
	if master := agentMaster(); master != token || legacyAgentTokenAllowed() {
		if hub.NodeTokenValid(master, node, tok) {
			return true
		}
		if prev := agentMasterPrev(); prev != "" && hub.NodeTokenValid(prev, node, tok) {
			if _, seen := prevWarned.LoadOrStore(node, struct{}{}); !seen {
				log.Printf("WARNING offer/agent [node=%s]: токен від СТАРОГО master (OO_SCREEN_AGENT_SECRET_PREV) — перевипусти токен ноди; після переходу всіх нод прибери _PREV", node)
			}
			return true
		}
	} else if _, seen := strictNoMasterWarned.LoadOrStore(struct{}{}, struct{}{}); !seen {
		// Повторний аудит #17: master == спільний T1-токен, а він є на кожному
		// ПК — будь-який агент виковує токен чужої ноди. У strict токени нод
		// без окремого OO_SCREEN_AGENT_SECRET не приймаються взагалі.
		log.Printf("ERROR offer/agent: OO_SCREEN_AGENT_AUTH=strict, але OO_SCREEN_AGENT_SECRET не задано (або == OO_SCREEN_T1_TOKEN) — токени нод відхиляються; задай окремий master")
	}
	if !tokenMatches(tok) {
		return false
	}
	if !legacyAgentTokenAllowed() {
		log.Printf("offer/agent [node=%s]: спільний легасі-токен відхилено (OO_SCREEN_AGENT_AUTH=strict) — агенту потрібен токен ноди: oo-node-token -node %q", node, node)
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
