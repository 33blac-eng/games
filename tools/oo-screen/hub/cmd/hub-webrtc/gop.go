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
	"time"

	"github.com/pion/rtp"
)

const (
	// gopMaxPackets — запобіжна стеля кешу в ПАКЕТАХ. Основна межа тепер у
	// байтах (gopByteBudget), а ця лише гарантує, що навіть потік із крихітних
	// пакетів не роздує зріз вказівників. 8192 ≈ 10 с на 8 Мбіт/с (~830 пак/с).
	gopMaxPackets = 8192
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
	// gopDefaultSpan — типова стеля кешу в часі RTP. GOP агента — 2 с; довший
	// «GOP» означає, що IDR ми пропустили, і хвіст уже не самодостатній.
	gopDefaultSpan = 3 * time.Second
	// gopSpanLimit — найбільший проміжок, який дозволяє OO_SCREEN_GOP_SPAN.
	// Кеш на хвилину — це вже не «миттєвий старт», а відео хвилинної давнини;
	// пам'ять при цьому однаково тримає gopMaxBytes.
	gopSpanLimit = 30 * time.Second
	// gopClockRate — та сама, що й у треку глядача (H.264, 90 кГц).
	gopClockRate = 90000
)

// gopMaxSpan — стеля кешу в часі RTP. Налаштовується OO_SCREEN_GOP_SPAN
// (формат time.ParseDuration), коли агент кодує з довшим GOP: ставити її треба
// трохи БІЛЬШОЮ за GOP агента (GOP 2 с -> 3 с, GOP 8 с -> 10 с). Без змінної —
// сьогоднішні 3 с. Змінна, а не os.Getenv на місці: тести перемикають напряму.
var gopMaxSpan = clampGopSpan(envDuration("OO_SCREEN_GOP_SPAN", gopDefaultSpan))

// clampGopSpan обрізає проміжок до [gopDefaultSpan, gopSpanLimit]: коротша за
// типову стеля лише ламала б кеш на штатному GOP 2 с, довша за ліміт — див.
// gopSpanLimit.
func clampGopSpan(d time.Duration) time.Duration {
	if d < gopDefaultSpan {
		return gopDefaultSpan
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
// Пам'ять обмежена за будь-якого входу: найгірше — gopMaxBytes байтів payload-у
// плюс зріз на gopMaxPackets вказівників.
type gopCache struct {
	pkts []*rtp.Packet
	// overflow — кеш переповнився (за пакетами або за часом), тобто хвіст уже
	// НЕ безперервний від IDR. Віддавати такий не можна: глядач отримав би
	// картинку з діркою посередині замість чесної паузи до keyframe.
	overflow bool
	// inKey — попередній пакет належав ключовому набору. Потрібен саме фронт
	// (перехід «не ключовий» -> «ключовий»): SPS, PPS і фрагменти IDR ідуть
	// підряд, і скидати кеш на кожному з них означало б викинути SPS/PPS, без
	// яких сам IDR не декодується.
	inKey bool
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
func (g *gopCache) note(pkt *rtp.Packet) {
	key := h264KeyPart(pkt.Payload)
	if key && !g.inKey {
		g.drop()
		g.overflow = false
		g.startTS = pkt.Timestamp
	}
	g.inKey = key
	if g.overflow {
		return
	}
	if len(g.pkts) == 0 && !key {
		return // ще не бачили жодного ключового набору — накопичувати нема від чого
	}
	size := len(pkt.Payload) + gopPacketOverhead
	if len(g.pkts) >= gopMaxPackets ||
		g.bytes+size > gopByteBudget(g.bps) ||
		pkt.Timestamp-g.startTS > uint32(gopMaxSpan.Seconds()*gopClockRate) {
		g.overflow = true
		g.drop()
		return
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
}

// reset викидає кеш цілком. H-07: на заміні агента вміст лишається від
// ПОПЕРЕДНЬОГО кодера — чужі SPS/PPS і кадри, яких у новому потоці вже немає.
// Порожній кеш чесніший за такий: нова нога просто чекає IDR нового агента.
// Кликати під ns.mu, як і note/replay.
func (g *gopCache) reset() {
	g.drop()
	g.overflow = false
	g.inKey = false
	g.startTS = 0
}

// replay — вміст кешу для нового глядача, або nil, якщо віддавати нічого (кеш
// порожній чи розірваний). Кликати під ns.mu; зріз читається одразу ж, під тим
// самим локом.
func (g *gopCache) replay() []*rtp.Packet {
	if g.overflow || len(g.pkts) == 0 {
		return nil
	}
	return g.pkts
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
