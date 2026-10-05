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
}

func newCursorFan(pub *cursorproto.Publisher) *cursorFan { return &cursorFan{pub: pub} }

// cursorSinks — fan агента над cursorPub.
var cursorSinks = newCursorFan(cursorPub)

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

// attach — підключити канал, коли він відкриється, і відʼєднати при закритті.
func (f *cursorFan) attach(dc dcSink) {
	dc.OnOpen(func() { f.add(dc) })
	dc.OnClose(func() { f.remove(dc) })
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
