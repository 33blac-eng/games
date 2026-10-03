// hub-webrtc — кандидат A (T1). Приймає H.264 RTP від агентів, ретранслює
// на viewer-ногу (RTP-forward, не P2P): окремі PeerConnection на hub для
// кожної ноги, host-only ICE (localhost). NACK/RTX термінується локально на
// кожній нозі через дефолтний interceptor-ланцюг Pion. Вгору до агента з
// viewer-ноги пропускається лише PLI.
//
// МУЛЬТИ-PUBLISHER: hub тримає РЕЄСТР publisher-ів (мапа node_id ->
// nodeSession). Кожен агент реєструється зі своїм node (прапорець -node на
// oo-agent, поле "node" в offer; фолбек — env OO_SCREEN_AGENT_NODE_ID для
// одного T1-агента). Viewer маршрутизується до publisher-а САМЕ своєї ноди
// (node_id з тікета). Немає publisher для ноди -> fail-closed (viewer впаде на
// Mesh-фолбек). Ноди повністю ізольовані: reconnect/replace однієї ноди не
// чіпає інші; кілька viewer на РІЗНІ ноди працюють паралельно.
//
// FANOUT (Ф1): кілька viewer на ОДНУ ноду теж працюють — RTP від агента
// розкладається по ВСІХ живих viewer-ногах цієї ноди (fanout.go). Новий глядач
// ДОДАЄТЬСЯ, а не заміщає наявного.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/hub"
	"github.com/organicoils/oo-screen/internal/cursorproto"
)

const (
	defaultListenAddr = ":4470"
	// Main, не High: браузери оголошують Main (4d001f) повсюдно, а High
	// частина приймачів не оголошує ВЗАГАЛІ — 05.09 живий Chrome дав
	// 42001f/42e01f/4d001f/f4001f і жодного 64xx, через що хаб чесно віддав
	// 415, а фронт мовчки пішов у MeshCentral без звуку. Хаб не
	// перекодовує, тож його оголошений профіль мусить збігатися з тим, чим
	// кодує агент (той теж переведений на Main).
	h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"

	// seamlessTSStep — на скільки тактів 90 кГц зсувається egress-timestamp на
	// заміні агента (H-07). Один кадр при 30 fps: достатньо, щоб час лишався
	// строго зростаючим, і замало, щоб глядач вирішив, ніби потік завмер.
	// Реальний темп кадрів після цього кроку веде вже новий агент.
	seamlessTSStep = 3000
)

// envDuration — тривалість зі змінної середовища у форматі time.ParseDuration
// ("90m", "2h"). Нерозбірне значення — це друкарська помилка людини, і мовчки
// підставити типове означало б дати їй повірити, що налаштування діє.
func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("%s=%q не розібралось (%v) — беру типове %s", k, v, err, def)
		return def
	}
	return d
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const defaultToken = "t1-dev-token"

var token = envOr("OO_SCREEN_T1_TOKEN", defaultToken)

// listenAddr — адреса HTTP-сигналінгу. Дефолт той самий :4470, тож ні бенчі, ні
// web/, ні агент нічого не помічають; env потрібен, щоб на одній машині можна
// було підняти ДРУГИЙ хаб (навантажувальний прогін поруч із робочим), не
// відбираючи порт у першого — мовчазний bind-конфлікт інакше виглядає як
// «вимірювання відбулось», хоча міряли чужий процес.
var listenAddr = envOr("OO_SCREEN_HUB_ADDR", defaultListenAddr)

// ticket-режим вмикається наявністю OO_SCREEN_ERP_BASE. Без нього hub лишається
// повністю T1-сумісним (статичний token, як і раніше) — БЛОКЕР #2 закривається
// лише коли ERP-контур розгорнутий; бенч і T1-демо продовжують працювати без нього.
var (
	erpBase = envOr("OO_SCREEN_ERP_BASE", "")
	hubKey  = envOr("OO_SCREEN_HUB_KEY", "")
	// OO_SCREEN_AGENT_NODE_ID — фолбек node identity для ОДНОГО агента, який
	// підключається БЕЗ поля "node" в offer (старий T1/бенч-агент). Агент, що
	// несе власний -node, реєструється під тим node, а не під цим env.
	// Порожній env + агент без node = node "" (T1-бенч): у ticket-режимі жоден
	// viewer з node у тікеті до нього не підключиться (fail-closed).
	agentNodeIDEnv = envOr("OO_SCREEN_AGENT_NODE_ID", "")

	// H-06: ОДИН UDP-порт ICE-mux на ВЕСЬ процес замість діапазону ефемерних
	// портів (див. коментар у newAPI, чому саме mux).
	//
	// Дефолт успадковує OO_SCREEN_UDP_PORT_MIN — нижню межу старого діапазону
	// 4544-4607. Це не косметика: на стендах, де цю змінну вже виставили,
	// «просто 4544» означало б тихий переїзд на порт, якого немає у firewall.
	// OO_SCREEN_ICE_PORT перекриває результат явно; 4544 лишається дефолтом і
	// лежить усередині старого блоку, тож правил firewall міняти не треба.
	icePort = envPort("OO_SCREEN_ICE_PORT", envPort("OO_SCREEN_UDP_PORT_MIN", 4544))

	// H-19. Досі хаб роздавав ВИКЛЮЧНО host-кандидати по UDP. Для флоту в одній
	// мережі цього досить, але глядач за корпоративним firewall-ом, який ріже
	// вихідний UDP, не піднімав ICE ніколи — і бачив не помилку, а просто
	// «не працює». Ручки нижче це лікують і УСІ вимкнені за замовчуванням:
	// порожній env = поведінка бітово та сама, що й до H-19. Нічого не
	// вмикається саме, бо і TCP-порт, і TURN — це правила firewall і чужа
	// інфраструктура, а їх наосліп не міняють.
	//
	// OO_SCREEN_ICE_TCP_PORT — порт ICE-TCP. 0 (дефолт) = TCP не слухаємо
	// зовсім і NetworkTypes лишаються суто UDP.
	iceTCPPort = envPort("OO_SCREEN_ICE_TCP_PORT", 0)
	// OO_SCREEN_STUN_URLS — список через кому ("stun:host:3478,stun:...").
	stunURLs = envOr("OO_SCREEN_STUN_URLS", "")
	// TURN — усі три змінні мають сенс лише разом; будь-яка порожня вимикає.
	turnURL  = envOr("OO_SCREEN_TURN_URL", "")
	turnUser = envOr("OO_SCREEN_TURN_USER", "")
	turnPass = envOr("OO_SCREEN_TURN_PASS", "")
)

// iceServers — конфігурація ICE для КОЖНОЇ PeerConnection. Порожній зріз (а не
// nil-Configuration з дефолтами pion) означає «жодних STUN/TURN», тобто рівно
// той стан, у якому хаб працював досі.
func iceServers() []webrtc.ICEServer {
	var out []webrtc.ICEServer
	for _, u := range strings.Split(stunURLs, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, webrtc.ICEServer{URLs: []string{u}})
		}
	}
	if turnURL != "" && turnUser != "" && turnPass != "" {
		out = append(out, webrtc.ICEServer{
			URLs:           []string{turnURL},
			Username:       turnUser,
			Credential:     turnPass,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}
	return out
}

// envPort — номер порту з env. Парситься саме в 16 біт: інакше 70000 мовчки
// стало б 4464, і хаб слухав би не там, де сказали. Не число, нуль або
// завелике — лишається дефолт.
func envPort(k string, def uint16) uint16 {
	if v, err := strconv.ParseUint(os.Getenv(k), 10, 16); err == nil && v > 0 {
		return uint16(v)
	}
	return def
}

func ticketModeEnabled() bool {
	return erpBase != ""
}

type offerReq struct {
	SDP    string `json:"sdp"`
	Token  string `json:"token"`
	Ticket string `json:"ticket"`
	// SessionID — F-11: РЕНЕГОЦІАЦІЯ вже наявної viewer-ноги (ICE-restart)
	// замість нової. Непорожнє поле переводить /offer/viewer на гілку
	// renegotiateViewer: квиток НЕ споживається вдруге (він одноразовий), нова
	// PeerConnection не створюється, права не переоцінюються — бо й нога та
	// сама. Значення хаб видав у answer тієї ж ноги.
	SessionID string `json:"session_id"`
	// Node — mesh node_id публікуючого агента (agent-нога). Порожнє = фолбек на
	// env OO_SCREEN_AGENT_NODE_ID (один T1-агент). Viewer node НЕ береться
	// звідси — лише з claims тікета (ticket-режим) або env (T1).
	Node string `json:"node"`
	// Bitrate — фактичний стартовий -bitrate агента, біт/с (лише agent-нога).
	// Це стеля адаптації (bitrate.go): вище стартового бітрейту сесії hub не
	// підіймає. 0/відсутнє = старий агент без поля -> фолбек startBitrateBps.
	Bitrate uint64 `json:"bitrate"`
	// Outputs/ActiveOutput — монітори ПК агента і той, що зараз у потоці (лише
	// agent-нога). Знає їх ЛИШЕ агент, тож hub їх просто запамʼятовує і віддає
	// консолі через /control — інакше перемикачу монітора нічим наповнитись.
	Outputs      []outputInfo `json:"outputs,omitempty"`
	ActiveOutput int          `json:"active_output"`
	// Audio — агент публікує другу доріжку зі звуком ПК (лише agent-нога).
	// Вивести це з SDP чи з приходу RTP не можна: агент із гейтингом не шле
	// медіа, поки немає глядача, тож «пакетів ще немає» і «звуку в нього
	// немає» виглядали б однаково — той самий глухий кут, через який 30.08
	// не заходив перший глядач. Відсутнє поле = старий агент без звуку, і
	// тоді глядач отримує запасний тон (audio.go).
	Audio bool `json:"audio,omitempty"`
}

// outputInfo — монітор ПК агента. Форма 1-в-1 з capture.OutputInfo, але
// СВОЯ: agent/capture — це cgo+DXGI, і тягнути її в hub заради чотирьох
// полів означало б прив'язати збірку хаба до Windows-заголовків.
type outputInfo struct {
	Index   int  `json:"index"`
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	Primary bool `json:"primary"`
}

type answerResp struct {
	SDP string `json:"sdp"`
	// SessionID — ідентифікатор ЦІЄЇ viewer-ноги для ренегоціації (F-11).
	// Порожній для agent-ноги.
	SessionID string `json:"session_id,omitempty"`
}

// nodeSession — стан ОДНІЄЇ ноди: агентська нога (publisher) + УСІ viewer-ноги
// цієї ноди + пересилка RTP від першої до других. Кожен node_id у реєстрі має
// власний nodeSession; ноди повністю ізольовані одна від одної.
type nodeSession struct {
	nodeID string // незмінний ключ у реєстрі

	mu      sync.Mutex
	agentPC *webrtc.PeerConnection
	// viewers — УСІ глядачі цієї ноди (fanout, див. fanout.go). Ключ — їхній
	// PeerConnection. Нога форвардиться лише поки vl.live (Connected + є
	// publisher); публікацію вмикає/вимикає тільки recomputeBinding().
	viewers   map[*webrtc.PeerConnection]*viewerLeg
	agentSSRC webrtc.SSRC // SSRC агентського треку (ціль для PLI вгору)

	// agentCtrl — control-DataChannel до агента (label "oosc-ctl"). Hub шле
	// сюди "resume"/"pause" за присутністю глядача (on-demand гейтинг): агент
	// кодує лише коли є viewer. nil, поки агент не відкрив канал (старий агент
	// без DataChannel → гейтингу немає, агент лишається always-on).
	agentCtrl *webrtc.DataChannel

	// agentInput — канал вводу до агента (label "oosc-input", input.go). Hub
	// пересилає сюди перевірені події глядача. nil без OO_SCREEN_INPUT і в
	// агента без цього каналу — тоді ввід просто нікуди не йде, а Mesh працює
	// як працював.
	agentInput *webrtc.DataChannel
	// agentChanPC — PeerConnection, якому належать agentCtrl/agentInput (H-09):
	// на Failed/Closed канали обнуляються лише якщо вони ще його.
	agentChanPC *webrtc.PeerConnection

	// tiles — кеш текстових тайлів поточного епізоду (tiles.go). Порожній
	// і невживаний без OO_SCREEN_TILES.
	tiles tilesCache

	// generation — покоління агентської ноги ЦІЄЇ ноди; гейтить старий read
	// loop при replace. Доступ лише атомарно (&ns.generation).
	generation uint64

	// ctlSeq — наскрізний seq control-повідомлень до агента цієї ноди
	// (bitrate_target, keyframe_request). Доступ лише атомарно (&ns.ctlSeq).
	ctlSeq uint64

	// bitrate — стан контролера адаптації бітрейту цієї ноди (див. bitrate.go);
	// startBps — фактичний стартовий -bitrate агента з offer (стеля адаптації),
	// 0 = агент поля не прислав -> фолбек startBitrateBps; lastKeyframeReq —
	// дебаунс keyframe_request. Усі три під ns.mu.
	bitrate         bitrateCtl
	startBps        uint64
	lastKeyframeReq time.Time

	// lastPLI — окремий годинник дебаунсу PLI від глядачів (pliGate, nack.go).
	// Під ns.mu.
	lastPLI time.Time

	// outputs/activeOutput — монітори цієї ноди з offer агента (outputs.go).
	// Під ns.mu.
	outputs      []outputInfo
	activeOutput int

	// agentProfile — profile-level-id (6 hex-цифр), який агент ЦІЄЇ ноди
	// оголосив у своєму offer-і. Порожній = невідомий (агент не назвав
	// профіль або назвав кілька різних). Саме це, а не глобальний
	// h264FmtpLine, хаб оголошує глядачам цієї ноди. Під ns.mu.
	agentProfile string

	// agentAudio — агент цієї ноди оголосив у offer, що шле звук
	// (offerReq.Audio). Вибір джерела для глядача (звук агента vs запасний
	// тон) робиться саме за цим полем — див. audioPump. Під ns.mu.
	agentAudio bool

	// rec — запис сесії цієї ноди у MKV (record.go). nil, поки OO_SCREEN_RECORD
	// не заданий. Ставиться разом з agent-ногою й знімається, коли її read loop
	// гине; атомарний вказівник, бо читає його ще й audioPump чужої горутини.
	rec atomic.Pointer[recorder]
	// viewerCount — скільки глядачів зараз у мапі; атомарно, бо читається на
	// кожному пакеті агентської ноги без ns.mu.
	viewerCount atomic.Int32

	// egress seq/ts — МОНОТОННІ на весь час життя ноди, ніколи не скидаються
	// при заміні агента (generation): для кожного вхідного пакета egress =
	// попередній egress + delta(вхід). haveEgress ініціалізується один раз, при
	// першому пакеті цієї ноди.
	haveEgress bool
	// egressGen — покоління агента, до ВХІДНОГО простору якого прив'язані
	// lastInSeq/lastInTS. H-07/H-27: новий агент починає з власного випадкового
	// seq/ts, тож delta від чужого відліку — це стрибок на пів-діапазону
	// uint16 вперед. Розбіжність із поточним generation означає «перший пакет
	// нового покоління»: переанкоруємо вхід і рухаємо egress на один крок.
	egressGen uint64
	// gop — кеш хвоста потоку від останнього IDR (пункт 41, gop.go). Пишеться
	// у forwardToViewers, читається у recomputeBinding, увесь стан під ns.mu.
	gop        gopCache
	lastInSeq  uint16
	lastInTS   uint32
	lastOutSeq uint16
	lastOutTS  uint32
}

// hasAgent — чи є ЖИВИЙ publisher у цієї ноди (для fail-closed на viewer-нозі).
func (ns *nodeSession) hasAgent() bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.agentPC != nil
}

// hasAgentAudio — чи оголосив агент цієї ноди звукову доріжку. Завжди false
// без OO_SCREEN_AUDIO: поле ставиться лише під прапорцем (setAgentAudio).
func (ns *nodeSession) hasAgentAudio() bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.agentAudio
}

// setAgentAudio запамʼятовує оголошення агента. Пишемо на КОЖНОМУ offer-і (у
// т.ч. після реконекту) з тієї ж причини, що й setOutputs: правда про агента —
// це його останній offer, а не той, з яким він колись підключився вперше.
func setAgentAudio(ns *nodeSession, on bool) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.agentAudio = on
}

// videoProfile — profile-level-id агента цієї ноди ("" = ще невідомий).
func (ns *nodeSession) videoProfile() string {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.agentProfile
}

// setAgentProfile запамʼятовує профіль з offer-а агента і повертає true, якщо
// він ЗМІНИВСЯ проти того, під яким уже могли домовитись наявні глядачі
// (порожній == дефолт хаба: саме його оголошують глядачам ноди без агента).
// Пишемо на КОЖНОМУ offer-і — правда про агента це його останній offer.
func setAgentProfile(ns *nodeSession, plid string) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	before := ns.agentProfile
	ns.agentProfile = plid
	return !sameProfile(effectiveProfile(before), effectiveProfile(plid))
}

// effectiveProfile — що саме хаб оголошує глядачеві при такому стані ноди.
// strictCodec — чи різати сесію при неузгодженому профілі H.264.
// Дефолт — НІ (лише журнал і лічильник у /healthz), див. коментар у
// обробнику offer/viewer.
func strictCodec() bool {
	v := strings.TrimSpace(os.Getenv("OO_SCREEN_STRICT_CODEC"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func effectiveProfile(plid string) string {
	if plid == "" {
		return wantedProfileLevelID
	}
	return plid
}

// sameProfile — profile_idc+profile_iop збігаються (рівень поза порівнянням,
// див. h264ProfileCompatible).
func sameProfile(a, b string) bool {
	aIDC, aIOP, ok1 := splitProfile(a)
	bIDC, bIOP, ok2 := splitProfile(b)
	return ok1 && ok2 && aIDC == bIDC && aIOP == bIOP
}

// h264FmtpFor — fmtp-рядок хаба для конкретного профілю ноди.
func h264FmtpFor(plid string) string {
	if plid == "" || plid == wantedProfileLevelID {
		return h264FmtpLine
	}
	return "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + plid
}

// registry — реєстр publisher-ів: node_id -> nodeSession. Мапа під власним
// мьютексом; кожен nodeSession має свій внутрішній мьютекс на per-node стан.
type registry struct {
	mu    sync.Mutex
	nodes map[string]*nodeSession
}

func newRegistry() *registry {
	return &registry{nodes: make(map[string]*nodeSession)}
}

// getOrCreate повертає nodeSession для node, створюючи його за відсутності.
// Використовує agent-нога (publisher реєструється) та viewer-нога у НЕ
// ticket-режимі (T1: viewer може прийти раніше за агента й чекати).
// nil — досягнуто стелі maxNodes (SEC #21).
func (r *registry) getOrCreate(nodeID string) *nodeSession {
	ns, _ := r.getOrCreateNew(nodeID)
	return ns
}

// getOrCreateNew — як getOrCreate, але ще й каже, чи нода створена саме зараз
// (SEC #17: невдалий offer мусить прибрати те, що сам створив).
func (r *registry) getOrCreateNew(nodeID string) (*nodeSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns := r.nodes[nodeID]
	if ns != nil {
		return ns, false
	}
	if len(r.nodes) >= maxNodes {
		return nil, false
	}
	ns = &nodeSession{nodeID: nodeID}
	r.nodes[nodeID] = ns
	return ns, true
}

// removeIfIdle прибирає ноду, якщо в неї немає ні агента, ні глядачів (SEC #17:
// невдалий agent-offer не лишає порожніх нод — інакше реєстр росте без меж).
func (r *registry) removeIfIdle(ns *nodeSession) {
	ns.mu.Lock()
	idle := ns.agentPC == nil && len(ns.viewers) == 0
	ns.mu.Unlock()
	if idle {
		r.remove(ns.nodeID, ns)
	}
}

// get повертає nodeSession для node або nil. Використовує viewer-нога у
// ticket-режимі: nil => немає publisher => fail-closed (403).
func (r *registry) get(nodeID string) *nodeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodes[nodeID]
}

// remove прибирає node зі реєстру, якщо там саме ця nodeSession (не пізніша,
// що встигла зайняти те саме node_id після повторного підключення агента).
func (r *registry) remove(nodeID string, ns *nodeSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodes[nodeID] == ns {
		delete(r.nodes, nodeID)
	}
}

// all повертає знімок УСІХ живих нод. Потрібне рівно одному викликачу —
// fail-closed за недоступності ERP (hub.RevokeKindStale): там рвати треба все,
// бо жоден дозвіл уже не підтверджений. Знімок під локом, а закриття — поза
// ним: closeNode кличе reg.remove, тобто той самий лок.
func (r *registry) all() []*nodeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*nodeSession, 0, len(r.nodes))
	for _, ns := range r.nodes {
		out = append(out, ns)
	}
	return out
}

// nodesForUser повертає всі nodeSession, де ХОЧА Б ОДНА жива viewer-нога
// належить цьому user_id (runtime-revoke kind="user"). Знімок під локом реєстру,
// потім без нього — не тримаємо reg.mu на час Close() кожної ноди.
func (r *registry) nodesForUser(userID string) []*nodeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*nodeSession
	for _, ns := range r.nodes {
		ns.mu.Lock()
		match := false
		for _, vl := range ns.viewers {
			if vl.userID == userID {
				match = true
				break
			}
		}
		ns.mu.Unlock()
		if match {
			out = append(out, ns)
		}
	}
	return out
}

// closeNode рве живі ноги ЦІЄЇ ноди (viewer + publisher) і прибирає її з
// реєстру. Викликається з runtime-revoke: hub нічого не вирішує сам, лише
// виконує "закрити" за наказом ERP (SubscribeRevoke -> onRevoke).
// PeerConnection.Close() сам зачищає ns.agentPC/ns.viewerPC через наявні
// OnConnectionStateChange-колбеки (Failed/Closed) — тут лише знімаємо
// поточні вказівники під локом ноди й закликаємо Close() поза ним.
func closeNode(ns *nodeSession) {
	ns.mu.Lock()
	agentPC := ns.agentPC
	viewerPCs := viewerPCsLocked(ns)
	ns.mu.Unlock()

	if agentPC != nil {
		_ = agentPC.Close()
	}
	// Fanout: рвемо ВСІ viewer-ноги ноди, не одну.
	for _, pc := range viewerPCs {
		_ = pc.Close()
	}

	reg.remove(ns.nodeID, ns)
	forgetRelays(ns)
	log.Printf("runtime-revoke: node=%s closed (%d viewer leg(s))", ns.nodeID, len(viewerPCs))
}

// dropUserViewers рве ноги САМЕ цього користувача на цій ноді (runtime-revoke
// kind="user"). З fanout-ом закривати всю ноду тут не можна: у неї можуть
// дивитись інші, законні глядачі — відкликання одного не має ні класти
// publisher-а, ні вибивати решту.
func dropUserViewers(ns *nodeSession, userID string) {
	ns.mu.Lock()
	var legs []*viewerLeg
	for _, vl := range ns.viewers {
		if vl.userID == userID {
			legs = append(legs, vl)
		}
	}
	ns.mu.Unlock()

	for _, vl := range legs {
		dropViewer(ns, vl, "runtime-revoke user="+userID)
	}
}

var (
	reg             = newRegistry()
	agentSeqSample  uint64
	viewerSeqSample uint64
)

// iceMux тримає ЄДИНИЙ на процес ICE UDP mux (H-06). Зберігаємо не сам
// ice.UDPMux, а замикання, яке його прив'язує: так пакет pion/ice не стає
// прямим імпортом хаба, а тип лишається під контролем webrtc/v4.
var iceMux struct {
	once  sync.Once
	apply func(*webrtc.SettingEngine)
	err   error
}

// applyICEUDPMux прив'язує спільний mux до SettingEngine цього offer'а. Перший
// виклик відкриває сокет, решта перевикористовують його. Помилка bind — це
// фатально для ВСЬОГО процесу (порт зайнятий), тому вона запам'ятовується і
// повертається кожному наступному offer'у, а не мовчки ковтається: інакше хаб
// піднявся б і роздавав кандидатів, до яких ніхто не достукається.
func applyICEUDPMux(se *webrtc.SettingEngine) error {
	iceMux.once.Do(func() {
		// IP не задаємо: wildcard-bind ловить і v4, і v6 — під SetNetworkTypes
		// вище. Фільтрацію адрес у кандидатах робить SetIPFilter, а не bind.
		conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: int(icePort)})
		if err != nil {
			iceMux.err = fmt.Errorf("ICE UDP mux :%d: %w", icePort, err)
			return
		}
		mux := webrtc.NewICEUDPMux(nil, conn)
		iceMux.apply = func(se *webrtc.SettingEngine) { se.SetICEUDPMux(mux) }
		log.Printf("ICE UDP mux слухає :%d (один сокет на всі ноги)", icePort)
	})
	if iceMux.err != nil {
		return iceMux.err
	}
	iceMux.apply(se)
	return nil
}

// iceTCPMux — те саме, що iceMux, але для ICE-TCP (H-19). Окремий Once, бо
// вмикається окремою змінною і може лишатись вимкненим назавжди.
var iceTCPMux struct {
	once  sync.Once
	apply func(*webrtc.SettingEngine)
	err   error
}

// iceTCPReadBuffer — скільки пакетів тримати в буфері одного TCP-стріму до
// того, як читач їх забере. 8 — типове для pion; ICE-TCP тут запасний шлях для
// глядача за firewall-ом, а не основний, тож глибший буфер лише з'їдав би
// пам'ять на кожній нозі.
const iceTCPReadBuffer = 8

// applyICETCPMux вішає спільний ICE-TCP слухач на SettingEngine цього offer'а.
// Без OO_SCREEN_ICE_TCP_PORT не робить НІЧОГО — жодного сокета, жодного
// кандидата: так виглядає «поведінка як зараз».
func applyICETCPMux(se *webrtc.SettingEngine) error {
	if iceTCPPort == 0 {
		return nil
	}
	iceTCPMux.once.Do(func() {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", iceTCPPort))
		if err != nil {
			iceTCPMux.err = fmt.Errorf("ICE TCP mux :%d: %w", iceTCPPort, err)
			return
		}
		mux := webrtc.NewICETCPMux(nil, ln, iceTCPReadBuffer)
		iceTCPMux.apply = func(se *webrtc.SettingEngine) { se.SetICETCPMux(mux) }
		log.Printf("ICE TCP mux слухає :%d (запасний шлях для глядача за firewall-ом)", iceTCPPort)
	})
	if iceTCPMux.err != nil {
		return iceTCPMux.err
	}
	iceTCPMux.apply(se)
	return nil
}

// newAPI будує API для ОДНІЄЇ ноги. profile — profile-level-id, який ця нога
// оголошує ("" = дефолт h264FmtpLine). Хаб не перекодовує, тож глядачеві ноди
// оголошується рівно те, чим кодує агент САМЕ ЦІЄЇ ноди.
func newAPI(profile string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: h264FmtpFor(profile),
			// Без цього interceptor-ланцюг Pion не будує NACK/PLI generator+responder
			// для нашого кастомного PT 102 — RTCPFeedback з RTPCodecParameters мапиться
			// напряму в interceptor.RTPCodecCapability (mediaengine.go RegisterCodec ->
			// interceptor.go newFeedbackFromCodec, ~line 340).
			//
			// ПУНКТ 40: goog-remb тепер Є. Без нього браузер не шле жодної
			// оцінки смуги, і єдиним входом контролера лишався FractionLost із
			// RR — тобто збиток, який УЖЕ стався.
			//
			// transport-cc свідомо НЕ додаємо, і це не забудькуватість:
			// Chrome шле REMB лише поки transport-cc НЕ узгоджено. Оголосити
			// його, не маючи в хабі оцінювача смуги над TWCC-звітами, означало
			// б ВИМКНУТИ єдину оцінку, якою ми вміємо керувати, і отримати
			// натомість потік зворотного звʼязку, який нікуди не йде.
			// Апгрейд робиться на місці: зʼявиться оцінювач — додається
			// {Type: "transport-cc"} і ConfigureTWCCSender.
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"},
				{Type: "nack", Parameter: "pli"},
				{Type: "goog-remb"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	// Opus — лише під OO_SCREEN_AUDIO (audio.go). Без прапорця MediaEngine
	// лишається бітово тим самим, що й до появи звуку.
	if err := registerAudioCodec(m); err != nil {
		return nil, err
	}

	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}

	se := webrtc.SettingEngine{}
	// Базово — host-кандидати по UDP, як і було. H-19: TCP додається лише тоді,
	// коли для нього ЯВНО задано порт (див. applyICETCPMux нижче); порожній env
	// лишає рівно колишню поведінку.
	netTypes := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6}
	if iceTCPPort != 0 {
		netTypes = append(netTypes, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6)
	}
	se.SetNetworkTypes(netTypes)
	// H-06: ICE UDP mux — ОДИН сокет на ВСІ ноги процесу замість «порт на ногу».
	//
	// Чому саме mux, а не ширший діапазон. Було SetEphemeralUDPPortRange(
	// 4544-4607): кожна нога (agent + КОЖЕН viewer) брала рівно один порт із
	// блоку, тобто 64 порти == 64 одночасні ноги на ВЕСЬ процес — ~63 глядачі
	// на весь флот, а не на ноду. Це замірено, а не виведено: на 50 глядачах
	// доставка 100%, на 100 — ICE-ноги масово падають у failed, і в
	// Get-NetUDPEndpoint видно всі 64 порти блоку зайнятими. Розширення блоку
	// лише відсуває стелю і тягне за собою нові правила firewall на кожен
	// доданий порт; mux прибирає стелю як таку — кількість ніг більше не
	// впирається в кількість портів, бо порт один.
	//
	// Порт береться з env саме тому, що firewall наосліп міняти не можна:
	// дефолт 4544 лежить усередині старого блоку 4544-4607, тож уже відкритий,
	// і netem.sh (tc u32 filter на конкретні порти) навіть спрощується — ціль
	// одна замість шістдесяти чотирьох.
	//
	// Сокет спільний на процес (iceMuxOnce), бо newAPI() кличеться на КОЖЕН
	// offer: створювати тут новий ListenUDP означало б «address already in use»
	// на другому ж глядачі.
	if err := applyICEUDPMux(&se); err != nil {
		return nil, err
	}
	// H-19: TCP-кандидати. Той самий mux-підхід — один слухач на процес.
	if err := applyICETCPMux(&se); err != nil {
		return nil, err
	}
	// На VPS кандидат A публікує host-кандидати з публічною IP замість
	// внутрішньої; локально (OO_SCREEN_PUBLIC_IP не задано) поведінка не
	// змінюється.
	if ip := os.Getenv("OO_SCREEN_PUBLIC_IP"); ip != "" {
		se.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeHost)
		// H-34: приватні адреси хоста (docker0, lo) у кандидатах — сміття для
		// глядача і зайвий шлях перебору. Лишаємо лише публічну.
		se.SetIPFilter(func(ip net.IP) bool { return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() })
	}

	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(se)), nil
}

func corsHeaders(w http.ResponseWriter) {
	// H-29: «*» на публічному сигналінгу — будь-який сайт міг слати offer
	// з квитком, вкраденим із вкладки. Дозволяємо лише origin ЕРП; без
	// ERP_BASE (стенд) — як було.
	origin := "*"
	if erpBase != "" {
		if u, err := url.Parse(erpBase); err == nil && u.Scheme != "" && u.Host != "" {
			origin = u.Scheme + "://" + u.Host
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// authorizeViewer — ЄДИНА авторизація глядача на цьому хабі. Її звуть і
// /offer/viewer (дивитись), і /control (перемкнути монітор), саме однією
// функцією: два входи в один і той самий ПК не сміють мати двох різних правд про
// те, кого пускати. Повертає (ns, claims, 0, "") при успіху; інакше — HTTP-статус
// і текст, які виклик віддає як є.
//
// Ticket-режим (прод): одноразовий ERP-квиток -> claims -> node з claims (а не з
// тіла запиту) -> нода мусить мати ЖИВОГО publisher-а. Fail-closed на кожному
// кроці. T1 static-token режим лишається як був — hub у ньому голосно пише
// WARNING і для проду не призначений.
func authorizeViewer(req offerReq) (*nodeSession, *hub.TicketClaims, int, string) {
	if !ticketModeEnabled() {
		// Viewer-нога, T1 static-token режим: node з env (один агент).
		// getOrCreate дозволяє viewer прийти раніше за агента й чекати
		// (recomputeBinding не опублікує трек, доки немає publisher-а).
		if !tokenMatches(req.Token) {
			return nil, nil, http.StatusUnauthorized, "bad token"
		}
		ns := reg.getOrCreate(agentNodeIDEnv)
		if ns == nil {
			return nil, nil, http.StatusServiceUnavailable, "too many nodes"
		}
		return ns, nil, 0, ""
	}

	// hardening (blocker-2): static-token гілка для viewer у проді
	// (ERP_BASE заданий) НЕДОСЯЖНА — без req.Ticket завжди 403.
	if req.Ticket == "" {
		return nil, nil, http.StatusForbidden, "ticket required"
	}
	claims, err := hub.ConsumeTicket(erpBase, hubKey, req.Ticket)
	if err != nil {
		log.Printf("viewer ticket consume failed: %v", err)
		return nil, nil, http.StatusForbidden, "ticket consume failed"
	}
	// Node-binding fail-closed: порожній node у тікеті = агент без node
	// недосяжний; publisher має існувати ДО viewer-а (інакше viewer
	// впаде на Mesh-фолбек).
	node := claims.NodeID
	if node == "" {
		log.Printf("viewer ticket has empty node_id, fail-closed")
		return nil, nil, http.StatusForbidden, "node required"
	}
	ns := reg.get(node)
	if ns == nil || !ns.hasAgent() {
		log.Printf("no publisher for node %q, fail-closed (viewer -> mesh fallback)", node)
		return nil, nil, http.StatusNotFound, "no publisher for node"
	}
	return ns, claims, 0, ""
}

func handleOffer(leg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost { // H-30
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// H-03: публічний порт без стелі на тіло = O(память) на запит.
		r.Body = http.MaxBytesReader(w, r.Body, maxOfferBody)
		var req offerReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}

		// F-11: ренегоціація наявної ноги йде ОКРЕМИМ шляхом — без нової
		// PeerConnection і без другого споживання квитка.
		if leg == "viewer" && req.SessionID != "" {
			renegotiateViewer(w, req)
			return
		}

		// ns — nodeSession, до якого належить ця нога. Визначається ПІСЛЯ
		// автентифікації (viewer: node з тікета; agent: node з offer/env).
		var (
			ns           *nodeSession
			viewerClaims *hub.TicketClaims
			// viewerTicket — той самий одноразовий квиток, яким відкрилась ця
			// нога. Кожне повідомлення каналу вводу мусить нести саме його
			// (input.go); порожній = ввід недоступний цій нозі взагалі.
			viewerTicket string
			// sessionID — ключ ренегоціації цієї viewer-ноги (F-11), їде в answer.
			sessionID string
			// legProfile — profile-level-id, який ця нога оголошує. Agent-нога
			// бере його зі свого ж offer-а, viewer-нога — з ноди.
			legProfile string
			// agentCreated — agent-offer створив ноду сам (SEC #17); answered —
			// дійшли до answer. Невдалий offer прибирає створену ним ноду.
			agentCreated bool
			answered     bool
		)
		defer func() {
			if agentCreated && !answered {
				reg.removeIfIdle(ns)
			}
		}()

		switch {
		case leg == "viewer":
			var status int
			var msg string
			ns, viewerClaims, status, msg = authorizeViewer(req)
			viewerTicket = req.Ticket
			if status != 0 {
				http.Error(w, msg, status)
				return
			}
			// SEC: стеля глядачів на ноду. Без неї кожна viewer-нога = PeerConnection,
			// черга й дві горутини без жодної межі (DoS памʼяттю/CPU хаба).
			if viewerCapReached(ns) {
				http.Error(w, "too many viewers for node", http.StatusTooManyRequests)
				return
			}
			legProfile = ns.videoProfile()
			// Агент є, а профілю немає — це «не знаю, що я тобі віддам».
			// Мовчки підставити дефолт означало б домовитись про Main і
			// віддати High: глядач бачить сірий екран замість відмови.
			if legProfile == "" && ns.hasAgent() {
				rejectedProfileTotal.Add(1)
				rejectedProfileAt.Store(time.Now().Unix())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnsupportedMediaType)
				_ = json.NewEncoder(w).Encode(&codecMismatch{
					Error:  profileUnknownYet,
					Detail: "агент ноди не оголосив profile-level-id — хаб не знає, який профіль він шле, і не вигадує його",
				})
				log.Printf("offer/viewer [node=%s]: %s", ns.nodeID, profileUnknownYet)
				return
			}
		default:
			// Agent-нога: токен НОДИ (SEC #17, agentauth.go) або легасі-спільний.
			// Node з offer (поле "node"), фолбек — env OO_SCREEN_AGENT_NODE_ID.
			node := req.Node
			if node == "" {
				node = agentNodeIDEnv
			}
			if !agentAuthorized(node, req.Token) {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			ns, agentCreated = reg.getOrCreateNew(node)
			if ns == nil {
				log.Printf("offer/agent [node=%s]: стеля нод %d (OO_SCREEN_MAX_NODES) — відмова", node, maxNodes)
				http.Error(w, "too many nodes", http.StatusServiceUnavailable)
				return
			}
			// Стартовий бітрейт агента = стеля адаптації (bitrate.go). Старий
			// агент поля не шле — лишається фолбек startBitrateBps.
			if req.Bitrate > 0 {
				setStartBitrate(ns, req.Bitrate)
			}
			// Монітори цієї ноди — рівно те, що агент бачить у себе. Пишемо на
			// КОЖНОМУ offer-і (у т.ч. після реконекту): це заодно єдине місце, де
			// оптимістичний active хаба звіряється з правдою агента.
			setOutputs(ns, req.Outputs, req.ActiveOutput)
			// Чи є в агента звук. Під прапорцем — і лише під ним: із
			// вимкненим OO_SCREEN_AUDIO поле лишається false, аудіо-доріжки
			// в глядача немає взагалі, і читати це нема кому.
			setAgentAudio(ns, audioEnabled && req.Audio)
			// Профіль H.264 ЦІЄЇ ноди — з її ж offer-а. Змінився (агент
			// перезібрано з іншим профілем) — рвемо наявні viewer-ноги: вони
			// домовились про старий профіль, і мовчки лишити їх означало б
			// віддавати потік, який вони не декодують.
			legProfile = sdpVideoProfile(req.SDP)
			if setAgentProfile(ns, legProfile) {
				if n := dropAllViewers(ns, "профіль H.264 ноди змінився"); n > 0 {
					log.Printf("offer/agent [node=%s]: профіль %q — рву %d viewer-ніг зі старим профілем",
						ns.nodeID, effectiveProfile(legProfile), n)
				}
			}
			if legProfile == "" {
				log.Printf("offer/agent [node=%s]: agent не назвав profile-level-id — глядачі цієї ноди отримають %s",
					ns.nodeID, profileUnknownYet)
			}
		}

		api, err := newAPI(legProfile)
		if err != nil {
			internalError(w, "newAPI", err)
			return
		}
		// H-19: ICEServers порожні, доки не задано OO_SCREEN_STUN_URLS/TURN_*.
		pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers()})
		if err != nil {
			internalError(w, "NewPeerConnection", err)
			return
		}
		// H-04: кожен error-path нижче раніше лишав PeerConnection (ICE-агент,
		// UDP-сокети, горутини) жити назавжди. Закриваємо, якщо не дійшли до answer.
		defer func() {
			if !answered {
				_ = pc.Close()
			}
		}()

		switch leg {
		case "agent":
			if err := setupAgentLeg(ns, pc); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "viewer":
			var err error
			if sessionID, err = setupViewerLeg(ns, pc, viewerClaims, viewerTicket, legProfile); err != nil {
				if errors.Is(err, errViewerCap) {
					http.Error(w, "too many viewers for node", http.StatusTooManyRequests)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.SDP}
		if err := pc.SetRemoteDescription(offer); err != nil {
			// Це вхід клієнта: поганий SDP — 400 з коротким текстом, без нутрощів pion.
			http.Error(w, "bad offer sdp", http.StatusBadRequest)
			log.Printf("offer/%s: SetRemoteDescription: %v", leg, err)
			return
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			internalError(w, "CreateAnswer", err)
			return
		}
		// H-18: домовитись про відео могло і не вийти. Якщо глядач не запропонував
		// H.264 із нашим profile-level-id, узгодження не падає з помилкою — pion
		// просто викидає відеодоріжку з answer або лишає м-лінію без спільного
		// кодека. Далі все виглядало здоровим: 200 OK, ICE піднімається, RTP
		// летить — і людина нескінченно дивиться на сірий екран, бо декодувати
		// цей потік її браузеру нічим. Ловимо це ТУТ і кажемо причину вголос,
		// поки є кому її прочитати.
		if leg == "viewer" {
			if why := videoCodecMismatch(answer.SDP, effectiveProfile(legProfile)); why != nil {
				rejectedProfileTotal.Add(1)
				rejectedProfileAt.Store(time.Now().Unix())
				log.Printf("offer/viewer [node=%s]: %s: %s", ns.nodeID, why.Error, why.Detail)
				// 🚨 За замовчуванням ЛИШЕ ПОПЕРЕДЖЕННЯ, не 415. Ціна знята з
				// живих людей 05.09.2026: перша редакція H-18 віддавала 415, і
				// за півгодини вісім реальних спроб відкрити екран на двох ПК
				// отримали відмову. Chrome оголошує Constrained High як 640c1f
				// (profile_idc 0x64, iop 0x0c), хаб шле 64002a (iop 0x00) —
				// за правилом «прапорці глядача мусять бути підмножиною наших»
				// це не збіг, хоча декодер той самий і до H-18 такі сесії
				// відкривались.
				//
				// Тобто гіпотеза «неузгоджений кодек = сірий екран» так і
				// лишилась НЕ ДОВЕДЕНОЮ на бою, а ціна хибного спрацювання —
				// повна відмова в доступі. Тому детекція лишається і кричить у
				// журнал та в /healthz, а різати сесію дозволено лише явно:
				// OO_SCREEN_STRICT_CODEC=1. Вмикати — коли буде запис, де саме
				// цей мисматч дав сірий екран.
				if strictCodec() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnsupportedMediaType)
					_ = json.NewEncoder(w).Encode(why)
					return
				}
			}
		}
		gatherComplete := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(answer); err != nil {
			internalError(w, "SetLocalDescription", err)
			return
		}
		// H-05: gathering без дедлайну в HTTP-хендлері = завислий запит і
		// завислий PC. Host-only кандидати збираються за мілісекунди; 5 с —
		// це вже поламаний мережевий стек, а не повільна мережа.
		select {
		case <-gatherComplete:
		case <-time.After(gatherTimeout):
			http.Error(w, "ICE gathering timeout", http.StatusGatewayTimeout)
			return
		}

		answered = true
		writeJSON(w, answerResp{SDP: pc.LocalDescription().SDP, SessionID: sessionID})
	}
}

// rejectedProfileTotal / rejectedProfileAt — скільки глядачів хаб відхилив за
// неузгоджений відеокодек і коли востаннє (unix-секунди, 0 = жодного разу).
// Видно в /healthz, щоб deploy/ops/oo_screen_health.py міг кричати: тихий
// фолбек фронта в MeshCentral більше не мусить бути єдиним слідом.
var (
	rejectedProfileTotal atomic.Int64
	rejectedProfileAt    atomic.Int64
)

// internalError — H-17: текст помилки йде в журнал, назовні — лише код.
func internalError(w http.ResponseWriter, where string, err error) {
	log.Printf("offer: %s: %v", where, err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// maxViewersPerNode — стеля одночасних viewer-ніг однієї ноди (SEC-аудит).
// Env OO_SCREEN_MAX_VIEWERS перекриває; невалідне/нульове — дефолт 16.
var maxViewersPerNode = func() int {
	if v, err := strconv.Atoi(os.Getenv("OO_SCREEN_MAX_VIEWERS")); err == nil && v > 0 {
		return v
	}
	return 16
}()

// errViewerCap — стелю досягнуто вже під час реєстрації ноги (гонка
// паралельних /offer повз ранню перевірку viewerCapReached).
var errViewerCap = errors.New("too many viewers for node")

// viewerCapReached — чи вже досягнуто стелі глядачів ноди.
func viewerCapReached(ns *nodeSession) bool {
	ns.mu.Lock()
	n := len(ns.viewers)
	ns.mu.Unlock()
	return n >= maxViewersPerNode
}

// maxOfferBody — стеля тіла /offer/* і /control (SDP ~ 3–10 КБ; 256 КБ = запас).
const maxOfferBody = 256 << 10

// gatherTimeout — дедлайн збору ICE-кандидатів у HTTP-хендлері (H-05).
const gatherTimeout = 5 * time.Second

// tokenTail — останні 4 символи токена для журналу: досить, щоб звірити «той
// самий», замало, щоб підібрати.
func tokenTail() string {
	if len(token) <= 4 {
		return "????"
	}
	return token[len(token)-4:]
}

// tokenMatches — порівняння токена сталим часом (H-20): проста рівність рядків
// виходить на першому розбіжному байті і дає таймінг-оракул на публічному порту.
func tokenMatches(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// dropAllViewers рве всі viewer-ноги ноди, НЕ чіпаючи агента (H-13): stale-ERP
// означає «не можу підтвердити дозволи глядачів», а не «агент нелегітимний».
// Закривати агентів усього парку одним махом = thundering herd реконектів.
func dropAllViewers(ns *nodeSession, reason string) int {
	ns.mu.Lock()
	legs := make([]*viewerLeg, 0, len(ns.viewers))
	for _, vl := range ns.viewers {
		legs = append(legs, vl)
	}
	ns.mu.Unlock()
	for _, vl := range legs {
		dropViewer(ns, vl, reason)
	}
	return len(legs)
}

// setupAgentLeg — приймає H.264 track від агента ЦІЄЇ ноди, ретранслює RTP у
// viewer-ногу цієї ноди (якщо вона вже підʼєднана), обробляє RTCP агентської
// ноги. Працює лише над переданим ns — інші ноди не чіпає.
func setupAgentLeg(ns *nodeSession, pc *webrtc.PeerConnection) error {
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return err
	}
	// Приймальна сторона звуку агента — ЛИШЕ під прапорцем. Агент тут
	// offerer, тож зайвий локальний трансивер без m-рядка в offer-і просто не
	// потрапить у відповідь: старий агент без звуку нічого не помітить.
	if audioEnabled {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			return err
		}
	}

	// Control-DataChannel від агента (on-demand гейтинг). Коли відкриється —
	// одразу шлемо поточний стан присутності глядача, щоб агент, який щойно
	// (пере)підключився, миттєво знав: кодувати чи простоювати.
	tilesOn := tilesEnabled // знімок прапорця: колбеки живуть довше за виклик
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "oosc-ctl":
			ns.mu.Lock()
			ns.agentCtrl, ns.agentChanPC = dc, pc
			ns.mu.Unlock()
			// Пульс заводиться разом із гейтом і живе рівно стільки, скільки
			// цей канал (heartbeat.go). Без нього агент на паузі не має ЖОДНОЇ
			// ознаки, що хаб іще живий.
			dc.OnOpen(func() {
				sendGate(ns)
				startHubHeartbeat(ns, dc)
			})
		case inputChannelLabel:
			// Ввід від глядача (input.go) — ЛИШЕ під прапорцем. З вимкненим
			// OO_SCREEN_INPUT агент цього каналу й не відкриває, а якби відкрив
			// (пара агент+хаб роз'їхалась) — хаб його просто не бере, і ввід
			// нікуди не піде.
			if !inputEnabled {
				return
			}
			ns.mu.Lock()
			ns.agentInput, ns.agentChanPC = dc, pc
			ns.mu.Unlock()
			log.Printf("input: agent channel open [node=%s]", ns.nodeID)
		case tilesLabel:
			// Текстові тайли (tiles.go) — лише під OO_SCREEN_TILES.
			if !tilesOn {
				return
			}
			log.Printf("tiles: agent channel open [node=%s]", ns.nodeID)
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				if msg.IsString {
					return
				}
				onAgentTiles(ns, msg.Data)
			})
		case cursorproto.ChannelLabel:
			// Шар курсора (cursor.go): агент із -cursor-layer.
			attachAgentRelay(ns, dc, cursorRelayConfig())
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// Звук іде окремою доріжкою того самого зʼєднання — і рівно тому
		// власного publisher-стану, SSRC, generation чи запису не має: усе це
		// веде відео. Тут лише читач і фанаут (audio.go).
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			log.Printf("agent audio track [node=%s]: %s", ns.nodeID, track.Codec().MimeType)
			go drainRTCP(receiver.Read, "agent-audio-rtcp")
			go readAgentAudio(ns, track)
			return
		}

		ns.mu.Lock()
		prevPC := ns.agentPC
		ns.agentPC = pc
		ns.agentSSRC = webrtc.SSRC(track.SSRC())
		ns.mu.Unlock()
		myGen := atomic.AddUint64(&ns.generation, 1)
		recomputeBinding(ns)

		// Заміна агента ЦІЄЇ ноди: старий read loop не має жити далі —
		// закриваємо стару PeerConnection (це рве ReadRTP старого loop-у
		// помилкою) і додатково гейтимо loop по generation ID цієї ноди, щоб не
		// було вікна інтерлівінгу навіть якщо Close() не встиг зупинити читання
		// одразу. Інші ноди мають власні generation — їх це не торкається.
		if prevPC != nil && prevPC != pc {
			_ = prevPC.Close()
		}

		// Обовʼязковий RTCP read-loop агентської ноги — інакше interceptors мертві.
		go drainRTCP(receiver.Read, "agent-rtcp")

		// Запис сесії (record.go). nil без OO_SCREEN_RECORD, і тоді все нижче —
		// два порівняння з nil. Файл закриває defer ТІЄЇ Ж горутини, що читає
		// RTP: інших виходів у неї немає, тож розрив агента, витіснення новим
		// агентом і closeNode ведуть в один і той самий коректний фінал.
		//
		// 15.09.2026: файл більше НЕ живе стільки, скільки агентська нога. Агент
		// тримає ногу добами, і в один файл зливались сеанси різних днів (файл від
		// 14.09 22:34 дописувався о 01:18). Тепер файл відкривається на першому
		// пакеті, коли є глядач, а закриває його таймер у removeViewer через
		// recordIdleClose після ОСТАННЬОГО глядача (fanout.go). Відкриває лише ця
		// горутина, тож двох писарів на ноду не буває.
		var mine *recorder
		var nextTry time.Time

		go func() {
			defer func() {
				ns.rec.CompareAndSwap(mine, nil)
				mine.Close()
			}()
			for {
				if atomic.LoadUint64(&ns.generation) != myGen {
					return
				}
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				if atomic.LoadUint64(&ns.generation) != myGen {
					return
				}
				// Ingress ДО форварда (finding 10: раніше форвард/egress-лог
				// писався першим, egress випереджав ingress у NDJSON).
				n := atomic.AddUint64(&agentSeqSample, 1)
				if n%100 == 0 {
					logNDJSON("agent", ns.nodeID, pkt.SequenceNumber, pkt.Timestamp)
				}
				// Пишемо ВХІДНИЙ пакет, не переписаний egress: у файл має лягти
				// те, що прислав кодер. Send неблокуючий — диск не стоїть на
				// шляху глядача.
				rec := ns.rec.Load()
				if rec == nil && recordEnabled && ns.viewerCount.Load() > 0 && time.Now().After(nextTry) {
					// ponytail: повтор не частіше за 30 с — startRecording при тісному
					// диску відмовляє, а питати диск на кожному пакеті дорого.
					nextTry = time.Now().Add(30 * time.Second)
					if rec = startRecording(ns.nodeID); rec != nil {
						mine = rec
						ns.rec.Store(rec)
					}
				}
				rec.offer(pkt)
				forwardToViewers(ns, myGen, pkt)
			}
		}()
	})

	// «Публікатор є» = АГЕНТСЬКА НОГА ПІДНЯТА, а не «медіа вже прийшло».
	//
	// 🔴 Раніше ns.agentPC присвоювався ЛИШЕ в OnTrack вище, і це замикало коло
	// на проді: агент з on-demand гейтингом не кодує, поки нема глядача, а
	// authorizeViewer у ticket-режимі не пускає глядача, поки hasAgent() хибне.
	// Перший глядач не міг зайти НІКОЛИ — людина бачила банер «OO втрачено,
	// перемкнено на Mesh» (інцидент 30.08). Стенди мовчали, бо static-token
	// гілка authorizeViewer hasAgent() не перевіряє взагалі.
	//
	// Заміну агента лишаємо в OnTrack: SSRC відомий лише з треку, і саме той,
	// хто приніс трек, має право витіснити попередника. Тому тут — тільки коли
	// publisher-а немає зовсім (ns.agentPC == nil): нога, що піднялась поруч із
	// живою, чекає свого треку, як і раніше.
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			ns.mu.Lock()
			up := ns.agentPC == nil
			if up {
				ns.agentPC = pc
			}
			ns.mu.Unlock()
			if up {
				log.Printf("publisher up [node=%s]: agent leg connected -> node available (media may lag until first viewer)", ns.nodeID)
				recomputeBinding(ns)
			}
		// На Failed/Closed знімаємо publisher-а цієї ноди (fail-closed: viewer,
		// що прийде після смерті агента, отримає 404 і впаде на Mesh).
		// Disconnected — потенційно транзієнтний, не чіпаємо.
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			ns.mu.Lock()
			gone := ns.agentPC == pc
			if gone {
				ns.agentPC = nil
			}
			// H-09: канали цього PC мертві — обнуляємо, інакше кожна подія
			// вводу до реконекту агента била в «closed pipe» (534 рядки/72 год).
			// Лише якщо їх не встиг перевідкрити НОВИЙ PC того ж агента.
			if ns.agentChanPC == pc {
				ns.agentCtrl, ns.agentInput, ns.agentChanPC = nil, nil, nil
			}
			ns.mu.Unlock()
			if gone && tilesOn {
				agentTilesGone(ns)
			}
			if gone {
				// Логуємо ОБОВ'ЯЗКОВО: це єдиний слід втрати публікатора.
				// OnICEConnectionStateChange нижче пише лише стан ICE, а це
				// ІНША машина станів — вона може мовчати, поки PeerConnection
				// уже Failed/Closed. Через цей розрив живий інцидент 30.08 був
				// нечитний: у журналі "connected", через 26 хв "no publisher",
				// а між ними порожньо, і причину не було з чого відновити.
				log.Printf("publisher lost [node=%s]: peer connection %s -> node unavailable until agent reconnects", ns.nodeID, s)
				// Ногу знімаємо АКТИВНО, а не «тихо забуваємо». Спершу явне
				// повідомлення в control-канал (доїде лише якщо транспорт іще
				// живий — але тоді це найшвидший сигнал агентові), потім сам
				// Close. Close із колбека стану — тільки в окремій горутині:
				// колбеки pion виконуються на його горутинах, і синхронне
				// закриття звідси і реентрантне, і блокує обробку STUN.
				sendShutdown(ns, "publisher lost: "+s.String())
				go func() { _ = pc.Close() }()
				// Немає publisher -> знімаємо публікацію з УСІХ глядачів ноди.
				recomputeBinding(ns)
			}
		}
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Printf("agent leg ICE [node=%s]: %s", ns.nodeID, s)
	})
	return nil
}

// setupViewerLeg — ЩЕ ОДНА viewer-нога ноди: власний TrackLocalStaticRTP до
// браузера, власна черга у fanout (fanout.go), власний RTCP-цикл; пропускає
// вгору до агента ЦІЄЇ ноди лише PLI. Нова нога ДОДАЄТЬСЯ до наявних, а не
// заміщає їх — кілька глядачів дивляться одну ноду одночасно. claims —
// ненульовий лише в ticket-режимі (результат hub.ConsumeTicket);
// node-маршрутизація вже виконана в handleOffer (ns вибрано за node тікета), тож
// публікація треку вмикається ЛИШЕ через recomputeBinding() після Connected ТА
// наявного publisher-а цієї ноди.
// ticket — одноразовий квиток ЦІЄЇ ноги; ним підписане кожне повідомлення
// каналу вводу (input.go). Порожній (T1 static-token режим) = каналу вводу в
// цієї ноги немає взагалі.
func setupViewerLeg(ns *nodeSession, pc *webrtc.PeerConnection, claims *hub.TicketClaims, ticket, profile string) (string, error) {
	// node-binding вже застосовано у handleOffer (вибір ns за node тікета);
	// тут лише запам'ятовуємо user_id для runtime-revoke за user (kind="user").
	viewerUserID := ""
	if claims != nil {
		viewerUserID = claims.UserID
	}

	trk, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: h264FmtpFor(profile),
	}, "video", "oo-screen-hub")
	if err != nil {
		return "", err
	}

	sender, err := pc.AddTrack(trk)
	if err != nil {
		return "", err
	}

	// Друга доріжка — звук (audio.go), ЛИШЕ під OO_SCREEN_AUDIO. AddTrack
	// робиться ТУТ, до addViewer: помилка мусить впасти там само, де падає
	// помилка відеотреку — до реєстрації ноги, поки нема ні черги, ні pump-а,
	// які довелось би прибирати.
	var atrk *webrtc.TrackLocalStaticSample
	if audioEnabled {
		if atrk, err = addAudioTrack(pc); err != nil {
			return "", err
		}
	}

	// Нога стає в ряд до наявних; ще НЕ live — forwardToViewers форвардить лише
	// у ноги з vl.live, а це вмикає тільки recomputeBinding() після Connected ТА
	// наявного publisher-а цієї ноди (на failure/close знімаємо назад).
	vl := addViewerLimit(ns, pc, trk, viewerUserID, maxViewersPerNode)
	if vl == nil {
		return "", errViewerCap
	}
	// F-11: секрет ЦІЄЇ ноги. Він не дає нічого, крім права переукласти
	// ICE/SDP на PeerConnection, яка вже існує і вже авторизована: node, user і
	// grant у неї ті самі, а зникає нога з реєстру — зникає й дія цього
	// секрету (див. findViewerBySession: шукаємо лише серед ЖИВИХ ніг).
	sessionID, err := newSessionID()
	if err != nil {
		return "", err
	}
	ns.mu.Lock()
	vl.sessionID = sessionID
	ns.mu.Unlock()
	if atrk != nil {
		go vl.audioPump(ns, atrk)
	}

	// Канал вводу (input.go) — ЛИШЕ під прапорцем І ЛИШЕ для ноги з квитком.
	// Без прапорця OnDataChannel не ставиться взагалі: канал, який відкриє
	// браузер, лишиться без обробника, і жодна подія нікуди не поїде.
	// Канали глядача. pion тримає ОДИН OnDataChannel на PeerConnection,
	// тож обробники збираються тут і диспетчеризуються за міткою.
	var onInput func(*webrtc.DataChannel)
	if inputEnabled && ticket != "" {
		// grant із квитка — рівень дозволу, який визначив ЕРП. Порожній
		// claims (T1-режим) дає порожній grant, і канал вводу такій нозі не
		// відчиниться (judgeInput).
		grant := ""
		if claims != nil {
			grant = claims.Grant
		}
		onInput = viewerInputHandler(ns, vl, ticket, grant)
	}
	tilesOn := tilesEnabled
	// Канал курсора (cursor.go) приймається завжди: його відкриває лише плеєр
	// з config.cursorLayer, а дані в нього йдуть лише від агента з
	// -cursor-layer. Без каналу від браузера обробник просто не спрацьовує.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch {
		case dc.Label() == inputChannelLabel && onInput != nil:
			onInput(dc)
		case dc.Label() == tilesLabel && tilesOn:
			viewerTilesHandler(ns, vl, dc)
		case dc.Label() == cursorproto.ChannelLabel:
			viewerRelayHandler(ns, dc, cursorRelayConfig())
		}
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("viewer leg PC state [node=%s]: %s", ns.nodeID, s)
		switch s {
		case webrtc.PeerConnectionStateConnected:
			first := markViewerReady(ns, vl)
			recomputeBinding(ns)
			sendGate(ns) // зʼявився глядач → агент кодує (resume — вже на ПЕРШОМУ)
			if first {
				// Стелю повертаємо на стартову ЛИШЕ на переході 0->1: втрати
				// минулої сесії не наші. Глядач, що прийшов до вже наявних,
				// стелю не рухає — інакше він щоразу збивав би адаптацію,
				// яку веде найгірша нога (worstViewerRR). resetBitrate сам шле
				// і ціль, і keyframe.
				resetBitrate(ns)
			} else if !viewerPrimed(ns, vl) {
				// Новий глядач посеред потоку не має чекати природного IDR
				// (GOP 2с) — просимо keyframe (дебаунс усередині). Пункт 41:
				// нозі, якій уже поїхав кеш GOP, IDR не потрібен — саме заради
				// цього сплеску кеш і заводився.
				requestKeyframe(ns)
			}
		case webrtc.PeerConnectionStateDisconnected:
			// Потенційно транзієнтний: ногу з ноди НЕ знімаємо (повернеться
			// Connected — поновиться), лише перестаємо в неї форвардити.
			markViewerNotReady(ns, vl)
			sendGate(ns) // міг бути останнім → агент може простоювати
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			// Ця нога мертва — знімаємо саме її; решта глядачів ноди й
			// agent-нога не зачіпаються (sendGate усередині dropViewer).
			dropViewer(ns, vl, "viewer PC "+s.String())
		}
	})

	// RTCP read-loop viewer-ноги: обовʼязковий для NACK responder + PLI-propagation.
	// Тут же читаємо Receiver Report — джерело втрат/jitter для адаптації
	// бітрейту (bitrate.go): окремого каналу для цього не треба, RR і так тут.
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := sender.Read(buf)
			if err != nil {
				return
			}
			pkts, err := rtcp.Unmarshal(buf[:n])
			if err != nil {
				continue
			}
			for _, p := range pkts {
				switch pkt := p.(type) {
				case *rtcp.PictureLossIndication:
					propagatePLI(ns)
				case *rtcp.TransportLayerNack:
					// NACK responder pion відповідає на цей же пакет сам
					// (BindRTCPReader його лише ПРОПУСКАЄ далі, не зʼїдає) —
					// тут ми рахуємо, скільки з запитаного він фізично міг
					// віддати. Впала частка нижче порога -> ретрансмісія цій
					// нозі більше не рятує, переходимо на keyframe (nack.go).
					st := onNack(ns, vl, pkt, time.Now())
					logNackWindow(ns.nodeID, st)
					if st.escalate {
						propagatePLI(ns)
					}
				case *rtcp.ReceiverEstimatedMaximumBitrate:
					// Пункт 40. REMB — це ПРЯМА оцінка смуги hub->глядач від
					// самого приймача, і вона приходить раніше за втрати в RR:
					// браузер бачить, що черга наливається, ще до першого
					// втраченого пакета. Ціль = min(REMB, рішення по втратах),
					// див. bitrateCtl.withRemb.
					onRembEstimate(ns, uint64(pkt.Bitrate), time.Now())
				case *rtcp.ReceiverReport:
					// На цій нозі рівно один відеотрек, тож і reception report
					// один; цикл — на випадок, коли їх складено кілька в пакет.
					for _, rr := range pkt.Reports {
						now := time.Now()
						// RTT з LSR/DLSR ЦЬОГО Ж звіту (rtt.go): друге джерело
						// для контролера, під bufferbloat без втрат. ok=false —
						// семпла немає, попередній RTT ноги лишається.
						rtt, _ := rttFromReport(rr, now)
						// FANOUT: потік один на ВСІХ глядачів ноди, тож
						// контролер веде НАЙГІРША нога, а не та, чий RR
						// щойно прийшов (див. worstViewerRR).
						loss, jitter, rttExcess := worstViewerRR(ns, vl, float64(rr.FractionLost)/256, rr.Jitter, rtt, now)
						onReceiverReport(ns, loss, jitter, rttExcess, now)
					}
				}
			}
		}
	}()

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Printf("viewer leg ICE [node=%s]: %s", ns.nodeID, s)
	})
	return sessionID, nil
}

// newSessionID — 128 біт з crypto/rand. Це секрет, не лічильник: він мусить
// бути невгадним, інакше сусідня вкладка перебором переукладала б чужу ногу.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// findViewerBySession шукає ЖИВУ viewer-ногу за її session_id (F-11). Скан по
// реєстру, а не другий індекс, і саме тому: індекс довелось би чистити на
// кожному шляху смерті ноги (dropViewer, closeNode, revoke за user, revoke за
// node), і будь-який пропущений шлях лишав би вбитій нозі робочий ключ на
// переукладання. Тут же джерело правди одне — ns.viewers; зникла нога звідти,
// зник і її session_id. Ціна — прохід по реєстру, який трапляється лише на
// мережевому збої в конкретного глядача.
func findViewerBySession(id string) (*nodeSession, *viewerLeg) {
	if id == "" {
		return nil, nil
	}
	for _, ns := range reg.all() {
		ns.mu.Lock()
		for _, vl := range ns.viewers {
			if subtle.ConstantTimeCompare([]byte(vl.sessionID), []byte(id)) == 1 {
				ns.mu.Unlock()
				return ns, vl
			}
		}
		ns.mu.Unlock()
	}
	return nil, nil
}

// renegotiateViewer — F-11: новий offer (ICE-restart) на ВЖЕ НАЯВНУ viewer-ногу.
//
// Навіщо окрема гілка. Браузер на 'disconnected' може підняти зʼєднання
// restartIce()-ом, але сам по собі restartIce нічого не робить: він лише
// позначає, що наступний offer має нести нові ufrag/pwd — а доставити той offer
// нікуди. Звичайний /offer/viewer тут не годиться: квиток ОДНОРАЗОВИЙ (він уже
// спожитий на цій же нозі), тож глядач у грейс-періоді або отримав би 403, або
// мусив би брати НОВИЙ квиток і будувати НОВУ ногу — тобто рівно той розрив
// картинки, якого грейс і уникає.
//
// Чому це не друга авторизація. Ключ дійсний рівно поки нога є в ns.viewers:
// відкликання (за user, за node, stale-ERP), смерть агента чи будь-який
// dropViewer прибирають ногу — і ключ разом з нею. Ні node, ні user, ні grant
// не приходять із запиту: усе це вже стоїть на нозі й не переоцінюється.
func renegotiateViewer(w http.ResponseWriter, req offerReq) {
	ns, vl := findViewerBySession(req.SessionID)
	if ns == nil {
		// Нога вже мертва (або ключ чужий): чесна відмова, а глядач піде
		// звичайним шляхом — новий квиток або Mesh-фолбек.
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	// Два offer-и одночасно на одну PeerConnection — це гарантований
	// InvalidStateError усередині pion; глядач у грейсі цілком може натиснути
	// «підключитись» ще раз поверх власного авто-повтору.
	if !vl.renegotiating.CompareAndSwap(false, true) {
		http.Error(w, "renegotiation in progress", http.StatusConflict)
		return
	}
	defer vl.renegotiating.Store(false)

	pc := vl.pc
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.SDP}); err != nil {
		log.Printf("renegotiate [node=%s]: SetRemoteDescription: %v", ns.nodeID, err)
		http.Error(w, "bad offer sdp", http.StatusBadRequest)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		internalError(w, "renegotiate CreateAnswer", err)
		return
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		internalError(w, "renegotiate SetLocalDescription", err)
		return
	}
	select {
	case <-gatherComplete:
	case <-time.After(gatherTimeout):
		http.Error(w, "ICE gathering timeout", http.StatusGatewayTimeout)
		return
	}
	log.Printf("renegotiate [node=%s]: viewer leg ICE restarted", ns.nodeID)
	writeJSON(w, answerResp{SDP: pc.LocalDescription().SDP, SessionID: req.SessionID})
}

func drainRTCP(read func([]byte) (int, interceptor.Attributes, error), tag string) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := read(buf); err != nil {
			return
		}
	}
}

// forwardToViewers переписує seq/timestamp-простір (свій базовий відлік цієї
// ноди, ОДИН на всіх глядачів) і розкладає пакет по чергах УСІХ живих
// viewer-ніг. Сам запис у трек робить pump кожної ноги (fanout.go): тут лише
// неблокуючий send, тож повільний глядач не тримає цей цикл — його чергу
// переповнить, і рвемо саме його ногу.
func forwardToViewers(ns *nodeSession, gen uint64, pkt *rtp.Packet) {
	ns.mu.Lock()
	if !hasLiveViewerLocked(ns) {
		ns.mu.Unlock()
		return
	}
	// Egress seq/ts = попередній egress + delta(вхід). Ніколи не перескидається
	// на заміні агента (finding 9/10): це прибирає стрибок назад/interleaving у
	// нумерації, яку бачить viewer.
	//
	// H-07/H-27. Самої відсутності скиду мало. Новий агент починає власний
	// потік із ВИПАДКОВИХ seq/ts, а lastInSeq/lastInTS лишались від старого:
	// перша ж delta == pkt.SequenceNumber - <чужий відлік> давала стрибок
	// уперед у середньому на пів-діапазону uint16, і той самий безлад у
	// rtp_ts. Глядач читав це як діру в ~32 тисячі пакетів: NACK-шторм на весь
	// уявний проміжок і скид джитер-буфера. Тому перший пакет НОВОГО покоління
	// не рахує delta взагалі — він переанкорює вхідний відлік на себе, а
	// egress рухає рівно на один крок уперед. Вперед, не назад: монотонність
	// нумерації, яку бачить viewer, лишається інваріантом ноди.
	genSwitched := false
	switch {
	case !ns.haveEgress:
		// Перший пакет ноди взагалі — базовий відлік egress нульовий.
		ns.haveEgress = true
		ns.egressGen = gen
		ns.lastInSeq = pkt.SequenceNumber
		ns.lastInTS = pkt.Timestamp
		ns.lastOutSeq = 0
		ns.lastOutTS = 0
	case ns.egressGen != gen:
		// Заміна агента. +1 до seq саме тому, що це НЕ діра: пропуск номера
		// глядач зажадав би через NACK, а нам нічого йому вислати.
		genSwitched = true
		ns.egressGen = gen
		ns.lastInSeq = pkt.SequenceNumber
		ns.lastInTS = pkt.Timestamp
		ns.lastOutSeq++
		ns.lastOutTS += seamlessTSStep
	default:
		ns.lastOutSeq += pkt.SequenceNumber - ns.lastInSeq
		ns.lastOutTS += pkt.Timestamp - ns.lastInTS
		ns.lastInSeq = pkt.SequenceNumber
		ns.lastInTS = pkt.Timestamp
	}
	outSeq := ns.lastOutSeq
	outTS := ns.lastOutTS
	if genSwitched {
		// Кеш GOP лишився від ПОПЕРЕДНЬОГО кодера: його SPS/PPS новому потоку
		// не підходять, а віддати його новій нозі означало б показати кадри,
		// яких у поточному потоці вже немає. Чекаємо на IDR нового агента.
		ns.gop.reset()
	}

	// Один пакет на всіх: TrackLocalStaticRTP.WriteRTP його не мутує (копіює
	// заголовок і сам переписує SSRC/PT під свою прив'язку), тож ділити один
	// вказівник між ногами безпечно.
	out := &rtp.Packet{Header: pkt.Header, Payload: pkt.Payload}
	out.SequenceNumber = outSeq
	out.Timestamp = outTS

	// Пункт 41: той самий вказівник осідає в кеші GOP — нова нога отримає його
	// звідти, а не чекатиме наступного IDR.
	// Бюджет у байтах — від СТЕЛІ ноди, а не поточної цілі: кодер сходить до
	// нової цілі лише за GOP, і хвіст, набраний на старому бітрейті, не має
	// рватися через те, що ціль щойно впала.
	ns.gop.setBitrate(ns.ceilingBps())
	ns.gop.note(out)

	// send неблокуючий: черга повна => ця нога відстає від джерела.
	//
	// H-12: відставання лікується скиданням кадрів, а не вбивством сесії. Нога,
	// що переповнилась, переходить у drop-to-IDR: їй НІЧОГО не кладуть до
	// наступного ключового пакета, і саме ця пауза дає її pump-у розібрати
	// чергу. Рвемо лише того, кому це не допомогло viewerOverflowStreakMax разів
	// поспіль — і, як і раніше, ПОЗА локом (dropViewer смикає колбеки pion).
	isKey := h264KeyPart(out.Payload)
	var slow []*viewerLeg
	overflowed := false
	for _, vl := range ns.viewers {
		if !vl.live {
			continue
		}
		if vl.discarding {
			if !isKey {
				continue // кадр у смітник: нога зараз наздоганяє
			}
			// Ключовий набір — точка, з якої декодер уміє почати заново.
			vl.discarding = false
		}
		// Нога, якій щойно віддали кеш GOP, має в черзі ще й той хвіст: йому
		// дозволено лежати ПОНАД viewerQueueDepth (ємність каналу це вміщає),
		// а поріг відставання повертається до звичного, щойно pump його розібрав.
		limit := viewerQueueDepth + vl.primeSlack
		if vl.primeSlack > 0 && len(vl.out) < viewerQueueDepth {
			vl.primeSlack = 0
			limit = viewerQueueDepth
		}
		var sent bool
		if len(vl.out) < limit {
			select {
			case vl.out <- out:
				sent = true
			default:
			}
		}
		if !sent {
			now := time.Now()
			if vl.overflowAt.IsZero() || now.Sub(vl.overflowAt) > viewerOverflowWindow {
				vl.overflowStreak = 1 // попереднє переповнення було давно — це новий епізод
			} else {
				vl.overflowStreak++
			}
			vl.overflowAt = now
			if vl.overflowStreak >= viewerOverflowStreakMax {
				slow = append(slow, vl)
				continue
			}
			vl.discarding = true
			overflowed = true
		}
	}
	ns.mu.Unlock()

	if genSwitched {
		log.Printf("publisher замінено [node=%s]: egress продовжено з seq=%d ts=%d", ns.nodeID, outSeq, outTS)
	}
	if overflowed {
		log.Printf("viewer leg у drop-to-IDR [node=%s]: черга переповнена, кадри скидаються до наступного ключового", ns.nodeID)
	}
	if genSwitched || overflowed {
		// ПОЗА ns.mu: requestKeyframe бере той самий лок. Обидві причини — про
		// одне й те саме: у глядача немає точки, з якої його декодер уміє
		// почати. Без IDR це рівно той сірий екран, від якого H-07 і H-12
		// рятують, тільки з різних боків.
		requestKeyframe(ns)
	}

	for _, vl := range slow {
		dropViewer(ns, vl, fmt.Sprintf("черга переповнена %d рази поспіль — скидання кадрів не допомогло", viewerOverflowStreakMax))
	}

	n := atomic.AddUint64(&viewerSeqSample, 1)
	if n%100 == 0 {
		// rtp_ts тут — ІММУТАБЕЛЬНИЙ вхідний timestamp (pkt.Timestamp), той
		// самий, що пише agent-лог для цього пакета: спільний ключ для
		// зіставлення ingress/egress семплів (finding 10). Один семпл на ноду,
		// а не на кожного глядача — egress-простір у них спільний.
		logNDJSON("viewer", ns.nodeID, outSeq, pkt.Timestamp)
	}
}

// recomputeBinding — єдине місце, де вмикається/вимикається vl.live (тобто
// реальна публікація треку у viewer-ноги цієї ноди). Викликається після кожної
// зміни, що впливає на прив'язку: глядач стає Connected/відпадає, агент цієї
// ноди публікує/вмирає. Node-маршрутизація вже гарантована вибором ns за node
// тікета в handleOffer, тож тут лишається перевірити готовність КОЖНОЇ ноги ТА
// наявність publisher-а (fail-closed: немає агента -> не форвардимо нікому).
func recomputeBinding(ns *nodeSession) {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	hasAgent := ns.agentPC != nil
	for _, vl := range ns.viewers {
		was := vl.live
		// !vl.hidden — F-39: глядач сам сказав, що його вкладка прихована.
		// Робимо це саме тут, а не другим гейтом у forwardToViewers, бо тоді
		// правд про «кому йде потік» стало б дві, і повернення з прихованого
		// пройшло б повз праймінг GOP нижче — тобто рівно в сірий екран.
		vl.live = hasAgent && vl.ready && !vl.hidden
		// Пункт 41: перехід «не публікуємо» -> «публікуємо» — ЄДИНИЙ момент,
		// коли нозі можна віддати кеш. Робиться саме тут і саме під цим локом:
		// forwardToViewers пише в черги під ним же, тож між кешем і першим
		// живим пакетом нічого не встромиться і порядок seq не порушиться.
		if !was && vl.live {
			vl.primed = primeViewerLocked(ns, vl)
		}
	}
}

// primeViewerLocked віддає новій нозі кеш GOP ноди. true = картинка в неї
// поїде одразу, тобто позачерговий IDR у агента просити не треба.
// Кликати ЛИШЕ під ns.mu.
//
// Черга свіжої ноги порожня, а її ємність — viewerQueueDepth + gopMaxPackets,
// тож кеш у нормі влазить цілком; якщо ні — це вже не наш випадок «глядач
// щойно зайшов», і ногу лікує звичайний requestKeyframe. Скільки з цього —
// хвіст кешу, пам'ятає vl.primeSlack (поріг відставання у forwardToViewers).
func primeViewerLocked(ns *nodeSession, vl *viewerLeg) bool {
	pkts := ns.gop.replay()
	if len(pkts) == 0 {
		return false
	}
	// Все-або-нічого: частково вкладений кеш при primeSlack=0 дав би
	// неперервний лише до розриву потік — живі пакети різались би як
	// відставання. Під ns.mu черга лише спорожнюється (читач — writer-горутина),
	// тож вільне місце, виміряне тут, не зменшиться до кінця циклу.
	if cap(vl.out)-len(vl.out) < len(pkts) {
		log.Printf("gop prime [node=%s]: черга глядача не вмістить кеш (%d пакетів) — лишаємо keyframe_request", ns.nodeID, len(pkts))
		return false
	}
	for _, p := range pkts {
		vl.out <- p
	}
	vl.primeSlack = len(pkts)
	log.Printf("gop prime [node=%s]: віддано %d кешованих пакетів від останнього IDR", ns.nodeID, len(pkts))
	return true
}

// sendGate — повідомляє агенту цієї ноди присутність глядачів через control-
// DataChannel (on-demand гейтинг). FANOUT: "resume" поки є ХОЧА Б ОДИН глядач,
// що ДИВИТЬСЯ, "pause" лише коли їх НУЛЬ — тобто resume на першому, pause на
// останньому, що пішов АБО сховався (F-39). Викликати щоразу, коли МОЖЕ
// змінитись присутність: глядач підключився/відпав/сховався/повернувся, або
// щойно відкрився control-канал агента. Агент застосовує CompareAndSwap, тож
// повтор того самого стану нешкідливий. Тихий no-op, поки канал не відкритий
// (старий агент без DataChannel лишається always-on — безпечний фолбек).
func sendGate(ns *nodeSession) {
	ns.mu.Lock()
	dc := ns.agentCtrl
	present := hasVisibleViewerLocked(ns)
	ns.mu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	msg := "pause"
	if present {
		msg = "resume"
	}
	if err := dc.SendText(msg); err != nil {
		log.Printf("sendGate [node=%s]: %v", ns.nodeID, err)
	}
}

// propagatePLI шле PLI агенту ЦІЄЇ ноди у відповідь на PLI від viewer-ноги (або
// коли NACK цій нозі вже не допомагає, див. nack.go). Дебаунс pliGate — на ВСЮ
// ноду: потік один на всіх глядачів, отже й keyframe один на всіх. Без нього N
// глядачів, що втратили один і той самий пакет, коштували б агентові N запитів
// IDR — це і є підсилювач каскаду «втрата -> PLI -> keyframe -> сплеск
// бітрейту -> втрата в сусіда».
func propagatePLI(ns *nodeSession) {
	if !pliGate(ns, time.Now()) {
		return
	}
	sendPLIToAgent(ns)
}

// wantedProfileLevelID — profile-level-id, який хаб оголошує в MediaEngine.
// Береться з h264FmtpLine, а не дублюється константою: два джерела правди тут
// розійшлись би тихо, і перевірка H-18 почала б відхиляти власний же answer.
var wantedProfileLevelID = fmtpParam(h264FmtpLine, "profile-level-id")

// h264ProfileCompatible — чи зможе глядач із profile-level-id `got` декодувати
// потік, який хаб оголошує як `want`.
//
// Порівнюються ЛИШЕ перші 4 шістнадцяткові цифри (profile_idc + profile_iop).
// Останні дві — level_idc, і порівнювати їх НЕ МОЖНА: ми самі оголошуємо
// level-asymmetry-allowed=1, тож різні рівні у двох боків — штатна ситуація
// (глядач pion пропонує 64001f, хаб шле 64002a — той самий профіль).
//
// Порівнювати ці 4 цифри як РЯДОК не можна, і це коштувало 2.5 години 05.09.2026.
// За RFC 6184 §8.1 profile-level-id — це три байти: profile_idc, profile_iop,
// level_idc. profile_iop — не частина назви профілю, а НАБІР ПРАПОРЦІВ
// constraint_set0..5, тобто ОБМЕЖЕНЬ, які потік обіцяє не порушувати. Chrome
// штатно оголошує High як 640c1f: той самий profile_idc 0x64, але iop 0x0c
// (constraint_set4+5 — Constrained High, тобто «прогресивна розгортка, без
// полів»). Декодер той самий. Рядкове порівняння «6400» != «640c» гнало таких
// глядачів у 415.
//
// Правило: profile_idc мусить збігтися ТОЧНО (High != Main != Baseline !=
// High 4:4:4 — це різні декодери), а прапорці обмежень глядача мають бути
// НАДМНОЖИНОЮ наших. Наш потік — screen capture, прогресивний, тож жодного
// з констрейнтів, які додає Chrome, він і так не порушує.
//
// ponytail: свідомо НЕ реалізую повну таблицю еквівалентності профілів із
// libwebrtc (baseline↔constrained-baseline через constraint_set1 тощо). Хаб
// оголошує рівно один профіль — High; усе, що треба, — не відкидати його ж
// самого в чужому записі. Знадобиться більше — тоді й таблиця.
func h264ProfileCompatible(got, want string) bool {
	gIDC, gIOP, ok1 := splitProfile(got)
	wIDC, wIOP, ok2 := splitProfile(want)
	if !ok1 || !ok2 || gIDC != wIDC {
		return false
	}
	// Напрямок тут не симетричний, і помилитись легко (я помилився).
	// constraint_set-прапорці — це ОБІЦЯНКИ ПОТОКУ чогось не робити, а не
	// вимоги до приймача. Потік, який пообіцяв БІЛЬШЕ, декодується будь-яким
	// приймачем того ж profile_idc: він строго простіший. Тому вимагаємо, щоб
	// прапорці ГЛЯДАЧА були підмножиною наших, а не навпаки.
	//
	// Ціна помилки виміряна: живий NVENC/MFT віддає SPS 4D402A
	// (constraint_set1), Chrome оголошує 4d001f без прапорців. Зворотне
	// порівняння відхиляло рівно того глядача, заради якого все це робилось.
	return gIOP&^wIOP == 0
}

// splitProfile ріже profile-level-id на profile_idc і profile_iop.
func splitProfile(pli string) (idc, iop byte, ok bool) {
	if len(pli) < 4 {
		return 0, 0, false
	}
	b, err := hex.DecodeString(strings.ToLower(pli[:4]))
	if err != nil {
		return 0, 0, false
	}
	return b[0], b[1], true
}

// fmtpParam дістає значення одного параметра з fmtp-рядка
// ("a=b;c=d" -> fmtpParam(s,"c") == "d"). Порівняння імені —
// регістронезалежне, як того і вимагає SDP.
func fmtpParam(fmtp, name string) string {
	for _, part := range strings.Split(fmtp, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

// videoCodecMismatch перевіряє, що у ВІДПОВІДІ лишилась відеодоріжка з H.264 і
// саме нашим profile-level-id. Порожній рядок — усе гаразд; інакше повертається
// ЛЮДСЬКА причина, яку не соромно віддати клієнту в тілі 415.
//
// Розбираємо текст SDP, а не стан pion, свідомо: у відповідь піде рівно те, що
// тут написано, і перевіряти треба саме його. Дивимось ЛИШЕ на секцію m=video —
// H.264 у секції звуку не буває, а от Opus із власним fmtp там є завжди.
// codecMismatch — причина відмови в машинно-читаному вигляді. Саме ЦЕ їде в
// тілі 415: фронт 05.09.2026 бачив голе «415» без причини і мовчки падав у
// MeshCentral (де звуку немає взагалі), а причину шукали 2.5 години в журналі
// сервера. Причина мусить бути там, де її побачить той, хто впав.
type codecMismatch struct {
	Error          string   `json:"error"` // код: h264_profile_mismatch тощо
	Detail         string   `json:"detail"`
	ViewerProfiles []string `json:"viewer_profiles,omitempty"`
	HubProfile     string   `json:"hub_profile,omitempty"`
}

// profileUnknownYet — код відмови, коли агент ноди є, але профіль його потоку
// хабу невідомий. Окремий код, бо це НЕ «глядач не той»: це «хаб не знає, що
// віддасть», і чесна відмова тут краща за домовленість навмання.
const profileUnknownYet = "profile_unknown_yet"

// h264VideoProfiles розбирає секцію m=video і повертає profile-level-id кожної
// H.264-payload (порожній рядок — fmtp без profile-level-id), а також чи була
// відеодоріжка взагалі і чи її відхилено ("m=video 0", RFC 4566).
//
// Один парсер на два виклики: перевірку відповіді глядачеві (videoCodecMismatch)
// і читання профілю з offer-а агента (sdpVideoProfile). Два окремих парсери
// SDP розійшлись би тихо.
func h264VideoProfiles(sdp string) (profiles []string, haveVideo, rejected bool) {
	var (
		inVideo bool
		rtpmap  = map[string]string{} // payload type -> "H264/90000"
		fmtps   = map[string]string{} // payload type -> fmtp-рядок
	)
	for _, raw := range strings.Split(sdp, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "m="):
			inVideo = strings.HasPrefix(line, "m=video")
			if inVideo {
				haveVideo = true
				if f := strings.Fields(line); len(f) >= 2 && f[1] == "0" {
					rejected = true
				}
			}
		case !inVideo:
			// Атрибути секції звуку чи сесії нас не стосуються.
		case strings.HasPrefix(line, "a=rtpmap:"):
			if pt, rest, ok := strings.Cut(strings.TrimPrefix(line, "a=rtpmap:"), " "); ok {
				rtpmap[pt] = rest
			}
		case strings.HasPrefix(line, "a=fmtp:"):
			if pt, rest, ok := strings.Cut(strings.TrimPrefix(line, "a=fmtp:"), " "); ok {
				fmtps[pt] = rest
			}
		}
	}
	for pt, codec := range rtpmap {
		if strings.HasPrefix(strings.ToUpper(codec), "H264/") {
			profiles = append(profiles, fmtpParam(fmtps[pt], "profile-level-id"))
		}
	}
	sort.Strings(profiles) // мапа невпорядкована — інакше вибір і текст стрибають
	return profiles, haveVideo, rejected
}

// sdpVideoProfile — profile-level-id, який offer агента оголошує для свого
// відеотреку. "" = невідомо: профілю немає або їх кілька РІЗНИХ, і вгадувати
// хаб не буде (див. profileUnknownYet). Живий агент реєструє в MediaEngine
// рівно один кодек — свій, з SPS власного енкодера, тож у нього PT один.
func sdpVideoProfile(sdp string) string {
	profiles, _, _ := h264VideoProfiles(sdp)
	got := ""
	for _, p := range profiles {
		if p == "" {
			continue
		}
		if got != "" && !sameProfile(got, p) {
			return ""
		}
		got = p
	}
	return got
}

// videoCodecMismatch перевіряє, що у ВІДПОВІДІ лишилась відеодоріжка з H.264 і
// саме тим profile-level-id, який хаб оголошує ЦІЙ нозі (want). Nil — усе
// гаразд; інакше повертається ЛЮДСЬКА причина, яку не соромно віддати клієнту
// в тілі 415.
func videoCodecMismatch(sdp, want string) *codecMismatch {
	profiles, haveVideo, rejected := h264VideoProfiles(sdp)
	switch {
	case !haveVideo:
		return &codecMismatch{Error: "no_video_track",
			Detail: "у відповіді немає відеодоріжки — глядач не запропонував жодного відео"}
	case rejected:
		return &codecMismatch{Error: "video_track_rejected",
			Detail: "відеодоріжку відхилено (m=video 0): спільного з хабом кодека немає"}
	}

	seen := make([]string, 0, 2)
	for _, got := range profiles {
		// Рівень (останні дві цифри) свідомо поза порівнянням — див. h264ProfileCompatible.
		if h264ProfileCompatible(got, want) {
			return nil
		}
		if got == "" {
			got = "без profile-level-id"
		}
		seen = append(seen, got)
	}
	if len(seen) == 0 {
		return &codecMismatch{Error: "no_h264",
			Detail:     "в узгодженому відео немає H.264 (хаб форвардить лише його, перекодування немає)",
			HubProfile: want}
	}
	return &codecMismatch{
		Error: "h264_profile_mismatch",
		Detail: fmt.Sprintf("H.264 є, але профіль у profile-level-id %s не той — хаб шле %s",
			strings.Join(seen, "/"), want),
		ViewerProfiles: seen,
		HubProfile:     want,
	}
}

// sendPLIToAgent шле один RTCP PLI на трек агента цієї ноди БЕЗ дебаунсу —
// його накладає той, хто кличе (pliGate у propagatePLI, keyframeDebnc у
// requestKeyframe). false = слати не було куди: агентської ноги немає або її
// трек ще не приніс SSRC.
//
// Перевірка agentSSRC != 0 — не косметика: PLI з MediaSSRC == 0 не адресує
// нічого, агент його мовчки відкине, і хаб вважав би, що IDR замовлено.
func sendPLIToAgent(ns *nodeSession) bool {
	ns.mu.Lock()
	pc := ns.agentPC
	ssrc := ns.agentSSRC
	ns.mu.Unlock()
	if pc == nil || ssrc == 0 {
		return false
	}
	if err := pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}}); err != nil {
		log.Printf("PLI агенту [node=%s]: %v", ns.nodeID, err)
		return false
	}
	return true
}

// H-28. NDJSON пишуть КІЛЬКА горутин одночасно: читач RTP агента (ingress),
// forwardToViewers (egress), RTCP-цикл КОЖНОЇ viewer-ноги (nack) і контролер
// бітрейту (ctl). fmt.Printf між собою їх не серіалізує, а os.Stdout у трубу
// пише рядки, довші за PIPE_BUF, частинами — тобто рядки могли злипатись і
// рвати рівно той файл, за яким потім рахують втрати. Один мьютекс на весь
// NDJSON-вихід; усі писарі ходять через ndjsonf, а не через fmt.Printf.
//
// ponytail: стеля — лок глобальний на процес. Пишемо рідко (семпл на 100
// пакетів), тож конкуренції тут немає; зʼявиться — апгрейд на місці: канал з
// одним писарем замість мьютекса.
var ndjsonMu sync.Mutex

// ndjsonOut — куди йде NDJSON. Змінна, а не os.Stdout напряму, рівно заради
// гейта: тест підставляє сюди небезпечний для конкурентного запису Writer, і
// без мьютекса вище -race валить прогін. Гейт, який не вміє почервоніти, нічого
// не стереже.
var ndjsonOut io.Writer = os.Stdout

func ndjsonf(format string, a ...any) {
	ndjsonMu.Lock()
	defer ndjsonMu.Unlock()
	fmt.Fprintf(ndjsonOut, format, a...)
}

func logNDJSON(leg, node string, seq uint16, rtpTS uint32) {
	ndjsonf(`{"leg":%q,"node":%q,"seq":%d,"rtp_ts":%d,"t_ms":%d}`+"\n", leg, node, seq, rtpTS, time.Now().UnixMilli())
}

// startRevokeSubscription — runtime-відкликання (§6.4): поки hub живе, питає
// ERP кожні 2с "що відкликано" і рве відповідні живі WebRTC-ноги. Лише в
// ticket-режимі (ERP_BASE заданий) — у T1/static-token режимі ERP-контуру
// взагалі немає, питати нема кого.
func startRevokeSubscription(ctx context.Context) {
	if !ticketModeEnabled() {
		return
	}
	// 3 с замість 2: за 72 год пол не вкладався 891 раз у власний таймаут
	// (H-32). Таймаут клієнта = інтервал − 0.5 с (revoke.go), щоб тіки не
	// накладались.
	go hub.SubscribeRevoke(ctx, erpBase, hubKey, 3*time.Second, func(kind, val string) {
		switch kind {
		case "node":
			if ns := reg.get(val); ns != nil {
				closeNode(ns)
			}
		case "user":
			for _, ns := range reg.nodesForUser(val) {
				dropUserViewers(ns, val)
			}
		case hub.RevokeKindStale:
			// ERP мовчить довше за поріг — жоден дозвіл більше не підтверджений,
			// тож рвемо ВСЕ тим самим closeNode. Причина й тривалість уже в журналі
			// (revoke.go, staleGate.observe); тут — лише скільки нод це зачепило.
			all := reg.all()
			dropped := 0
			for _, ns := range all {
				dropped += dropAllViewers(ns, "runtime-revoke stale: ERP unreachable "+val)
			}
			log.Printf("runtime-revoke: fail-closed через недоступність ERP (%s) — знято %d глядацьких ніг на %d нодах; агенти лишаються", val, dropped, len(all))
		default:
			log.Printf("runtime-revoke: невідомий kind %q, ігноровано", kind)
		}
	})
}

func main() {
	// O-01/H-01: дефолтний токен публічний і передбачуваний; хаб із ним на
	// публічному порту = будь-хто реєструє агента під чужою нодою.
	if token == defaultToken {
		log.Fatal("hub-webrtc: OO_SCREEN_T1_TOKEN не задано (дефолт заборонено) — задай у EnvironmentFile сервісу")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startRevokeSubscription(ctx)

	// pprof — ОКРЕМИЙ слухач і лише за явним OO_SCREEN_PPROF_ADDR. Не на mux
	// сигналінгу: /debug/pprof віддає дампи горутин і профілі, і на проді він
	// не має бути досяжний з того ж порту, що й /offer/*. Порожній env = вимкнено.
	if addr := os.Getenv("OO_SCREEN_PPROF_ADDR"); addr != "" {
		go func() {
			log.Printf("pprof on %s (діагностика; НЕ вмикати на публічному інтерфейсі)", addr)
			log.Printf("pprof exited: %v", http.ListenAndServe(addr, nil))
		}()
	}

	mux := http.NewServeMux()
	// SEC #21: per-IP rate-limit (ratelimit.go) — кожен viewer-offer це виклик ERP.
	mux.HandleFunc("/offer/agent", rateLimited(offerLimiter, handleOffer("agent")))
	mux.HandleFunc("/offer/viewer", rateLimited(offerLimiter, handleOffer("viewer")))
	mux.HandleFunc("/control", handleControl)
	// F-39: глядач каже «моя вкладка прихована/знову видима» — хаб на цей час
	// не шле йому відео, а коли приховані ВСІ, ставить агента на паузу.
	// Ключ — session_id його ж ноги, як у ренегоціації (visibility.go).
	mux.HandleFunc("/viewer/visibility", handleViewerVisibility)
	// Готовність вузлів для консолі ЕРП (гасити кнопку ДО кліку). Лише читання,
	// гейт — X-OO-Hub-Key; без ключа маршрут мовчить (див. nodes.go).
	mux.HandleFunc("/nodes", handleNodes)
	// H-16: здоровʼя для моніторингу: скільки нод, чи свіжий пол ревокацій.
	mux.HandleFunc("/healthz", handleHealthz)

	if ticketModeEnabled() {
		// H-01: токен у журнал НЕ пишемо — journald читає будь-хто з групи adm.
		log.Printf("hub-webrtc listening on %s (ticket-mode: erp=%s, agent token=…%s, multi-publisher; env fallback node=%q)", listenAddr, erpBase, tokenTail(), agentNodeIDEnv)
	} else {
		log.Printf("WARNING: static-token mode, not for prod (OO_SCREEN_ERP_BASE not set — viewer offers accept a static OO_SCREEN_T1_TOKEN, default is public/predictable)")
		log.Printf("hub-webrtc listening on %s (T1 static-token mode, token=…%s, multi-publisher; env fallback node=%q)", listenAddr, tokenTail(), agentNodeIDEnv)
	}
	// H-03: голий ListenAndServe = без жодного таймауту на публічному порту
	// (slowloris, забуті зʼєднання). Write 30 с покриває gatherTimeout.
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// H-15: на SIGTERM — сказати кожному агентові «хаб іде» (control-канал)
	// і закрити слухач, а не рвати сокети мовчки: інакше весь парк 25 с
	// чекає на consent-таймаут, перш ніж перепідключитись.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigCh
		log.Printf("hub-webrtc: %s — graceful shutdown", s)
		for _, ns := range reg.all() {
			sendShutdown(ns, "hub shutting down")
		}
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = srv.Shutdown(shutdownCtx)
		cancel()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // дати control-повідомленням вийти
}

// handleHealthz — H-16. 200 = живий і пол ревокацій свіжий (або ticket-mode
// вимкнено); 503 = пол ревокацій старіший за stale-поріг.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	nodes := reg.all()
	agents := 0
	viewers := 0
	for _, ns := range nodes {
		if ns.hasAgent() {
			agents++
		}
		ns.mu.Lock()
		viewers += len(ns.viewers)
		ns.mu.Unlock()
	}
	age, ok := hub.RevokePollAge(time.Now())
	body := map[string]any{
		"ok":                 true,
		"ticket_mode":        ticketModeEnabled(),
		"nodes":              len(nodes),
		"agents":             agents,
		"viewers":            viewers,
		"revoke_poll_ok":     ok,
		"revoke_poll_age_ms": age.Milliseconds(),
		// H-18: відмови за відеокодеком. 0 і 0 — жодної.
		"rejected_profile_total": rejectedProfileTotal.Load(),
		"rejected_profile_at":    rejectedProfileAt.Load(),
	}
	status := http.StatusOK
	if ticketModeEnabled() && (!ok || age > 60*time.Second) {
		body["ok"] = false
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
