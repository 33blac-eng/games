// Шар курсора на боці агента (-cursor-layer, ТИПОВО ВИМКНЕНО).
//
// F9, ПЕРЕГОВОРИ: прапорець лише дозволяє шар. Агент вмальовує вказівник у
// кадр, доки хаб не пришле по тому ж каналу cursorproto.KindMode=1 — а хаб
// шле його лише коли КОЖЕН глядач ноги відкрив 'oosc-cursor' (плеєр із
// config.cursorLayer) і нода не пишеться в MKV. Новий глядач без шару,
// увімкнений запис чи закритий канал — KindMode=0 / відкат: вказівник знову в
// кадрі, Publisher на паузі (останнім шле приховану позицію).
//
// Без прапорця нічого тут не працює: канал "oosc-cursor" не створюється, SDP
// той самий, вказівник і далі вмальовується в кадр (dxgi.c), як було.
//
// З прапорцем capture перестає вмальовувати вказівник (capture.SetCursorLayer),
// а рух миші без змін картинки стає NoChange-кадром — тобто НЕ кодується, не
// збиває refine і не коштує бітрейту. Сам вказівник їде окремо через
// cursorproto.Publisher: форма — на зміну (PNG, кеш за хешем), позиція —
// лише на зміну й не частіше 8 мс. Один надійний упорядкований канал —
// обґрунтування в internal/cursorproto.
//
// Позицію опитує ОКРЕМА горутина (runCursorPoller, ~120 Гц, GetCursorInfo):
// з кадрового циклу вона приходила б лише тоді, коли NextFrame повернувся, —
// тобто чекала б на кодування великого кадру. Капчер не потокобезпечний, тож
// горутина його не чіпає: лише Win32 GetCursorInfo і capture.OutputRect
// (геометрія виходу без стану капчера). Форма й далі з DXGI у кадровому
// циклі. Коли геометрії нема чи вона не збігається з кадром (повернутий
// монітор), опитувач віддає позицію назад кадровому циклу, як було.
package main

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/organicoils/oo-screen/agent/capture"
	"github.com/organicoils/oo-screen/internal/cursorproto"
	"github.com/pion/webrtc/v4"
)

// cursorLayerEnabled — прапорець -cursor-layer. Ставиться в main до dial.
var cursorLayerEnabled bool

var cursorPub = cursorproto.NewPublisher()

// cursorPollOwns — опитувач зараз сам шле позицію; кадровий цикл тоді лише
// форму.
var cursorPollOwns atomic.Bool

// frameGeom — що кадровий цикл бачить зараз: який вихід і якого розміру
// кадри. Опитувачу це потрібно, щоб перевести екранні координати в кадрові.
type frameGeom struct{ out, w, h int }

var cursorFrameGeom atomic.Pointer[frameGeom]

// cursorChan — поточний канал курсора (старий OnClose не чіпає новий).
var cursorChan atomic.Pointer[webrtc.DataChannel]

// applyCursorGrant — єдине місце, що вмикає/вимикає шар: і капчер (кадр із
// вказівником чи без), і Publisher (шле чи мовчить) перемикаються разом.
func applyCursorGrant(on bool) {
	capture.SetCursorLayer(on)
	cursorPub.SetActive(on)
}

func cursorChannelOpen(dc *webrtc.DataChannel) {
	cursorChan.Store(dc)
	applyCursorGrant(false) // новий хаб/нога: дозвіл треба отримати заново
	cursorPub.SetSink(dc)
}

func cursorChannelClosed(dc *webrtc.DataChannel) {
	if cursorChan.CompareAndSwap(dc, nil) {
		applyCursorGrant(false)
	}
	cursorPub.ClearSink(dc)
}

// cursorGrantMessage — хаб -> агент. Чуже/зіпсоване ігнорується (шар
// лишається, яким був; типово — вимкнено).
func cursorGrantMessage(dc *webrtc.DataChannel, b []byte) {
	on, ok := cursorproto.DecodeMode(b)
	if !ok || cursorChan.Load() != dc {
		return
	}
	log.Printf("oo-agent: cursor layer %v (хаб)", on)
	applyCursorGrant(on)
}

// observeCursor — з кадрового циклу на КОЖЕН кадр (NoChange теж).
func observeCursor(f *capture.NV12Frame, c *capture.Capturer, output int) {
	if !cursorLayerEnabled || f == nil || c == nil {
		return
	}
	if g := cursorFrameGeom.Load(); g == nil || g.out != output || g.w != f.Width || g.h != f.Height {
		cursorFrameGeom.Store(&frameGeom{output, f.Width, f.Height})
	}
	shape := func() (cursorproto.RawShape, bool) {
		raw, ok := c.CursorShape()
		if !ok {
			return cursorproto.RawShape{}, false
		}
		return cursorproto.RawShape{
			Type: int(raw.Type), W: raw.W, H: raw.H, Pitch: raw.Pitch,
			HotX: raw.HotX, HotY: raw.HotY, Data: raw.Data,
		}, true
	}
	if cursorPollOwns.Load() {
		cursorPub.ObserveShape(f.CursorShapeSeq, shape)
		return
	}
	cursorPub.Observe(cursorproto.Sample{
		Visible:  f.CursorVisible,
		X:        f.CursorX,
		Y:        f.CursorY,
		FrameW:   f.Width,
		FrameH:   f.Height,
		ShapeSeq: f.CursorShapeSeq,
	}, shape)
}

// runCursorPoller — замість cursorPub.Run: на кожному тіку опитує вказівник
// і (Poller.Step) або шле позицію, або лише дошле відкладену коалесценцією.
func runCursorPoller(stop <-chan struct{}) {
	t := time.NewTicker(cursorproto.PollInterval)
	defer t.Stop()
	poll := cursorproto.Poller{Pub: cursorPub}
	var rc outputRectCache
	for {
		select {
		case <-stop:
			cursorPollOwns.Store(false)
			return
		case now := <-t.C:
			r, rok := readCursorPos()
			g, gok := rc.geometry(now, cursorFrameGeom.Load(), capture.OutputRect)
			cursorPollOwns.Store(poll.Step(r, rok, g, gok))
		}
	}
}

// outputRectCache — DesktopCoordinates виходу, перечитуються на зміну
// виходу/розміру кадру і раз на секунду (перестановка моніторів у Windows).
type outputRectCache struct {
	fg   frameGeom
	at   time.Time
	ok   bool
	l, t int
	w, h int
}

const outputRectTTL = time.Second

func (c *outputRectCache) geometry(now time.Time, fg *frameGeom, rect func(int) (int, int, int, int, error)) (cursorproto.Geometry, bool) {
	if fg == nil {
		return cursorproto.Geometry{}, false
	}
	if *fg != c.fg || now.Sub(c.at) >= outputRectTTL {
		c.fg, c.at = *fg, now
		var err error
		c.l, c.t, c.w, c.h, err = rect(fg.out)
		c.ok = err == nil
	}
	if !c.ok {
		return cursorproto.Geometry{}, false
	}
	return cursorproto.Geometry{Left: c.l, Top: c.t, W: c.w, H: c.h, FrameW: fg.w, FrameH: fg.h}, true
}
