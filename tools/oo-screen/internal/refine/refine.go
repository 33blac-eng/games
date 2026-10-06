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

	// AfterKeyframe — ключовий кадр від rate control (періодичний GOP
	// енкодера, keyframe_request нового глядача, PLI) на НЕРУХОМОМУ екрані
	// перезаписує вже дошліфоване зображення якістю rate control, а руху, що
	// завів би refine, нема — текст лишався мильним до наступної зміни. З
	// прапорцем такий IDR заводить refine заново (TASK.md крок 4).
	AfterKeyframe bool
	// QPAware — пропускати кроки refine, що не кращі за найгірший QP, який
	// зараз лишився на екрані (Coded стежить за ним за QP кадрів з потоку).
	// Refine QP 22 поверх кадру з QP 16 не покращує нічого, лише коштує біт.
	// Невідомий QP (0) — кроки не пропускаються, як без прапорця.
	QPAware bool
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
	// worst — найгірший QP, що лишився на екрані після останнього IDR /
	// refine (0 — невідомо). Лише для QPAware.
	worst int
}

// Frame — закодований кадр, як його бачить refine. Апаратний MFT конвеєрний:
// AU повертається пізніше за свій кадр, тож агент зіставляє його з видом
// кадру за PTS (agent/cmd/oo-agent, auKinds).
type Frame struct {
	Key    bool // AU — IDR
	Refine int  // >0: refine-кадр, закодований із цим QP
	Motion bool // новий вміст; false і Refine==0 — keepalive-повтор без змін
	QP     int  // QP кадру з потоку (internal/h264.QPReader); 0 — невідомий
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

// Due повертає QP refine-кадру, якщо саме час його кодувати. З QPAware
// кроки, що не кращі за найгірший QP на екрані, тут же пропускаються (без
// кадру); якщо пропущено всі — refine завершено (Complete).
func (s *State) Due(now time.Time) (qp int, ok bool) {
	for s.armed && s.done < len(s.cfg.QPs) && !now.Before(s.nextAt) {
		q := s.cfg.QPs[s.done]
		if s.cfg.QPAware && s.worst > 0 && q >= s.worst {
			s.done++
			continue
		}
		return q, true
	}
	if s.done >= len(s.cfg.QPs) {
		s.armed = false
	}
	return 0, false
}

// Coded — енкодер віддав AU кадру f. Оновлює найгірший QP на екрані і, з
// AfterKeyframe, заводить refine після IDR від rate control.
func (s *State) Coded(now time.Time, f Frame) {
	switch {
	case f.Key:
		// IDR перезаписує весь екран: найгірший QP — його власний.
		s.worst = f.QP
		if f.Refine > 0 && f.QP == 0 {
			s.worst = f.Refine
		}
		if s.cfg.AfterKeyframe && f.Refine == 0 {
			s.armed, s.done = true, 0
			s.nextAt = now.Add(s.cfg.Idle)
		}
	case f.Refine > 0:
		q := f.Refine
		if f.QP > 0 {
			q = f.QP // MFT міг проігнорувати QP семпла — віримо потоку
		}
		if s.worst == 0 || q < s.worst {
			s.worst = q
		}
	case f.Motion:
		// Нові ділянки не кращі за свій QP; невідомий QP — невідомий і екран.
		if f.QP == 0 {
			s.worst = 0
		} else if s.worst > 0 && f.QP > s.worst {
			s.worst = f.QP
		}
	}
}

// WorstQP — найгірший QP на екрані за оцінкою Coded (0 — невідомо).
func (s *State) WorstQP() int { return s.worst }

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
