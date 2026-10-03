// GOP-кеш ноди (пункт 41). Новий глядач приєднується посеред GOP: до
// найближчого IDR декодеру немає з чого почати кадр, і людина 0–2 с дивиться на
// сірий прямокутник. Просити позачерговий IDR (requestKeyframe) — це і затримка
// в один RTT до агента, і сплеск бітрейту рівно в момент, коли на ноді стало
// на одного глядача більше.
//
// Тому хаб тримає ХВІСТ ПОТОКУ від останнього ключового набору (SPS/PPS/IDR) і
// віддає його новому глядачеві першим — картинка зʼявляється з першого ж
// прочитаного пакета, а агент нічого зайвого не кодує.
//
// КЛЮЧОВЕ ПРО НУМЕРАЦІЮ: у кеші лежать ВЖЕ ПЕРЕПИСАНІ egress-пакети
// (forwardToViewers), а egress-простір seq/ts у ноди ОДИН на всіх глядачів і
// монотонний на весь час її життя. Тому відтворення кешу новій нозі не створює
// власної нумерації: вона просто починає читати потік ноди трохи раніше за
// точку приєднання, рівно як інші глядачі його вже прочитали.
package main

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/pion/rtp"
)

const (
	// gopMaxPackets — запобіжна стеля кешу в ПАКЕТАХ. Основна межа тепер у
	// байтах (gopByteBudget), а ця лише гарантує, що навіть потік із крихітних
	// пакетів не роздує зріз вказівників. 16384 ≈ 20 с на 8 Мбіт/с (~830
	// пак/с): з GOP агента 10 с (-gop-seconds) кеш упирається в байти
	// (gopMaxBytes), а не в цей запобіжник — 8192 різав хвіст GOP 10 с на
	// 8 Мбіт/с (RESULTS-hub.md S5).
	gopMaxPackets = 16384
	// gopMaxBytes — жорстка стеля кешу в БАЙТАХ на одну ноду, незалежно від
	// бітрейту й прапорця: що б не прислав агент, більше за це хаб не тримає.
	gopMaxBytes = 12 << 20
	// gopMinBytes — нижня межа бюджету: на дуже низькому бітрейті один IDR
	// усе одно важить сотні кілобайт, і бюджет «бітрейт × проміжок» його б
	// відрізав.
	gopMinBytes = 1 << 20
	// gopBudgetHeadroom — множник над «бітрейт × проміжок»: IDR у кілька разів
	// важчий за P-кадр, а кодер на сплесках виходить за ціль.
	gopBudgetHeadroom = 2
	// gopPacketOverhead — скільки байтів рахуємо на пакет понад payload
	// (заголовок RTP + сама структура). Без цього потік із порожніх payload-ів
	// не впирався б у байтову стелю зовсім.
	gopPacketOverhead = 64
	// gopDefaultSpan — типова стеля кешу в часі RTP. GOP агента — 10 с
	// (-gop-seconds), стеля трохи більша: довший «GOP» означає, що IDR ми
	// пропустили, і хвіст уже не самодостатній. Памʼять однаково тримає
	// gopMaxBytes; на переповненні (високий бітрейт) нога не primed і хаб
	// просить keyframe — агент відповідає IDR.
	gopDefaultSpan = 12 * time.Second
	// gopMinSpan — найменша стеля, яку дозволяє OO_SCREEN_GOP_SPAN (агент із
	// -gop-seconds 2 може жити з 3 с і меншим хвостом на приєднанні).
	gopMinSpan = 3 * time.Second
	// gopSpanLimit — найбільший проміжок, який дозволяє OO_SCREEN_GOP_SPAN.
	// Кеш на хвилину — це вже не «миттєвий старт», а відео хвилинної давнини;
	// пам'ять при цьому однаково тримає gopMaxBytes.
	gopSpanLimit = 30 * time.Second
	// gopClockRate — та сама, що й у треку глядача (H.264, 90 кГц).
	gopClockRate = 90000
)

// Бюджет ВІДТВОРЕННЯ кешу новому глядачеві (окремо від бюджету зберігання).
// Кеш віддається нозі одним шматком: з GOP 10 с на 8 Мбіт/с це до ~11 МБ, тобто
// секунди передачі на повільному каналі глядача. Тому віддаємо хвіст лише коли
// він невеликий — до gopReplaySpan на поточному бітрейті ноди, але не більше
// gopReplayMaxBytes; інакше нога не primed і хаб просить keyframe (дебаунс
// спільний, тож пачка приєднань ділить один IDR). Нижня межа gopMinBytes (не
// більша за стелю): на низькому бітрейті сам IDR важить сотні КБ.
//
// OO_SCREEN_GOP_REPLAY_SPAN (time.ParseDuration, дефолт 2s) і
// OO_SCREEN_GOP_REPLAY_MAX_BYTES (байти, дефолт 3 МБ, ≤ gopMaxBytes).
var (
	gopReplaySpan     = envDuration("OO_SCREEN_GOP_REPLAY_SPAN", 2*time.Second)
	gopReplayMaxBytes = envBytes("OO_SCREEN_GOP_REPLAY_MAX_BYTES", 3<<20)
)

// gopReplayBudget — скільки байтів кешу (payload + gopPacketOverhead) можна
// віддати новому глядачеві при бітрейті ноди bps; 0 = невідомий -> стеля.
func gopReplayBudget(bps uint64) int {
	ceil := gopReplayMaxBytes
	if ceil > gopMaxBytes {
		ceil = gopMaxBytes
	}
	if ceil < 0 {
		ceil = 0
	}
	floor := min(gopMinBytes, ceil)
	if bps == 0 {
		return ceil
	}
	b := float64(bps) / 8 * gopReplaySpan.Seconds() * 1.25 // +25 %: IDR і заголовки (GOP 2 с на 8 Мбіт/с вміщається)
	if b >= float64(ceil) {
		return ceil
	}
	if b < float64(floor) {
		return floor
	}
	return int(b)
}

// envBytes — ціле число байтів зі змінної середовища; нерозбірне чи від'ємне
// значення -> типове (з логом, як envDuration).
func envBytes(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("%s=%q не розібралось — беру типове %d", k, v, def)
		return def
	}
	return n
}

// gopMaxSpan — стеля кешу в часі RTP. Налаштовується OO_SCREEN_GOP_SPAN
// (формат time.ParseDuration), коли агент кодує з іншим GOP: ставити її треба
// трохи БІЛЬШОЮ за GOP агента (GOP 2 с -> 3 с, GOP 10 с -> 12 с). Без змінної —
// 12 с під типовий GOP агента 10 с. Змінна, а не os.Getenv на місці: тести перемикають напряму.
var gopMaxSpan = clampGopSpan(envDuration("OO_SCREEN_GOP_SPAN", gopDefaultSpan))

// clampGopSpan обрізає проміжок до [gopMinSpan, gopSpanLimit]: коротша
// стеля ламала б кеш навіть на GOP 2 с, довша за ліміт — див. gopSpanLimit.
func clampGopSpan(d time.Duration) time.Duration {
	if d < gopMinSpan {
		return gopMinSpan
	}
	if d > gopSpanLimit {
		return gopSpanLimit
	}
	return d
}

// gopByteBudget — байтова стеля кешу для бітрейту bps (біт/с): бітрейт ×
// gopMaxSpan × gopBudgetHeadroom, затиснута в [gopMinBytes, gopMaxBytes].
// bps == 0 (бітрейт ще невідомий) — жорстка стеля: пам'ять однаково обмежена.
func gopByteBudget(bps uint64) int {
	if bps == 0 {
		return gopMaxBytes
	}
	// float: будь-яке безглузде bps (аж до MaxUint64) просто впреться в стелю.
	b := float64(bps) / 8 * gopMaxSpan.Seconds() * gopBudgetHeadroom
	if b >= gopMaxBytes {
		return gopMaxBytes
	}
	if b < gopMinBytes {
		return gopMinBytes
	}
	return int(b)
}

// gopCache — хвіст потоку ноди від останнього ключового набору. Весь стан під
// ns.mu (пишеться з forwardToViewers, читається з recomputeBinding).
//
// Межі (будь-яка з них -> overflow, тобто порожній кеш до наступного IDR):
//   - байти: gopByteBudget(bps) — бітрейт ноди × проміжок, не більше gopMaxBytes;
//   - пакети: gopMaxPackets — запобіжник;
//   - час RTP: gopMaxSpan.
//
// Пам'ять обмежена за будь-якого входу: найгірше — gopMaxBytes байтів payload-у
// плюс зріз на gopMaxPackets вказівників.
type gopCache struct {
	pkts []*rtp.Packet
	// overflow — кеш переповнився (за пакетами або за часом), тобто хвіст уже
	// НЕ безперервний від IDR. Віддавати такий не можна: глядач отримав би
	// картинку з діркою посередині замість чесної паузи до keyframe.
	overflow bool
	// B1: межа кешу — ACCESS UNIT, а не «фронт ключового пакета». AU у RTP —
	// усі пакети з одним timestamp. Кеш перезапускається лише на ПЕРШОМУ
	// ключовому пакеті (SPS/PPS/IDR у будь-якому пакуванні) НОВОГО AU, і далі
	// в нього йде весь цей AU: усі слайси IDR (між FU-A-фрагментами яких раніше
	// був «фронт»), SEI між PPS та IDR, і навіть SEI/AUD, що стояли в AU ДО SPS
	// (їх тримає auPkts). Кілька слайсів одного IDR кеш більше не рвуть.
	//
	// haveAU/auTS — timestamp поточного AU; auKey — у поточному AU кеш уже
	// перезапущено; auPkts — пакети поточного AU до його першого ключового
	// пакета (лише поки !auKey; на зміні AU спорожнюється).
	haveAU bool
	auTS   uint32
	auKey  bool
	auPkts []*rtp.Packet
	// hasSPS/hasPPS/hasIDR — що з ключового набору є в ПЕРШОМУ AU кешу. Кеш
	// самодостатній (декодер почне з першого пакета) лише коли є всі три;
	// інакше replay() нічого не віддає, і нова нога просить keyframe, а не
	// вважається primed із кадром, якого не декодувати.
	hasSPS, hasPPS, hasIDR bool
	// startTS — RTP-timestamp першого пакета в кеші (межа gopMaxSpan).
	startTS uint32
	// bytes — скільки байтів (payload + gopPacketOverhead) зараз у кеші.
	bytes int
	// bps — бітрейт ноди для байтового бюджету; 0 = невідомий (жорстка стеля).
	// Виставляє forwardToViewers через setBitrate, під ns.mu.
	bps uint64
}

// setBitrate оновлює бітрейт, з якого рахується байтовий бюджет. Уже
// накопичений хвіст не чіпає: якщо він тепер більший за бюджет, наступний
// note() переведе кеш в overflow — так само чесно, як будь-яке переповнення.
func (g *gopCache) setBitrate(bps uint64) { g.bps = bps }

// note кладе черговий egress-пакет ноди у кеш. Кликати під ns.mu, ОДРАЗУ після
// перепису seq/ts і з тим самим вказівником, що йде в черги глядачів: пакет
// після цього ніхто не мутує (див. forwardToViewers).
//
// Повертає true, якщо цей пакет ПОЧАВ новий ключовий AU — єдина точка, з якої
// декодер уміє стартувати (нею ж forwardToViewers знімає drop-to-IDR).
func (g *gopCache) note(pkt *rtp.Packet) (keyStart bool) {
	if !g.haveAU || pkt.Timestamp != g.auTS {
		g.haveAU = true
		g.auTS = pkt.Timestamp
		g.auKey = false
		clear(g.auPkts)
		g.auPkts = g.auPkts[:0]
	}
	if !g.auKey && h264KeyPart(pkt.Payload) {
		// Перший ключовий пакет нового AU: новий кеш, і в нього — все, що
		// цей AU уже приніс (SEI/AUD перед SPS).
		g.auKey = true
		keyStart = true
		g.drop()
		g.overflow = false
		g.hasSPS, g.hasPPS, g.hasIDR = false, false, false
		g.startTS = pkt.Timestamp
		for _, p := range g.auPkts {
			g.add(p)
		}
		clear(g.auPkts)
		g.auPkts = g.auPkts[:0]
	} else if !g.auKey {
		// Пакети AU до його першого ключового: тримаємо, поки AU не скінчився
		// (зазвичай це P-кадр — тоді на зміні AU вони просто відпускаються).
		if len(g.auPkts) < 64 {
			g.auPkts = append(g.auPkts, pkt)
		}
	}
	if len(g.pkts) == 0 && !g.auKey {
		return false // ще не бачили жодного ключового набору — накопичувати нема від чого
	}
	g.add(pkt)
	return keyStart
}

// add — один пакет у кеш з усіма межами (overflow -> порожній кеш до
// наступного ключового AU).
func (g *gopCache) add(pkt *rtp.Packet) {
	if g.overflow {
		return
	}
	size := len(pkt.Payload) + gopPacketOverhead
	if len(g.pkts) >= gopMaxPackets ||
		g.bytes+size > gopByteBudget(g.bps) ||
		pkt.Timestamp-g.startTS > uint32(gopMaxSpan.Seconds()*gopClockRate) {
		g.overflow = true
		g.drop()
		return
	}
	if pkt.Timestamp == g.startTS {
		h264NALTypes(pkt.Payload, func(t byte) {
			switch t {
			case 7:
				g.hasSPS = true
			case 8:
				g.hasPPS = true
			case 5:
				g.hasIDR = true
			}
		})
	}
	g.pkts = append(g.pkts, pkt)
	g.bytes += size
}

// drop спорожнює кеш і ВІДПУСКАЄ пам'ять. Просто g.pkts[:0] лишав би живими
// вказівники на до gopMaxBytes payload-ів у хвості масиву аж до наступного
// заповнення; великий масив після рідкісного довгого GOP не тримаємо зовсім.
func (g *gopCache) drop() {
	if cap(g.pkts) > 1024 {
		g.pkts = nil
	} else {
		clear(g.pkts[:cap(g.pkts)])
		g.pkts = g.pkts[:0]
	}
	g.bytes = 0
	g.hasSPS, g.hasPPS, g.hasIDR = false, false, false
}

// reset викидає кеш цілком і віддає ВСЮ його пам'ять (B3). H-07: на заміні
// агента вміст лишається від ПОПЕРЕДНЬОГО кодера — чужі SPS/PPS і кадри, яких
// у новому потоці вже немає. B2: те саме на втраті агента — кеш мертвого
// кодера не має дожити ні до нового агента, ні до кінця життя запису ноди.
// Кликати під ns.mu, як і note/replay.
func (g *gopCache) reset() {
	g.pkts = nil
	g.bytes = 0
	g.hasSPS, g.hasPPS, g.hasIDR = false, false, false
	g.overflow = false
	g.haveAU, g.auKey, g.auTS = false, false, 0
	g.auPkts = nil
	g.startTS = 0
}

// selfContained — кеш починається з повного ключового набору (SPS+PPS+IDR у
// першому AU), тобто декодер нової ноги почне з першого ж пакета.
func (g *gopCache) selfContained() bool {
	return !g.overflow && len(g.pkts) > 0 && g.hasSPS && g.hasPPS && g.hasIDR
}

// replay — вміст кешу для нового глядача, або nil, якщо віддавати нічого (кеш
// порожній, розірваний або не самодостатній — тоді нога просить keyframe).
// Кликати під ns.mu; зріз читається одразу ж, під тим самим локом.
func (g *gopCache) replay() []*rtp.Packet {
	if !g.selfContained() {
		return nil
	}
	return g.pkts
}

// replayFor — replay з бюджетом відтворення: tooBig = кеш самодостатній, але
// важчий за gopReplayBudget — нозі краще IDR на запит, ніж мегабайти хвоста.
func (g *gopCache) replayFor() (pkts []*rtp.Packet, bytes int, tooBig bool) {
	pkts = g.replay()
	if pkts == nil {
		return nil, 0, false
	}
	if g.bytes > gopReplayBudget(g.bps) {
		return nil, g.bytes, true
	}
	return pkts, g.bytes, false
}

// h264NALTypes викликає f для типу кожного NAL, що ПОЧИНАЄТЬСЯ в цьому
// RTP-payload: одиничний NAL, кожен NAL зі STAP-A, початковий фрагмент FU-A.
func h264NALTypes(p []byte, f func(byte)) {
	if len(p) == 0 {
		return
	}
	switch t := p[0] & 0x1F; t {
	case 24:
		for i := 1; i+2 < len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			if n == 0 || i+2+n > len(p) {
				return
			}
			f(p[i+2] & 0x1F)
			i += 2 + n
		}
	case 28:
		if len(p) > 1 && p[1]&0x80 != 0 {
			f(p[1] & 0x1F)
		}
	default:
		f(t)
	}
}

// h264KeyPart — чи несе цей RTP-payload частину ключового набору H.264:
// SPS(7), PPS(8) або IDR(5). Розбираються три пакування RFC 6184, які реально
// шле наш кодер: одиничний NAL, STAP-A (24) і FU-A (28).
//
// Для FU-A рахується ЛИШЕ початковий фрагмент: середина IDR-кадру сама по собі
// нічого не починає, і фронт на ній зайвий раз рвав би кеш.
func h264KeyPart(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	switch p[0] & 0x1F {
	case 5, 7, 8:
		return true
	case 24: // STAP-A: [hdr][len16][NAL]...
		for i := 1; i+2 < len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			if n == 0 || i+2+n > len(p) {
				return false
			}
			switch p[i+2] & 0x1F {
			case 5, 7, 8:
				return true
			}
			i += 2 + n
		}
		return false
	case 28: // FU-A: [hdr][S|E|R|type]
		return len(p) > 1 && p[1]&0x80 != 0 && p[1]&0x1F == 5
	}
	return false
}
