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
	// MaxBitrateBps / MaxFps — «Якість» і «Швидкість» тулбара (контракт C1).
	// RawMessage, а не *int: тут ТРИ стани, і всі різні. Поля немає — не чіпати
	// (вибір монітора не має скидати якість); null — зняти стелю; число — стеля.
	MaxBitrateBps json.RawMessage `json:"max_bitrate_bps,omitempty"`
	MaxFps        json.RawMessage `json:"max_fps,omitempty"`
}

// controlResp — стан моніторів ноди, як його зараз знає hub, і застосовані
// стелі якості. nil = стелі немає (серіалізується як null, а не зникає: консоль
// має відрізняти «стелі немає» від «старий хаб про стелю не знає»).
type controlResp struct {
	Outputs       []outputInfo `json:"outputs"`
	Active        int          `json:"active"`
	MaxBitrateBps *uint64      `json:"max_bitrate_bps"`
	MaxFps        *int         `json:"max_fps"`
	// Streams — F6: монітори, що публікуються одночасно (0 = основний потік).
	// Порожнє без OO_SCREEN_MULTIMON — плеєр тоді не пропонує side-by-side.
	Streams []int `json:"streams,omitempty"`
}

// maxFpsCeil — верх стелі кадрів/с (C1). Ним же знімається стеля в агента:
// агент бере min(свій -fps, стеля), тож 60 = «як без стелі».
const maxFpsCeil = 60

// optInt розбирає поле з трьома станами: set=false — поля немає; set=true і
// null=true — явний null; інакше v.
func optInt(raw json.RawMessage) (v int64, set, null bool, err error) {
	if len(raw) == 0 {
		return 0, false, false, nil
	}
	if string(raw) == "null" {
		return 0, true, true, nil
	}
	err = json.Unmarshal(raw, &v)
	return v, true, false, err
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
	resp := controlResp{Outputs: out, Active: ns.activeOutput}
	if ns.capBps > 0 {
		v := ns.ceilingBps()
		resp.MaxBitrateBps = &v
	}
	if ns.maxFps > 0 {
		v := ns.maxFps
		resp.MaxFps = &v
	}
	return resp
}

// sendSelectOutput шле агентові ЦІЄЇ ноди «перемкни на idx» тим самим
// control-каналом і тим самим control.Write, що й bitrate_target. Повертає
// false, поки канал не відкритий: старий агент без DataChannel перемикати нічим,
// і мовчазний «успіх» тут був би брехнею консолі.
func sendSelectOutput(ns *nodeSession, idx int) bool {
	return sendAgentCtl(ns, func(seq uint64) control.Msg { return control.SelectOutput(seq, idx) })
}

// sendMaxFps — стеля кадрів/с агентові ЦІЄЇ ноди (C1), тим самим шляхом.
func sendMaxFps(ns *nodeSession, fps int) bool {
	return sendAgentCtl(ns, func(seq uint64) control.Msg { return control.MaxFps(seq, fps) })
}

func sendAgentCtl(ns *nodeSession, build func(seq uint64) control.Msg) bool {
	dc := ctlChan(ns)
	if dc == nil {
		return false
	}
	m := build(atomic.AddUint64(&ns.ctlSeq, 1))
	if err := control.Write(dcWriter{dc}, m); err != nil {
		log.Printf("ctl %s [node=%s]: %v", m.Type, ns.nodeID, err)
		return false
	}
	return true
}

// handleControl — POST /control: {ticket|token, output?, max_bitrate_bps?,
// max_fps?} -> {outputs, active, max_bitrate_bps, max_fps}.
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

	// Спершу розбираємо ВСІ поля, потім застосовуємо: інакше {"output":1,
	// "max_fps":0} перемикав монітор і відповідав 400, а консоль вважала, що
	// нічого не застосовано.
	if req.Output != nil && *req.Output < 0 {
		http.Error(w, "output must be >= 0", http.StatusBadRequest)
		return
	}
	bps, bpsSet, bpsNull, err := optInt(req.MaxBitrateBps)
	if err != nil || (bpsSet && !bpsNull && bps <= 0) {
		http.Error(w, "max_bitrate_bps must be a positive integer or null", http.StatusBadRequest)
		return
	}
	fps, fpsSet, fpsNull, err := optInt(req.MaxFps)
	if err != nil || (fpsSet && !fpsNull && fps <= 0) {
		http.Error(w, "max_fps must be a positive integer or null", http.StatusBadRequest)
		return
	}

	if req.Output != nil {
		idx := *req.Output
		if hasMonitorStreams(ns.nodeID) {
			http.Error(w, "multimon active: monitors are published as separate streams, select_output disabled", http.StatusConflict)
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
		auditControl(ns, claims, "select_output", idx)
	}

	ns.mu.Lock()
	curCap, hadFps := ns.capBps, ns.maxFps > 0
	ns.mu.Unlock()

	// C1: «Якість» — стеля бітрейту ноди. Лише стан хаба (агентові їде звичайний
	// bitrate_target), тож 409 тут не буває. Та сама стеля, що вже діє, — нічого
	// не робити, як і для fps нижче: тулбар повторює поточну «Якість» (число або
	// null) з кожною зміною «Швидкості» і на повторі після реконекту, а
	// setBitrateCap скинув би вивчену ціль контролера на стелю (H-26).
	if bpsSet && clampBitrateCap(uint64(bps)) != curCap {
		setBitrateCap(ns, uint64(bps)) // null -> bps 0 -> стелю знято
	}

	// C1: «Швидкість» — стеля кадрів/с, її виконує агент.
	// null при знятій стелі — нічого не робити: агента смикати нема чим, і 409
	// без каналу тут був би брехнею («не застосовано» те, що й так діє).
	if fpsSet && !(fpsNull && !hadFps) {
		f := int(min(fps, maxFpsCeil))
		if fpsNull {
			f = 0
		}
		send := f
		if send == 0 {
			send = maxFpsCeil
		}
		if !sendMaxFps(ns, send) {
			http.Error(w, "agent control channel not open", http.StatusConflict)
			return
		}
		ns.mu.Lock()
		ns.maxFps = f
		ns.mu.Unlock()
		log.Printf("max_fps [node=%s]: -> %d", ns.nodeID, send)
	}

	// Стелі тулбара належать глядачам (clearViewerCapsLocked). Глядачів зараз
	// нема — останній пішов, поки ми застосовували, або їх не було від початку:
	// стеля нічия і не має дістатись наступному, тож знімаємо тим самим шляхом.
	ns.mu.Lock()
	orphaned := len(ns.viewers) == 0 && (ns.capBps > 0 || ns.maxFps > 0)
	lateFps := false
	if orphaned {
		lateFps = clearViewerCapsLocked(ns)
	}
	ns.mu.Unlock()
	if lateFps {
		sendMaxFps(ns, maxFpsCeil)
	}

	resp := outputsSnapshot(ns)
	resp.Streams = liveStreams(ns.nodeID)
	writeJSON(w, resp)
}

// controlRequiresInput — SEC #26, env OO_SCREEN_CONTROL_REQUIRES_INPUT=1.
func controlRequiresInput() bool {
	return os.Getenv("OO_SCREEN_CONTROL_REQUIRES_INPUT") == "1"
}
