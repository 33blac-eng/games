package main

// cursorFan — N6: шар курсора їде і в канал хаба (oosc-cursor агента), і в
// канал прямої ноги (oosc-cursor браузера). Publisher знає одну Sink; fan —
// ця Sink, яка розсилає в усі відкриті канали. Новий канал = SetSink(fan)
// наново: Publisher тоді шле поточну форму й позицію ще раз (у старі канали
// це лише дубль останнього стану — він абсолютний, шкоди нема).

import (
	"sync"

	"github.com/organicoils/oo-screen/internal/cursorproto"
)

type cursorFan struct {
	pub   *cursorproto.Publisher
	mu    sync.Mutex
	sinks []cursorproto.Sink
	// gate — куди звітувати про прямі ноги та їхні канали (nil — нікуди:
	// тести fan-а без дозволу).
	gate *cursorGate
}

func newCursorFan(pub *cursorproto.Publisher) *cursorFan { return &cursorFan{pub: pub} }

// cursorSinks — fan агента над cursorPub; його прямі ноги беруть участь у
// дозволі шару (cursorGrantGate).
var cursorSinks = func() *cursorFan {
	f := newCursorFan(cursorPub)
	f.gate = cursorGrantGate
	return f
}()

// cursorGrantGate — єдиний дозвіл шару агента (див. cursorGate).
var cursorGrantGate = &cursorGate{apply: applyCursorGrant}

// cursorGate — F9 + N6: шар курсора вмикається, лише коли (1) хаб дозволив
// (KindMode=1: кожен relay-глядач має oosc-cursor, запису нема) І (2) КОЖНА
// жива пряма нога має відкритий oosc-cursor. Хаб прямих ніг не бачить: без
// (2) браузер прямої ноги без config.cursorLayer (той самий AU, що йде на
// хаб) отримував би кадри без вказівника, щойно relay-глядачі дозволили шар.
// Прямі ноги без relay-глядачів шар не вмикають (хаб без глядачів шле
// KindMode=0) — це свідомо: безпечний бік, вказівник у кадрі.
//
// Порядок застосувань серіалізовано mu (apply під mu): останнє, що отримали
// капчер і Publisher, завжди відповідає останньому стану.
type cursorGate struct {
	mu    sync.Mutex
	hub   bool
	legs  map[string]bool // id прямої ноги -> її oosc-cursor відкрито
	apply func(on bool)
}

// wantLocked — чи можна прибрати вказівник із кадру зараз.
func (g *cursorGate) wantLocked() bool {
	if !g.hub {
		return false
	}
	for _, open := range g.legs {
		if !open {
			return false
		}
	}
	return true
}

func (g *cursorGate) applyLocked() {
	if g.apply != nil {
		g.apply(g.wantLocked())
	}
}

// setHub — дозвіл/відкликання від хаба (і скидання на новий/закритий канал).
func (g *cursorGate) setHub(on bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hub = on
	g.applyLocked()
}

// legAdd — нова пряма нога (ДО answer: кадри їй ще не йдуть). Поки її
// oosc-cursor не відкрито, шар відкликано.
func (g *cursorGate) legAdd(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.legs == nil {
		g.legs = map[string]bool{}
	}
	if _, ok := g.legs[id]; !ok {
		g.legs[id] = false
	}
	g.applyLocked()
}

// legCursor — канал курсора прямої ноги відкрився/закрився. Невідома нога
// (уже знята) ігнорується: пізній OnClose не має «воскресити» її як ногу
// без шару і відкликати дозвіл назавжди.
func (g *cursorGate) legCursor(id string, open bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.legs[id]; !ok {
		return
	}
	g.legs[id] = open
	g.applyLocked()
}

// legDrop — пряму ногу знято.
func (g *cursorGate) legDrop(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.legs[id]; !ok {
		return
	}
	delete(g.legs, id)
	g.applyLocked()
}

func (f *cursorFan) legAdd(id string) {
	if f.gate != nil {
		f.gate.legAdd(id)
	}
}

func (f *cursorFan) legDrop(id string) {
	if f.gate != nil {
		f.gate.legDrop(id)
	}
}

func (f *cursorFan) add(s cursorproto.Sink) {
	f.mu.Lock()
	f.sinks = append(f.sinks, s)
	f.mu.Unlock()
	f.pub.SetSink(f)
}

func (f *cursorFan) remove(s cursorproto.Sink) {
	f.mu.Lock()
	for i, x := range f.sinks {
		if x == s {
			f.sinks = append(f.sinks[:i], f.sinks[i+1:]...)
			break
		}
	}
	empty := len(f.sinks) == 0
	f.mu.Unlock()
	if empty {
		f.pub.ClearSink(f)
	}
}

// dcSink — те, що треба від DataChannel (і для OnOpen/OnClose).
type dcSink interface {
	cursorproto.Sink
	OnOpen(func())
	OnClose(func())
}

// attach — підключити канал прямої ноги id, коли він відкриється, і
// відʼєднати при закритті; стан каналу — у дозвіл шару (gate).
func (f *cursorFan) attach(id string, dc dcSink) {
	dc.OnOpen(func() {
		f.add(dc)
		if f.gate != nil {
			f.gate.legCursor(id, true)
		}
	})
	dc.OnClose(func() {
		f.remove(dc)
		if f.gate != nil {
			f.gate.legCursor(id, false)
		}
	})
}

func (f *cursorFan) snapshot() []cursorproto.Sink {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cursorproto.Sink(nil), f.sinks...)
}

// Send — у всі канали; помилка лише коли не прийняв жоден.
func (f *cursorFan) Send(b []byte) error {
	var err error
	ok := false
	for _, s := range f.snapshot() {
		if e := s.Send(b); e != nil {
			err = e
		} else {
			ok = true
		}
	}
	if ok {
		return nil
	}
	return err
}

// BufferedAmount — найзабитіший канал (позицію притримуємо за ним).
func (f *cursorFan) BufferedAmount() uint64 {
	var m uint64
	for _, s := range f.snapshot() {
		if b := s.BufferedAmount(); b > m {
			m = b
		}
	}
	return m
}
