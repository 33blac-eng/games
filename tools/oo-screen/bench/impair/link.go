// Package impair — in-process емуляція мережевих вад для UDP (замінник
// `tc qdisc ... netem`, коли немає root/NET_ADMIN/iproute2 — як у хмарному
// контейнері, де зроблено RESULTS-network.md).
//
// Link — чиста модель одного напрямку: для кожного пакета вирішує «дропнути»
// чи «доставити в момент T». Proxy (proxy.go) — UDP-реле, що застосовує дві
// такі моделі (hub->viewer і viewer->hub) до живих сокетів.
//
// Порядок застосування — як у netem+tbf: втрата (Бернуллі або Гілберт-Елліотт)
// -> обмеження смуги (серіалізація + черга з tail-drop) -> затримка + джитер ->
// перестановка.
package impair

import (
	"math/rand"
	"sync"
	"time"
)

// GE — двостанова марковська модель втрат Гілберта-Елліотта (як netem
// `loss gemodel p r 1-h 1-k`). P — шанс переходу good->bad на пакет, R —
// bad->good; у стані bad губиться LossBad пакетів, у good — LossGood.
// Середня частка втрат = P/(P+R)*LossBad + R/(P+R)*LossGood, середня
// довжина пачки = 1/R пакетів.
type GE struct {
	P, R, LossBad, LossGood float64
}

// Config — вади одного напрямку. Нульове значення = чиста лінія.
type Config struct {
	Loss    float64       // рівномірні втрати, частка 0..1
	Burst   *GE           // якщо не nil — замість Loss
	Delay   time.Duration // стала однобічна затримка
	Jitter  time.Duration // + рівномірно [-Jitter, +Jitter] на пакет
	Reorder float64       // частка пакетів, яким джитер дозволено обганяти чергу
	// RateBps > 0 — вузьке місце: пакети серіалізуються зі швидкістю RateBps,
	// а черга понад QueueBytes — tail-drop (як tbf limit).
	RateBps    float64
	QueueBytes int
}

// DropReason — чому пакет не доставлено.
type DropReason int

const (
	Delivered DropReason = iota
	DropLoss
	DropQueue
)

// Link — стан одного напрямку. Безпечний для конкурентних викликів.
type Link struct {
	mu        sync.Mutex
	cfg       Config
	rnd       *rand.Rand
	geBad     bool
	busyUntil time.Time // коли вузьке місце звільниться
	lastOut   time.Time // остання запланована доставка (порядок без Reorder)
}

func NewLink(cfg Config, seed int64) *Link {
	return &Link{cfg: cfg, rnd: rand.New(rand.NewSource(seed))}
}

// Set міняє вади на ходу (фази сценарію). Черга вузького місця не скидається:
// вже прийняті пакети вийдуть за старим розкладом, як у справжньому tc change.
func (l *Link) Set(cfg Config) {
	l.mu.Lock()
	l.cfg = cfg
	if cfg.Burst == nil {
		l.geBad = false
	}
	l.mu.Unlock()
}

func (l *Link) Config() Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

// Decide — доля пакета розміру size, що прийшов у now.
func (l *Link) Decide(now time.Time, size int) (time.Time, DropReason) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.cfg

	if c.Burst != nil {
		g := c.Burst
		if l.geBad {
			if l.rnd.Float64() < g.R {
				l.geBad = false
			}
		} else if l.rnd.Float64() < g.P {
			l.geBad = true
		}
		p := g.LossGood
		if l.geBad {
			p = g.LossBad
		}
		if p > 0 && l.rnd.Float64() < p {
			return time.Time{}, DropLoss
		}
	} else if c.Loss > 0 && l.rnd.Float64() < c.Loss {
		return time.Time{}, DropLoss
	}

	t := now
	if c.RateBps > 0 {
		start := now
		if l.busyUntil.After(start) {
			start = l.busyUntil
		}
		q := c.QueueBytes
		if q <= 0 {
			q = int(c.RateBps / 8 * 0.1) // дефолт: 100 мс черги
		}
		if backlog := float64(start.Sub(now)) / float64(time.Second) * c.RateBps / 8; backlog+float64(size) > float64(q) {
			return time.Time{}, DropQueue
		}
		l.busyUntil = start.Add(time.Duration(float64(size*8) / c.RateBps * float64(time.Second)))
		t = l.busyUntil
	}

	// Reorder як у netem: обрані пакети йдуть БЕЗ затримки/джитера і тому
	// обганяють тих, що в лінії (потрібна ненульова Delay, інакше обганяти нема кого).
	// Без явного Reorder лінія тримає порядок (як черга, що гойдається цілком):
	// пакет не може вийти раніше за попередній.
	mayReorder := c.Reorder > 0 && l.rnd.Float64() < c.Reorder
	if !mayReorder {
		t = t.Add(c.Delay)
		if c.Jitter > 0 {
			t = t.Add(time.Duration((l.rnd.Float64()*2 - 1) * float64(c.Jitter)))
		}
	}
	// Джитер, як і в netem, порядок НЕ тримає: пакет із меншим випадковим
	// зсувом обганяє сусіда (на 800 пак/с при ±30 мс це звична картина).
	if !mayReorder && c.Jitter == 0 && t.Before(l.lastOut) {
		t = l.lastOut
	}
	if t.Before(now) {
		t = now
	}
	if !mayReorder && t.After(l.lastOut) {
		l.lastOut = t
	}
	return t, Delivered
}
