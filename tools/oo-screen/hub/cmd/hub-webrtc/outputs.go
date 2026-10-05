// outputs.go — вибір монітора з боку глядача: список моніторів ноди і команда
// «перемкни на N».
//
// ЧОМУ HTTP, А НЕ DataChannel від глядача. Канал «браузер -> hub» тут не існував
// узагалі: "oosc-ctl" створює АГЕНТ, і hub у нього лише пише. Завести його на
// viewer-нозі означало б правити desktop-oo-webrtc.js (створити DataChannel до
// offer-а, домовитись про його стан із життєвим циклом шару) — а це другий
// сигнальний контур поруч із наявним і ще один шлях, яким сесія може напівстати.
// HTTP лягає в те, що вже є: та сама адреса хаба, той самий CORS, той самий
// одноразовий ERP-квиток, що вже возить /offer/viewer, і той самий control.Write
// у "oosc-ctl" на іншому кінці. Ціна — один зайвий квиток на клік; вона мізерна
// проти другого сигнального контуру.
//
// АВТОРИЗАЦІЯ — ТА САМА ФУНКЦІЯ, що й у /offer/viewer (authorizeViewer), не
// «схожа»: одноразовий квиток ЕРП -> claims -> node БЕРЕТЬСЯ З CLAIMS, а не з
// тіла запиту -> нода мусить мати живого publisher-а. Хто не має доступу до цього
// ПК, квитка на нього не дістане (ERP: RemoteAccessScope::canAccessNode), а
// вживаний квиток не спрацює вдруге (consume одноразовий). Тобто «перемкнути
// монітор» рівно там і тільки там, де дозволено «дивитись».
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/organicoils/oo-screen/internal/control"
)

// controlReq — тіло POST /control. Token/Ticket/Node читаються тим самим
// offerReq, що й сигналінг, тож обидва входи розбирають авторизаційні поля
// однаково; тут лишається сама команда.
type controlReq struct {
	offerReq
	// Output — індекс монітора. Вказівник, а не int: nil = «лише скажи, що є»
	// (наповнення перемикача при відкритті консолі), інакше 0 не відрізнити від
	// «перемкни на основний».
	Output *int `json:"output"`
}

// controlResp — стан моніторів ноди, як його зараз знає hub.
type controlResp struct {
	Outputs []outputInfo `json:"outputs"`
	Active  int          `json:"active"`
	// Streams — F6: монітори, що публікуються одночасно (0 = основний потік).
	// Порожнє без OO_SCREEN_MULTIMON — плеєр тоді не пропонує side-by-side.
	Streams []int `json:"streams,omitempty"`
}

// setOutputs запамʼятовує монітори ноди з offer-а агента.
func setOutputs(ns *nodeSession, list []outputInfo, active int) {
	ns.mu.Lock()
	ns.outputs = list
	ns.activeOutput = active
	ns.mu.Unlock()
}

// outputsSnapshot — копія списку під локом. Копія, а не сам зріз: віддавати
// назовні той самий backing array, який перепише наступний offer агента, —
// класична гонка «читаємо, поки хтось міняє».
func outputsSnapshot(ns *nodeSession) controlResp {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	out := make([]outputInfo, len(ns.outputs))
	copy(out, ns.outputs)
	return controlResp{Outputs: out, Active: ns.activeOutput}
}

// sendSelectOutput шле агентові ЦІЄЇ ноди «перемкни на idx» тим самим
// control-каналом і тим самим control.Write, що й bitrate_target. Повертає
// false, поки канал не відкритий: старий агент без DataChannel перемикати нічим,
// і мовчазний «успіх» тут був би брехнею консолі.
func sendSelectOutput(ns *nodeSession, idx int) bool {
	dc := ctlChan(ns)
	if dc == nil {
		return false
	}
	seq := atomic.AddUint64(&ns.ctlSeq, 1)
	if err := control.Write(dcWriter{dc}, control.SelectOutput(seq, idx)); err != nil {
		log.Printf("sendSelectOutput [node=%s]: %v", ns.nodeID, err)
		return false
	}
	return true
}

// handleControl — POST /control: {ticket|token, output?} -> {outputs, active}.
//
// Без "output" — чисте читання (перемикачу в консолі є чим наповнитись). З
// "output" — команда агентові. Відповідь однакова в обох випадках, тож консоль
// після кліку одразу бачить, на що hub розраховує.
func handleControl(w http.ResponseWriter, r *http.Request) {
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
	var req controlReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// select_output — команда ОСНОВНОМУ потоку ноди; потоки моніторів F6
	// закріплені за своїм монітором і перемикати їх нема чого.
	req.Monitor = 0
	ns, claims, status, msg := authorizeViewer(req.offerReq)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	// SEC #26: перемикання монітора міняє картинку ВСІМ глядачам ноди. За
	// OO_SCREEN_CONTROL_REQUIRES_INPUT=1 — лише квиток із grant=control (той
	// самий, що відчиняє ввід). Дефолт — як було: ERP-консоль може слати сюди
	// view-квитки.
	if controlRequiresInput() && (claims == nil || claims.Grant != grantControl) {
		http.Error(w, "control grant required", http.StatusForbidden)
		return
	}

	if req.Output != nil {
		idx := *req.Output
		if idx < 0 {
			http.Error(w, "output must be >= 0", http.StatusBadRequest)
			return
		}
		if !sendSelectOutput(ns, idx) {
			http.Error(w, "agent control channel not open", http.StatusConflict)
			return
		}
		// Оптимістично: агент відповіді не шле (control §select_output), а
		// правду про активний вихід приносить його наступний offer. Якщо індексу
		// не існує, агент лишиться на поточному моніторі й напише це в свій лог —
		// консоль побачить розбіжність за нерухомою картинкою, а не за брехнею
		// хаба, і хаб виправиться на першому ж реконекті агента.
		// ponytail: зворотного ack навмисно немає — він вимагав би читання
		// "oosc-ctl" у hub-і і відповіді в агенті; додати, коли зʼявиться друга
		// причина слухати агента, а не одна ця.
		ns.mu.Lock()
		ns.activeOutput = idx
		ns.mu.Unlock()
		log.Printf("select_output [node=%s]: -> %d", ns.nodeID, idx)
	}

	resp := outputsSnapshot(ns)
	resp.Streams = liveStreams(ns.nodeID)
	writeJSON(w, resp)
}

// controlRequiresInput — SEC #26, env OO_SCREEN_CONTROL_REQUIRES_INPUT=1.
func controlRequiresInput() bool {
	return os.Getenv("OO_SCREEN_CONTROL_REQUIRES_INPUT") == "1"
}
