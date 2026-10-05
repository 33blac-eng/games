package contentmode

import "time"

// Стеля бітрейту для малорухомого вмісту (TZ-GENERAL R3, прапорець агента
// -lowmotion-cap, за замовчуванням ВИМКНЕНО).
//
// Навіщо: CBR-ціль (8 Мбіт/с на 1080p) розрахована на повноекранний рух
// (прокрутка, перетягування). Коли рухається лише дрібна ділянка (набір,
// курсор, відео в куті), енкодер усе одно витрачає майже всю ціль: симуляція
// (bench/quality/RESULTS-workloads.md) дає набору 299 МБ/год на 8M і
// 42 МБ/год на 2M. Тому:
//
//   - кадр із часткою руху ≥ FullArea — стелі НЕМАЄ одразу (перший же кадр
//     прокрутки/перетягування/перемикання вікна йде з повною ціллю);
//   - режим Video (тривалий рух ≥ 10 %), але кадр < FullArea — відео в куті:
//     VideoFrac від цілі;
//   - інакше (набір, курсор, дрібні зміни) — LowFrac від цілі.
//
// Знижуємо стелю лише після Hold безперервно «малих» кадрів (гістерезис, щоб
// не смикати SetBitrate між кадрами прокрутки); піднімаємо — миттєво.
// Нижче MinBps стеля не опускає ніколи. Чистий код, час передає викликач.
// Дзеркало для симуляції: bench/quality/lowmotion_run.py (ті самі пороги).

// Дефолти стелі.
const (
	DefaultFullArea  = 0.25
	DefaultLowFrac   = 0.25
	DefaultVideoFrac = 0.5
	DefaultLowHold   = 500 * time.Millisecond
	DefaultMinCapBps = 1_000_000
)

// CapConfig — пороги стелі. Нульові поля = дефолти.
type CapConfig struct {
	FullArea  float64
	LowFrac   float64
	VideoFrac float64
	Hold      time.Duration
	MinBps    int
}

func (c CapConfig) withDefaults() CapConfig {
	if c.FullArea <= 0 || c.FullArea > 1 {
		c.FullArea = DefaultFullArea
	}
	if c.LowFrac <= 0 || c.LowFrac > 1 {
		c.LowFrac = DefaultLowFrac
	}
	if c.VideoFrac <= 0 || c.VideoFrac > 1 {
		c.VideoFrac = DefaultVideoFrac
	}
	if c.Hold <= 0 {
		c.Hold = DefaultLowHold
	}
	if c.MinBps <= 0 {
		c.MinBps = DefaultMinCapBps
	}
	return c
}

// Capper — автомат стелі. Не потокобезпечний: живе в кадровому циклі.
type Capper struct {
	cfg      CapConfig
	frac     float64
	lowSince time.Time
}

// NewCapper створює автомат; старт — без стелі (frac 1).
func NewCapper(cfg CapConfig) *Capper { return &Capper{cfg: cfg.withDefaults(), frac: 1} }

// Update — новий захоплений кадр: режим автомата і частка руху (Area).
// Повертає множник цілі (1, VideoFrac або LowFrac).
func (c *Capper) Update(mode Mode, area float64, now time.Time) float64 {
	if area != area || area >= c.cfg.FullArea {
		c.frac, c.lowSince = 1, time.Time{}
		return c.frac
	}
	want := c.cfg.LowFrac
	if mode == Video {
		want = c.cfg.VideoFrac
	}
	if c.lowSince.IsZero() {
		c.lowSince = now
	}
	switch {
	case want > c.frac:
		c.frac = want
	case want < c.frac && now.Sub(c.lowSince) >= c.cfg.Hold:
		c.frac = want
	}
	return c.frac
}

// Frac — поточний множник.
func (c *Capper) Frac() float64 { return c.frac }

// Reset — новий енкодер/монітор: без стелі.
func (c *Capper) Reset() { c.frac, c.lowSince = 1, time.Time{} }

// Bps — ціль із урахуванням стелі: target·Frac, але не нижче MinBps і ніколи
// не вище target.
func (c *Capper) Bps(target int) int {
	if target <= 0 || c.frac >= 1 {
		return target
	}
	b := int(float64(target) * c.frac)
	if b < c.cfg.MinBps {
		b = c.cfg.MinBps
	}
	return min(b, target)
}
