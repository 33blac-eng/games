// Агент каже, що віддати картинку зараз НЕ МОЖЕ (16.09.2026).
//
// ЧОМУ. OO-агент живе в сесії користувача, а Windows не дає такій програмі
// знімати екран блокування (DuplicateOutput -> E_ACCESSDENIED). Хаб про це не
// знав і тримав ПК у /nodes як готовий: консоль ЕРП у режимі «Авто» йшла в OO,
// 8 секунд чекала першого кадру й лише тоді падала на Mesh — людина бачила
// «OO 0fps» на ПК Maria, заблокованому з 17:22. Mesh працює від SYSTEM і
// екран входу бачить, тож правильна відповідь — одразу Mesh.
//
// ЯК. Агент шле в "oosc-ctl" control.FallbackReason("session-locked") на
// блокуванні й FallbackReason("") на розблокуванні. Хаб лише запамʼятовує
// причину і прибирає ноду з /nodes. Старий агент нічого не шле — причина
// порожня, поведінка як була (безпечний бік). Нова агентська нога (реконект)
// скидає причину: агент повторить її сам, якщо вона досі чинна.
package main

import (
	"bufio"
	"bytes"
	"log"
	"time"

	"github.com/organicoils/oo-screen/internal/control"
)

// setAgentUnavailable — ЄДИНЕ місце запису ns.unavailable.
func setAgentUnavailable(ns *nodeSession, reason string) {
	ns.mu.Lock()
	changed := ns.unavailable != reason
	ns.unavailable = reason
	ns.mu.Unlock()
	if changed {
		log.Printf("agent availability [node=%s]: unavailable=%q", ns.nodeID, reason)
	}
}

func (ns *nodeSession) unavailableReason() string {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.unavailable
}

// handleAgentCtl — одне повідомлення агента в "oosc-ctl": fallback_reason
// (причина «нода недоступна») і content_mode (режим «Відео», videoboost.go).
// Нерозпізнане ігнорується мовчки: канал спільний із гейтом і пульсом, рвати
// його нема за що.
func handleAgentCtl(ns *nodeSession, data []byte, now time.Time) {
	if len(data) == 0 || data[0] != '{' {
		return
	}
	if data[len(data)-1] != '\n' {
		data = append(bytes.Clone(data), '\n')
	}
	m, err := control.Read(bufio.NewReader(bytes.NewReader(data)))
	if err != nil {
		return
	}
	switch m.Type {
	case control.TypeFallbackReason:
		setAgentUnavailable(ns, m.Reason)
	case control.TypeContentMode:
		onContentMode(ns, m.Mode, now)
	}
}
