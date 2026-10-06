// Package keyframe — коли агентові самому вставляти періодичний IDR
// (TASK.md крок 4, OO_SCREEN_IDLE_IDR).
//
// Без політики періодичний IDR ставить сам MFT: кожні GOP ЗАКОДОВАНИХ кадрів
// (-gop-seconds × fps), куди б це не впало. Падає воно здебільшого в рух
// (на нерухомому екрані кодується лише keepalive раз на секунду), тобто
// важкий I-кадр іде саме тоді, коли канал і так зайнятий: черга на лінку
// стрибає, rate control на наступних кадрах «віддає борг» і текст під час
// прокрутки мильніє.
//
// Політика переносить IDR у тишу: коли GOP уже вийшов, IDR вставляється на
// першому кадрі, перед яким екран стояв щонайменше Idle. Запобіжник — сам
// MFT із GOP = EncoderGOP (2×): якщо тиші так і не настало (довге відео),
// він вставить IDR сам, як раніше, лише вдвічі рідше.
//
// Типово ВИМКНЕНО: симуляція (bench/quality/ratecontrol_run.py --joins "",
// mixed 60 с) виграшу не показала — з пропуском незмінених кадрів
// періодичний IDR MFT і так падає на набір, а не на прокрутку, а IDR у тиші
// на 2M — мило на весь екран, яке x264 у симуляції не дошліфовує (row-level
// VBV перекриває QP refine). Лишається як опція для пілоту на залізі.
//
// Чистий код без годинника і cgo: час і події передає викликач.
package keyframe

import "time"

// Config — пороги політики. Нульові поля = дефолти.
type Config struct {
	// GOPFrames — бажаний інтервал IDR у закодованих кадрах (як GOP MFT).
	GOPFrames int
	// Idle — скільки екран мусить стояти, щоб IDR вважався «у тиші».
	Idle time.Duration
	// HardFactor — у скільки разів GOP енкодера (запобіжник) довший за
	// GOPFrames. 0 -> DefaultHardFactor.
	HardFactor int
}

// Дефолти.
const (
	DefaultIdle       = 500 * time.Millisecond
	DefaultHardFactor = 2
)

// Policy — автомат. Не потокобезпечний: живе в кадровому циклі.
type Policy struct {
	cfg        Config
	since      int       // закодованих кадрів від останнього IDR
	lastMotion time.Time // останній кадр з новим вмістом
}

// New будує політику; cfg.GOPFrames <= 0 вимикає її (Due завжди false).
func New(cfg Config) *Policy {
	if cfg.Idle <= 0 {
		cfg.Idle = DefaultIdle
	}
	if cfg.HardFactor <= 1 {
		cfg.HardFactor = DefaultHardFactor
	}
	return &Policy{cfg: cfg}
}

// EncoderGOP — GOP, який ставити в MFT: запобіжник, якщо тиші не буде.
func (p *Policy) EncoderGOP() int {
	if p.cfg.GOPFrames <= 0 {
		return 0
	}
	return p.cfg.GOPFrames * p.cfg.HardFactor
}

// Motion — прийшов кадр із новим вмістом.
func (p *Policy) Motion(now time.Time) { p.lastMotion = now }

// Coded — енкодер віддав AU; key — IDR (будь-чий: свій, на запит, від MFT).
func (p *Policy) Coded(key bool) {
	if key {
		p.since = 0
		return
	}
	p.since++
}

// Due — чи просити IDR на кадрі, який зараз кодуватиметься. still — це
// повтор нерухомого екрана (keepalive), не новий вміст і не refine.
func (p *Policy) Due(now time.Time, still bool) bool {
	if p.cfg.GOPFrames <= 0 || !still || p.since < p.cfg.GOPFrames {
		return false
	}
	return now.Sub(p.lastMotion) >= p.cfg.Idle
}

// Since — закодованих кадрів від останнього IDR (діагностика).
func (p *Policy) Since() int { return p.since }
