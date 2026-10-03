// F-39: глядач сам каже хабу «я прихований / я знову видимий», і хаб на цей час
// перестає слати йому відео, а коли приховані ВСІ — ще й ставить агента на паузу.
//
// 🔴 НАВІЩО ЦЕ ТУТ, А НЕ В БРАУЗЕРІ. З боку браузера прихована вкладка не має
// важеля зупинити декодування: track.enabled=false лише чорнить кадр (декодер
// працює), а transceiver.direction='inactive' вимагає ренегоціації, якої цей
// хаб на viewer-нозі не робить нікуди, крім ICE-restart. Тому єдиний реальний
// важіль — не слати пакети, а це рішення хаба. Трафік і CPU економляться саме
// там, де вони й витрачались: у людини у вкладці.
//
// ЧОМУ HTTP З session_id, А НЕ CONTROL-DATACHANNEL ГЛЯДАЧА.
//   - session_id уже існує і вже віддається глядачеві в answer (F-11,
//     answerResp.SessionID). Він адресує РІВНО ОДНУ viewer-ногу — а видимість це
//     властивість саме ноги (вкладки), не користувача й не ноди. Ні квиток
//     (одноразовий, спільний на user+node), ні токен цього виразити не вміють.
//   - Канал даних у глядача сьогодні є ЛИШЕ під OO_SCREEN_INPUT і лише в ноги з
//     grant-ом на ввід (input.go). Гейт видимості, прив'язаний до каналу вводу,
//     не працював би у глядача «тільки дивитись» — тобто рівно в того, хто сидить
//     із відкритою вкладкою годинами.
//   - Новий DataChannel на viewer-нозі означав би зміну SDP і ще одну гілку
//     ренегоціації. HTTP-ендпоїнт не чіпає медіатракт узагалі: помилка в ньому
//     не здатна зламати ні offer, ні answer.
//   - Прихована вкладка все ще може зробити fetch(keepalive) — саме те, що
//     потрібно в обробнику visibilitychange.
//
// 🔴 БЕЗПЕЧНИЙ ДЕФОЛТ (урок H-18 того ж дня: сувора перевірка кодека відмовила
// восьми живим глядачам за півгодини). Кожен сумнівний шлях тут веде до
// «продовжуємо слати», ніколи до «ріжемо»:
//   - нога, яка нічого не сказала, — видима (нульове значення hidden = false);
//   - битий JSON, чужий/протухлий session_id, не той метод — 4xx і ЖОДНОЇ зміни
//     стану: потік іде далі;
//   - поле hidden відсутнє -> false -> видимий;
//   - будь-яка прийнята подія вводу від цієї ноги знімає прихованість (input.go):
//     хто клацає, той дивиться, що б там не казав його останній POST.
//
// І прихований — це НЕ «пішов»: нога лишається в ns.viewers, її не рве ні
// таймаут (хаб узагалі не має reaper-а за бездіяльністю глядача — див.
// dropViewer callers), ні ICE (consent freshness у pion веде сам ICE-агент
// STUN-ами, незалежно від медіа), ні session cap (він про тривалість сесії, а
// не про активність). Повернення не потребує ні нового квитка, ні нової
// PeerConnection.
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// visibilityReq — тіло POST /viewer/visibility.
type visibilityReq struct {
	// SessionID — секрет ЦІЄЇ viewer-ноги з answer (F-11). Єдиний ключ: node,
	// user і grant не приходять із запиту й не переоцінюються — вони вже стоять
	// на нозі. Зникла нога з ns.viewers — зник і сенс цього ключа.
	SessionID string `json:"session_id"`
	// Hidden — true: вкладка прихована, відео мені не потрібне. Відсутнє поле =
	// false = видимий (безпечний бік).
	Hidden bool `json:"hidden"`
}

// visibilityResp — що хаб реально зробив. Повертаємо СТАН, а не "ok": браузер
// має бачити, чи його слово прийнято, і чи це вимкнуло агента.
type visibilityResp struct {
	Hidden bool `json:"hidden"`
	// AgentPaused — на цій ноді більше немає жодного видимого глядача, тобто
	// агенту пішов "pause". Діагностика для консолі; глядач на це не спирається.
	AgentPaused bool `json:"agent_paused"`
}

// setViewerHidden — ЄДИНЕ місце, де змінюється vl.hidden. Повертає true, якщо
// стан справді змінився.
//
// Порядок після зміни витриманий рівно як у гілці PeerConnectionStateConnected
// (main.go): спершу recomputeBinding (він і вмикає/вимикає публікацію, і на
// переході «не публікуємо»->«публікуємо» віддає нозі кеш GOP), потім sendGate
// (агент міг лишитись без єдиного видимого глядача або отримати першого), і
// лише потім, якщо кеш не поїхав, — позачерговий keyframe. Без цього останнього
// кроку глядач, що повернувся, дивився б у сірий екран до природного IDR.
func setViewerHidden(ns *nodeSession, vl *viewerLeg, hidden bool) bool {
	ns.mu.Lock()
	if vl.hidden == hidden {
		ns.mu.Unlock()
		return false
	}
	vl.hidden = hidden
	ns.mu.Unlock()

	recomputeBinding(ns)
	sendGate(ns)
	if !hidden && !viewerPrimed(ns, vl) {
		// Той самий аргумент, що й для новоприбулого глядача посеред потоку:
		// GOP 2 с чекати не треба, дебаунс усередині requestKeyframe.
		requestKeyframe(ns)
	}
	log.Printf("viewer visibility [node=%s]: hidden=%v", ns.nodeID, hidden)
	return true
}

// unhideViewer — безумовне «цей глядач точно дивиться». Кличеться з шляху
// вводу (input.go): подія миші чи клавіатури від ноги — незаперечний доказ, що
// людина за екраном, навіть якщо її останній POST загубився або прийшов не в
// тому порядку. Дешевий no-op, поки нога не прихована.
func unhideViewer(ns *nodeSession, vl *viewerLeg) {
	ns.mu.Lock()
	hidden := vl.hidden
	ns.mu.Unlock()
	if !hidden {
		return
	}
	setViewerHidden(ns, vl, false)
}

// handleViewerVisibility — POST /viewer/visibility: {session_id, hidden} ->
// {hidden, agent_paused}.
//
// Авторизація — той самий і єдиний ключ, що й у ренегоціації (F-11): нога
// знаходиться за своїм session_id серед ЖИВИХ ніг, більше нічого з запиту не
// береться. Права цим запитом не розширюються: він не вміє ні почати дивитись,
// ні перемкнути монітор, ні надіслати ввід — лише зупинити або відновити потік
// у ту саму ногу, яка вже авторизована.
func handleViewerVisibility(w http.ResponseWriter, r *http.Request) {
	corsHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOfferBody) // H-03
	var req visibilityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Стан НЕ чіпаємо: незрозумілий запит не сміє нікого позбавити картинки.
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	ns, vl := findViewerBySession(req.SessionID)
	if ns == nil {
		// Нога вже мертва або ключ чужий. 404 і жодних наслідків для чинних
		// глядачів — рівно як у renegotiateViewer.
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	setViewerHidden(ns, vl, req.Hidden)

	ns.mu.Lock()
	resp := visibilityResp{Hidden: vl.hidden, AgentPaused: !hasVisibleViewerLocked(ns)}
	ns.mu.Unlock()
	writeJSON(w, resp)
}
