// multimon.go — F6: кілька моніторів одночасно (OO_SCREEN_MULTIMON=1, типово
// ВИМКНЕНО).
//
// Додатковий монітор i ноди X агент публікує окремою agent-ногою під node_id
// "X#m<i>" (internal/multimon). Для хаба це звичайна nodeSession — зі своїм
// fanout-ом, GOP-кешем, NACK, бітрейтом і гейтом, — тож роздача «на трек»
// повністю перевикористовує перевірену машинерію одного треку і ізольована:
// глядач, що дивиться лише монітор 2, не тримає агента монітора 1 у resume.
//
// Що тут додається поверх:
//   - агентська автентифікація "X#m<i>" токеном НОДИ X (на ПК лише один токен);
//   - viewer/control: поле "monitor" обирає потік; нода береться, як і раніше,
//     ЛИШЕ з квитка (claims), монітор лише звужує її до X#m<i> — квиток на X
//     не відчиняє чужу ноду;
//   - /control віддає "streams" — які монітори зараз реально публікуються;
//   - runtime-revoke node=X закриває і всі X#m<i>.
//
// З вимкненим прапорцем "X#m1" — звичайний чужий node_id (потрібен свій токен),
// а "monitor">0 у viewer-запиті відхиляється 400.
package main

import (
	"os"

	"github.com/organicoils/oo-screen/internal/multimon"
)

// multimonEnabled читається в main() один раз; тести міняють напряму.
var multimonEnabled = os.Getenv("OO_SCREEN_MULTIMON") == "1"

// agentAuthNode — під чиїм токеном автентифікується agent-нога node.
func agentAuthNode(node string) string {
	if !multimonEnabled {
		return node
	}
	if base, _, ok := multimon.Parse(node); ok {
		return base
	}
	return node
}

// viewerStreamNode — node_id потоку, який просить глядач. monitor 0 = сама
// нода (старий шлях без змін). ok=false — запит некоректний.
func viewerStreamNode(node string, monitor int) (string, bool) {
	if monitor == 0 {
		return node, true
	}
	if !multimonEnabled || monitor < 0 || monitor > multimon.MaxIndex {
		return "", false
	}
	if _, _, nested := multimon.Parse(node); nested {
		return "", false
	}
	return multimon.NodeID(node, monitor), true
}

// liveStreams — індекси моніторів ноди base, які зараз мають publisher-а.
// 0 — сама нода. Порожньо, якщо фіча вимкнена (консоль тоді не показує
// side-by-side взагалі).
func liveStreams(base string) []int {
	if !multimonEnabled {
		return nil
	}
	var out []int
	if ns := reg.get(base); ns != nil && ns.hasAgent() {
		out = append(out, 0)
	}
	for i := 1; i <= multimon.MaxIndex; i++ {
		if ns := reg.get(multimon.NodeID(base, i)); ns != nil && ns.hasAgent() {
			out = append(out, i)
		}
	}
	return out
}

// monitorStreamSessions — живі nodeSession додаткових моніторів ноди base
// (для revoke node=base).
func monitorStreamSessions(base string) []*nodeSession {
	var out []*nodeSession
	for i := 1; i <= multimon.MaxIndex; i++ {
		if ns := reg.get(multimon.NodeID(base, i)); ns != nil {
			out = append(out, ns)
		}
	}
	return out
}

// hasMonitorStreams — чи публікує нода base хоч один додатковий потік X#m<i>.
// Тоді основний потік закріплений: select_output перевів би його на монітор,
// який уже захоплює дочірній процес (дубль, конфлікт DXGI).
func hasMonitorStreams(base string) bool {
	if !multimonEnabled {
		return false
	}
	for i := 1; i <= multimon.MaxIndex; i++ {
		if ns := reg.get(multimon.NodeID(base, i)); ns != nil && ns.hasAgent() {
			return true
		}
	}
	return false
}
