// Шар курсора на боці агента (-cursor-layer, ТИПОВО ВИМКНЕНО).
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
package main

import (
	"github.com/organicoils/oo-screen/agent/capture"
	"github.com/organicoils/oo-screen/internal/cursorproto"
)

// cursorLayerEnabled — прапорець -cursor-layer. Ставиться в main до dial.
var cursorLayerEnabled bool

var cursorPub = cursorproto.NewPublisher()

// observeCursor — з кадрового циклу на КОЖЕН кадр (NoChange теж).
func observeCursor(f *capture.NV12Frame, c *capture.Capturer) {
	if !cursorLayerEnabled || f == nil || c == nil {
		return
	}
	cursorPub.Observe(cursorproto.Sample{
		Visible:  f.CursorVisible,
		X:        f.CursorX,
		Y:        f.CursorY,
		FrameW:   f.Width,
		FrameH:   f.Height,
		ShapeSeq: f.CursorShapeSeq,
	}, func() (cursorproto.RawShape, bool) {
		raw, ok := c.CursorShape()
		if !ok {
			return cursorproto.RawShape{}, false
		}
		return cursorproto.RawShape{
			Type: int(raw.Type), W: raw.W, H: raw.H, Pitch: raw.Pitch,
			HotX: raw.HotX, HotY: raw.HotY, Data: raw.Data,
		}, true
	})
}
