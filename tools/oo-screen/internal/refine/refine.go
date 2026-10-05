// Package refine — рішення «дошліфувати нерухомий екран» (ТЗ P4, етап 2).
//
// Коли рух зупиняється, DXGI перестає віддавати кадри, і текст лишається з
// QP останнього «рухомого» кадру — мильним після скролу. Refine: через Idle
// без нових кадрів агент ще раз кодує ОСТАННІЙ кадр із нижчим QP (1–2 рази),
// і декодер отримує P-кадр, що доводить текст до різкості.
//
// Тут лише чиста логіка (таймер бездіяльності, лічильник, бюджет, переривання
// рухом), без cgo і без build-тегів — щоб тестувалось на Linux. Склейка з
// енкодером живе в agent/cmd/oo-agent/main.go.
package refine

import "time"

// Config — параметри refine. Нульові поля замінюються дефолтами в New.
type Config struct {
	// Idle — скільки екран мусить стояти після останнього кадру з рухом,
	// перш ніж піде перший refine (ТЗ: 150–300 мс).
	Idle time.Duration
	// QPs — QP для кожного refine-кадру по черзі; довжина = кількість кадрів.
	QPs []int
	// MinGap — мінімальна пауза між refine-кадрами (не частіше за кадровий
	// інтервал).
	MinGap time.Duration
}

// DefaultIdle — 200 мс простою до першого refine.
const DefaultIdle = 200 * time.Millisecond

// DefaultQPs — два refine-кадри: QP 22, потім 18.
var DefaultQPs = []int{22, 18}

// State — автомат refine. Не потокобезпечний: живе в кадровому циклі.
//
//	idle --Motion--> armed --(Idle минув, Due)--> Sent x len(QPs) --> idle
//	  Motion у будь-якому стані перериває refine і заводить таймер заново.
type State struct {
	cfg    Config
	armed  bool      // після руху ще лишились refine-кадри
	done   int       // скільки refine-кадрів уже пішло після останнього руху
	nextAt time.Time // не раніше цього моменту — наступний refine
	dirty  bool      // енкодер зараз у refine-налаштуваннях, їх треба зняти
}

// New будує автомат; порожні поля Config беруть дефолти.
func New(cfg Config) *State {
	if cfg.Idle <= 0 {
		cfg.Idle = DefaultIdle
	}
	if len(cfg.QPs) == 0 {
		cfg.QPs = DefaultQPs
	}
	return &State{cfg: cfg}
}

// Motion — прийшов НОВИЙ кадр (рух / dirty rects). Перериває refine, що
// триває, і заводить таймер простою заново.
func (s *State) Motion(now time.Time) {
	s.armed = true
	s.done = 0
	s.nextAt = now.Add(s.cfg.Idle)
}

// Disarm — refine зараз не має сенсу (пауза гейта, втрачено кадр, зміна
// енкодера). Наступний рух заведе заново.
func (s *State) Disarm() { s.armed = false }

// Wait — скільки чекати новий кадр: def (keepalive-дедлайн), або менше, якщо
// раніше настає час refine. Ніколи не менше 1 мс.
func (s *State) Wait(now time.Time, def time.Duration) time.Duration {
	if !s.armed {
		return def
	}
	d := s.nextAt.Sub(now)
	if d < time.Millisecond {
		d = time.Millisecond
	}
	if d > def {
		return def
	}
	return d
}

// Due повертає QP refine-кадру, якщо саме час його кодувати.
func (s *State) Due(now time.Time) (qp int, ok bool) {
	if !s.armed || s.done >= len(s.cfg.QPs) || now.Before(s.nextAt) {
		return 0, false
	}
	return s.cfg.QPs[s.done], true
}

// Postpone відкладає refine (транспорт зайнятий) на d, не витрачаючи кадр.
func (s *State) Postpone(now time.Time, d time.Duration) {
	if s.armed {
		s.nextAt = now.Add(d)
	}
}

// Sent — refine-кадр закодовано й відправлено: bytes байтів при піковому
// бітрейті peakBps. Наступний refine не раніше, ніж канал на піку
// «проковтне» цей: бітрейт refine ніколи не перевищує піку.
func (s *State) Sent(now time.Time, bytes, peakBps int) {
	s.done++
	s.dirty = true
	gap := s.cfg.MinGap
	if peakBps > 0 {
		if b := time.Duration(int64(bytes) * 8 * int64(time.Second) / int64(peakBps)); b > gap {
			gap = b
		}
	}
	s.nextAt = now.Add(gap)
	if s.done >= len(s.cfg.QPs) {
		s.armed = false
	}
}

// NeedRestore — викликається перед звичайним (не refine) кадром. true:
// енкодер ще в refine-налаштуваннях, їх треба повернути. Скидає ознаку.
func (s *State) NeedRestore() bool {
	r := s.dirty
	s.dirty = false
	return r
}

// Complete — усі refine-кадри після останнього руху вже пішли: екран
// нерухомий і дошліфований (тригер текстових тайлів, internal/tiles).
func (s *State) Complete() bool { return s.done >= len(s.cfg.QPs) }

// Refining — чи пішов уже хоч один refine після останнього руху.
func (s *State) Refining() bool { return s.done > 0 }
