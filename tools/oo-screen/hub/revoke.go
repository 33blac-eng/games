// revoke.go — runtime-відкликання доступу (§6.4 плану, розділ "runtime revoke").
//
// Проблема: consumeTicket() перевіряє грант ЛИШЕ в момент старту сесії (offer
// viewer-ноги). Якщо адмін відкликає грант/роль/тенанта ПІСЛЯ старту, hub про
// це нічого не знає — WebRTC-сесія живе до природного кінця (viewer сам
// закриє вкладку, або мережа впаде).
//
// Рішення: короткий HTTP-поллінг, БЕЗ нової go-залежності й БЕЗ redis (обидва
// прямо заборонені завданням). hub кожні pollInterval питає ERP "що
// відкликано з часу since" (курсор у unix ms, монотонний — сторінка
// відкликань append-only, тому max(at) серед обробленого завжди безпечний
// наступний since). ERP лишається джерелом правди: hub нічого не вирішує сам,
// лише виконує отримані kind/val як накази "закрити".
//
// Чому поллінг, а не push/websocket: hub і ERP — різні процеси, часто різні
// хости (screen.organicoils.com.ua і total.organicoils.com.ua), і держати між
// ними довгоживучий канал — це ще один вид з'єднання, який може впасти
// мовчки. Поллінг із fail-soft (HTTP-помилка не вбиває цикл, лише лишає
// since на місці — наступний тік просто перезапитає той самий діапазон)
// найпростіший тут: worst-case затримка відкликання — pollInterval.
//
// М'ЯКО СПОЧАТКУ, ТВЕРДО ЗГОДОМ (staleGate нижче). Сам по собі fail-soft —
// лише половина правди: курсор справді не губить відкликань (вони приїдуть,
// коли ERP оживе), але відкладання не мало межі. 30.08.2026 близько 01:45 hub
// кілька хвилин поспіль писав "erp unreachable ... context deadline exceeded",
// і весь той час рубильник був недієвий: будь-яка сесія лишалась живою, бо hub
// покладався на дозвіл, підтверджений востаннє невідомо коли. Тому: коротка
// недоступність терпима (ERP перезапускається на КОЖНОМУ деплої — рвати живі
// сесії колегам через це неприпустимо), а після staleAfter БЕЗ жодного
// успішного опитування hub перестає вірити застарілому дозволу й рве все.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// revocationEntry — один запис зі сторінки ERP-ендпоінта
// {erpBase}/remote-access/screen/revocations. Заповнене рівно одне з полів
// NodeID/UserID (валідується у subscribeRevoke, не тут).
type revocationEntry struct {
	NodeID string `json:"node_id"`
	UserID string `json:"user_id"`
	At     int64  `json:"at"`
}

// RevokeKindStale — псевдо-"відкликання", яке hub видає САМ, коли ERP надто
// довго недосяжний. Val — скільки саме тривала недоступність. Обробник (див.
// startRevokeSubscription у hub/cmd/hub-webrtc/main.go) закриває ВСІ ноди тим
// самим closeNode, яким закривається звичайне kind="node": окремого шляху
// обриву не заводимо, щоб fail-closed не розійшовся з нормальним відкликанням.
const RevokeKindStale = "stale"

// envStaleAfter — env, що перекриває поріг. Значення — будь-що, що розуміє
// time.ParseDuration ("45s", "2m").
const envStaleAfter = "OO_SCREEN_REVOKE_STALE_AFTER"

// defaultStaleAfter — скільки hub терпить ПОВНУ недоступність ERP, перш ніж
// перестати вірити застарілому дозволу.
//
// НИЖНЯ МЕЖА — звичайне викочування ERP, заміряне на проді 26.08.2026 (числа
// з deploy/rolling_reload.py і deploy/ship.py, не з голови): `octane:reload`
// піднімає всі 6 воркерів залпом, кожен платить ~4,3 с на першому запиті крізь
// себе, найгірший запит — 7211 мс. Штатний шлях ship.py переїжджає воркерами
// ПО ОДНОМУ: ~2,6 с на народження + 6 с усідання на воркера, тобто все вікно
// перезапуску ~50 с (у поганому випадку BORN_TIMEOUT_S=25 с на воркера). На
// повний `systemctl restart oo-erp-octane oo-erp-queue oo-erp-reverb` (потрібен
// при зміні .env) заміряного числа в репо НЕМА — прямо кажу це і беру запас.
// Отже нижня межа — десятки секунд, і поріг мусить бути помітно більшим, ніж
// найдовше відоме вікно (~50 с), інакше кожен деплой рватиме сесії колегам.
//
// ВЕРХНЯ МЕЖА — безпека: весь час понад поріг відкликаний глядач продовжує
// дивитись чужий екран. Хвилини — це вже погано, десятки хвилин — неприйнятно.
//
// 90 с = ~1,8× найдовшого відомого вікна перезапуску і вдвічі менше за "кілька
// хвилин" з інциденту. Порядок величини тут і є відповіддю: десятки секунд, не
// секунди (клали б усіх на кожному деплої) і не десятки хвилин (рубильник знову
// був би декоративним). При pollInterval=2 с і таймауті клієнта 5 с це 13-45
// провалених спроб — справжня недоступність, а не блимання.
const defaultStaleAfter = 90 * time.Second

// staleAfterFromEnv — поріг з оточення; невалідне/непарсабельне/нульове
// значення НЕ вимикає рубильник, а лишає дефолт (мовчазне "0 = вимкнено" було б
// найгіршим варіантом: fail-open через друкарську помилку в env).
// sharedERPTransport — один транспорт на весь процес: keep-alive тримає
// зʼєднання до ERP живим між тіками, тож типовий опит коштує один HTTP-запит,
// а не «TCP + TLS + запит» кожні дві секунди.
var (
	erpTransportOnce sync.Once
	erpTransport     *http.Transport
)

func sharedERPTransport() *http.Transport {
	erpTransportOnce.Do(func() {
		erpTransport = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
			ForceAttemptHTTP2:   true,
		}
	})
	return erpTransport
}

// revokeTimeoutFromEnv — стеля одного запиту відкликань. Дефолт 8 с: ендпоїнт
// відповідає за ~0.2 с, тож 8 с ловить лише справжню недоступність ERP, а не
// звичайне мережеве тремтіння.
func revokeTimeoutFromEnv() time.Duration {
	if v := strings.TrimSpace(os.Getenv("OO_SCREEN_REVOKE_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("subscribeRevoke: OO_SCREEN_REVOKE_TIMEOUT=%q не розібрано, беру 8s", v)
	}
	return 8 * time.Second
}

func staleAfterFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv(envStaleAfter))
	if v == "" {
		return defaultStaleAfter
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("subscribeRevoke: %s=%q не є додатною тривалістю, беру дефолт %s", envStaleAfter, v, defaultStaleAfter)
		return defaultStaleAfter
	}
	return d
}

// staleGate тримає рівно те, що потрібно для переходу "м'яко -> твердо", і
// нічого більше: коли востаннє опитування ВДАЛОСЬ і чи ми вже обірвали в цьому
// епізоді недоступності.
//
// Рахуємо ЧАС від останнього успіху, а не кількість підряд невдач: кількість
// бреше — при pollInterval=1 с "30 невдач" це 30 секунд, при 5 с — дві з
// половиною хвилини. Поріг же виражений у тому, що болить: скільки часу дозвіл
// непідтверджений.
//
// 🔴 lastOK нульове = успіху ще НЕ БУЛО ЖОДНОГО (свіжий старт хаба, ERP лежить
// із самого початку). Це НЕ те саме, що "успіх був давно": застарілого дозволу,
// якому можна перестати вірити, просто не існує — вірити не було чому. Рвати
// тут не можна і не треба: у ticket-режимі жодна viewer-нога не заходить без
// успішного ConsumeTicket на тому ж ERP (ticket.go, fail-closed), тож поки ERP
// мовчить, нових глядачів і так не з'являється. Плутанина цих двох станів
// клала б усіх одразу після кожного перезапуску хаба.
// revokeLastOK — unix ms останнього вдалого пола ревокацій (0 = ще не було);
// читає /healthz (H-16): застарілий пол = хаб не може підтвердити дозволи.
var revokeLastOK atomic.Int64

// RevokePollAge — скільки часу минуло від останнього вдалого пола; ok=false,
// якщо вдалого ще не було.
func RevokePollAge(now time.Time) (time.Duration, bool) {
	ms := revokeLastOK.Load()
	if ms == 0 {
		return 0, false
	}
	return now.Sub(time.UnixMilli(ms)), true
}

type staleGate struct {
	after    time.Duration
	url      string
	onRevoke func(kind, val string)

	lastOK time.Time // нуль = успіху ще не було ЖОДНОГО
	fired  bool      // вже обірвали в ЦЬОМУ епізоді недоступності
}

// observe — один тік поллінгу: ok=true, якщо ERP відповів. Обриває сесії рівно
// один раз на епізод; успішна відповідь повертає все до нормальної поведінки
// БЕЗ перезапуску хаба.
func (g *staleGate) observe(now time.Time, ok bool) {
	if ok {
		g.lastOK = now
		g.fired = false
		revokeLastOK.Store(now.UnixMilli())
		return
	}
	if g.lastOK.IsZero() || g.fired {
		return
	}
	down := now.Sub(g.lastOK)
	if down < g.after {
		return
	}
	g.fired = true
	log.Printf("runtime-revoke: ОБРИВАЮ ВСІ СЕСІЇ — ERP %s не відповідає вже %s (поріг %s, остання вдала відповідь %s): hub не може підтвердити, що доступ ще чинний, тому падає в fail-closed; це недоступність ERP, а не збій хаба",
		g.url, down.Round(time.Second), g.after, g.lastOK.Format(time.RFC3339))
	g.onRevoke(RevokeKindStale, down.Round(time.Second).String())
}

// subscribeRevoke кожні pollInterval (0 -> дефолт 2с) робить GET
// {erpBase}/remote-access/screen/revocations?since=<since_ms> із заголовком
// X-OO-Hub-Key, і для кожного нового запису кличе onRevoke("node", node_id)
// або onRevoke("user", user_id) — залежно від того, яке з полів непорожнє.
// Курсор since рухається вперед на max(at) серед ОБРОБЛЕНИХ у цьому тіку
// записів; HTTP/JSON-помилки логуються й НЕ рухають since (наступний тік
// повторить той самий діапазон — fail-soft, не fail-closed і не fail-open).
//
// Блокується, поки ctx не скасовано (виклик — go subscribeRevoke(...)).
func SubscribeRevoke(ctx context.Context, erpBase, hubKey string, pollInterval time.Duration, onRevoke func(kind, val string)) {
	if strings.TrimSpace(erpBase) == "" {
		log.Printf("subscribeRevoke: erpBase порожній, поллінг не запущено")
		return
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}

	url := strings.TrimRight(erpBase, "/") + "/remote-access/screen/revocations"
	// H-32, друга ітерація. Перша спроба зробила таймаут КОРОТШИМ за інтервал
	// (pollInterval - pollInterval/6 ≈ 1.67 с при інтервалі 2 с), щоб «повільний
	// тік не зʼїдав наступний». Замір показав, що це і було причиною: сам
	// ендпоїнт віддає 200 за ~0.20 с (12 послідовних вимірів із самого VPS),
	// а «erp unreachable: context deadline» усе одно капало 12 разів на годину
	// — 1.67 с не вистачає, коли до відповіді додається новий TLS-handshake.
	//
	// Тіків це не стакає в принципі: у time.Ticker канал місткістю 1 і він
	// ДРОПАЄ тік, якщо отримувач зайнятий. Довгий запит лише зсуває наступний
	// опит, черга не росте — отже тиснути таймаут донизу не було потреби.
	//
	// Тому: щедрий таймаут (env OO_SCREEN_REVOKE_TIMEOUT, дефолт 8 с) і СПІЛЬНИЙ
	// транспорт із keep-alive, щоб кожні 2 секунди не піднімати нове TLS-зʼєднання.
	client := &http.Client{Timeout: revokeTimeoutFromEnv(), Transport: sharedERPTransport()}

	since := time.Now().Add(-pollInterval).UnixMilli()
	gate := &staleGate{after: staleAfterFromEnv(), url: url, onRevoke: onRevoke}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			var ok bool
			since, ok = pollRevocationsOnce(ctx, client, url, hubKey, since, onRevoke)
			gate.observe(now, ok)
		}
	}
}

// pollRevocationsOnce — один тік поллінгу. Повертає новий курсор since (той
// самий since при будь-якій помилці — fail-soft) і чи опитування ВДАЛОСЬ.
// Другий результат — вхід staleGate: вдалим вважається лише повний цикл
// "2xx + розібраний JSON", бо саме він означає, що дозвіл щойно підтверджено.
func pollRevocationsOnce(ctx context.Context, client *http.Client, url, hubKey string, since int64, onRevoke func(kind, val string)) (int64, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"?since="+strconv.FormatInt(since, 10), nil)
	if err != nil {
		log.Printf("subscribeRevoke: build request: %v", err)
		return since, false
	}
	req.Header.Set("X-OO-Hub-Key", hubKey)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("subscribeRevoke: erp unreachable: %v", err)
		return since, false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("subscribeRevoke: read body: %v", err)
		return since, false
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("subscribeRevoke: erp status %d: %s", resp.StatusCode, string(body))
		return since, false
	}

	var entries []revocationEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		log.Printf("subscribeRevoke: bad json from erp: %v (body=%q)", err, string(body))
		return since, false
	}

	next := since
	for _, e := range entries {
		switch {
		case strings.TrimSpace(e.NodeID) != "":
			onRevoke("node", e.NodeID)
		case strings.TrimSpace(e.UserID) != "":
			onRevoke("user", e.UserID)
		default:
			log.Printf("subscribeRevoke: запис без node_id і user_id, пропущено: %s", fmt.Sprintf("at=%d", e.At))
			continue
		}
		if e.At > next {
			next = e.At
		}
	}

	return next, true
}
