// Package contentmode — ЄДИНИЙ автомат режиму вмісту агента: Text / Normal /
// Video (TZ-GENERAL P1, Q3/Q4/L5).
//
//   - Text — набір/читання: дрібні dirty rects (internal/textmode, стеля
//     -text-fps).
//   - Video — тривалий рух великої площі (відтворення відео, прокрутка,
//     перетягування): агент може підняти частоту до -video-fps (60) і
//     попросити хаб підняти ціль бітрейту в межах стелі.
//   - Normal — усе інше.
//
// Пріоритет: Video > Text > Normal. Обидва детектори оновлюються на кожному
// кадрі, тож вихід із Video повертає саме той стан, який зараз бачить
// текстовий детектор (Text або Normal), а не «скидає» його. Video і Text
// одночасно не існують: Video виграє (текстова стеля у Video не діє ніколи).
//
// Пакет чистий (без Windows, без годинника): час передає викликач.
package contentmode

import (
	"time"

	"github.com/organicoils/oo-screen/internal/textmode"
)

// Mode — режим вмісту.
type Mode int

const (
	Normal Mode = iota
	Text
	Video
)

func (m Mode) String() string {
	switch m {
	case Text:
		return "text"
	case Video:
		return "video"
	default:
		return "normal"
	}
}

// Дефолти детектора відео.
const (
	// DefaultVideoEnterArea — частка екрана, змінена (dirty+move) у КОЖНОМУ
	// кадрі серії, щоб серія рахувалась у вхід. 0.10: відео 640×360 у куті
	// 1080p — 11 %; набір/курсор — < 1 %.
	DefaultVideoEnterArea = 0.10
	// DefaultVideoExitArea — нижче цього кадр не «підтримує» режим (гістерезис:
	// вхід 10 %, утримання ≥ 4 %).
	DefaultVideoExitArea = 0.04
	// DefaultEnterHold — скільки серія великих змін має тривати до входу.
	DefaultEnterHold = time.Second
	// DefaultExitHold — скільки без підтримуючих кадрів до виходу (пауза
	// плеєра, кінець прокрутки).
	DefaultExitHold = 1500 * time.Millisecond
	// DefaultMaxGap — більша пауза між великими кадрами рве серію входу:
	// одиночні великі зміни (перемикання вікна, відкриття меню) раз на секунду
	// — не відео.
	DefaultMaxGap = 250 * time.Millisecond
)

// Config — пороги автомата. Нульові поля = дефолти.
type Config struct {
	Text      textmode.Config
	EnterArea float64
	ExitArea  float64
	EnterHold time.Duration
	ExitHold  time.Duration
	MaxGap    time.Duration
	// NoVideo вимикає режим Video (прапорець -video-mode=false): автомат тоді
	// рівно текстовий детектор.
	NoVideo bool
}

func (c Config) withDefaults() Config {
	if c.EnterArea <= 0 || c.EnterArea > 1 {
		c.EnterArea = DefaultVideoEnterArea
	}
	if c.ExitArea <= 0 || c.ExitArea > c.EnterArea {
		c.ExitArea = min(DefaultVideoExitArea, c.EnterArea)
	}
	if c.EnterHold <= 0 {
		c.EnterHold = DefaultEnterHold
	}
	if c.ExitHold <= 0 {
		c.ExitHold = DefaultExitHold
	}
	if c.MaxGap <= 0 {
		c.MaxGap = DefaultMaxGap
	}
	return c
}

// Machine — автомат режиму. Не потокобезпечний: живе в кадровому циклі.
type Machine struct {
	cfg    Config
	text   *textmode.Detector
	video  bool
	streak time.Time // початок серії великих кадрів (вхід); нуль = серії немає
	last   time.Time // останній великий кадр (серія) / підтримуючий кадр (Video)
	mode   Mode
}

// New створює автомат.
func New(cfg Config) *Machine {
	c := cfg.withDefaults()
	return &Machine{cfg: c, text: textmode.New(c.Text)}
}

// Area — частка руху кадру для детектора відео: dirty + move, обрізано до 1.
func Area(changed, moved float64) float64 {
	a := changed + moved
	if a != a || a < 0 {
		return 0
	}
	if a > 1 {
		return 1
	}
	return a
}

// Update — новий захоплений кадр з власними частками зміненої (dirty) і
// зсунутої (move) площі. Повертає режим і чи він змінився.
func (m *Machine) Update(changed, moved float64, now time.Time) (Mode, bool) {
	m.text.Update(changed, moved)
	if !m.cfg.NoVideo {
		a := Area(changed, moved)
		if m.video {
			if a >= m.cfg.ExitArea {
				m.last = now
			}
		} else if a >= m.cfg.EnterArea {
			if m.streak.IsZero() || now.Sub(m.last) > m.cfg.MaxGap {
				m.streak = now
			}
			m.last = now
			if now.Sub(m.streak) >= m.cfg.EnterHold {
				m.video, m.last = true, now
			}
		} else {
			m.streak = time.Time{}
		}
	}
	return m.settle(now)
}

// Tick — минув час без нового кадру (DXGI мовчить: екран став). Лише
// вихід із Video за ExitHold; текстовий детектор не чіпає (він кадровий).
func (m *Machine) Tick(now time.Time) (Mode, bool) {
	if !m.video && !m.streak.IsZero() && now.Sub(m.last) > m.cfg.MaxGap {
		m.streak = time.Time{}
	}
	return m.settle(now)
}

func (m *Machine) settle(now time.Time) (Mode, bool) {
	if m.video && now.Sub(m.last) >= m.cfg.ExitHold {
		m.video, m.streak = false, time.Time{}
	}
	prev := m.mode
	switch {
	case m.video:
		m.mode = Video
	case m.text.Text():
		m.mode = Text
	default:
		m.mode = Normal
	}
	return m.mode, m.mode != prev
}

// Mode — поточний режим.
func (m *Machine) Mode() Mode { return m.mode }

// TextDetector — внутрішній текстовий детектор (для логів Smoothed()).
func (m *Machine) TextDetector() *textmode.Detector { return m.text }

// Reset — новий енкодер/монітор: все з нуля.
func (m *Machine) Reset() {
	m.text.Reset()
	m.video, m.streak, m.last, m.mode = false, time.Time{}, time.Time{}, Normal
}

// FPSInput — що відомо агентові для рішення про частоту у Video.
type FPSInput struct {
	BaseFPS  int  // -fps (30): Normal/Text
	VideoFPS int  // -video-fps (60): бажана у Video
	Hardware bool // апаратний MFT
	// EncSec — EWMA часу кодування одного кадру (с), 0 = ще не виміряно.
	EncSec float64
	// MaxLoad — частка кадрового інтервалу VideoFPS, яку кодування може
	// зайняти (дефолт 0.6): на 60 к/с це 10 мс на кадр.
	MaxLoad float64
	// SoftwareFPS — скільки дозволяє софт-бюджет (internal/swlimit Policy.FPS()
	// з MaxFPS ≥ VideoFPS); 0 = не дозволяє (дефолт: софт-шлях не бустимо).
	SoftwareFPS int
}

// FPS — частота для режиму. Поза Video — BaseFPS. У Video: апаратний
// енкодер — VideoFPS, якщо вимірене кодування вміщується в MaxLoad на
// VideoFPS (невиміряне = не вміщується: спершу міряємо на базовій частоті);
// софт — лише до SoftwareFPS, якщо той вищий за базову. Ніколи не нижче
// BaseFPS і не вище VideoFPS.
func FPS(mode Mode, in FPSInput) int {
	base := in.BaseFPS
	if mode != Video || in.VideoFPS <= base {
		return base
	}
	if in.Hardware {
		maxLoad := in.MaxLoad
		if maxLoad <= 0 {
			maxLoad = 0.6
		}
		if in.EncSec > 0 && in.EncSec*float64(in.VideoFPS) <= maxLoad {
			return in.VideoFPS
		}
		return base
	}
	if in.SoftwareFPS > base {
		return min(in.SoftwareFPS, in.VideoFPS)
	}
	return base
}

// Gap — мінімальний інтервал між закодованими кадрами вмісту для fps; 0 = без
// обмеження.
func Gap(fps int) time.Duration {
	if fps <= 0 {
		return 0
	}
	return time.Second / time.Duration(fps)
}
