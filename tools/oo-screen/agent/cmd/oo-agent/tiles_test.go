package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/tiles"
	"github.com/pion/webrtc/v4"
)

type fakeTileDC struct {
	sent  [][]byte
	state webrtc.DataChannelState
	fail  bool
}

func (f *fakeTileDC) Send(b []byte) error {
	if f.fail {
		return errors.New("closed")
	}
	f.sent = append(f.sent, b)
	return nil
}
func (f *fakeTileDC) BufferedAmount() uint64              { return 0 }
func (f *fakeTileDC) ReadyState() webrtc.DataChannelState { return f.state }

// redTextImage — BGRA image with coloured "text" in every tile.
func redTextImage(w, h int) tiles.Image {
	img := tiles.Image{Pix: make([]byte, w*h*4), Stride: w * 4, W: w, H: h}
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if y%12 < 7 && (x/3)%2 == 0 && (y%12 == 0 || y%12 == 6 || x%5 == 0) {
				i := y*img.Stride + x*4
				img.Pix[i], img.Pix[i+1], img.Pix[i+2] = 0, 0, 220
			}
		}
	}
	return img
}

func TestSendEpisodeStopsOnStaleEpoch(t *testing.T) {
	dc := &fakeTileDC{state: webrtc.DataChannelStateOpen}
	img := redTextImage(256, 128)
	n := 0
	st := sendEpisode(context.Background(), dc, img, 4, 1, tiles.SelectConfig{}, 1<<30, nil, func(e uint32) bool {
		n++
		return n < 5 && e == 4
	}, nil)
	if !st.Aborted || len(dc.sent) == 0 || len(dc.sent) >= st.Selected {
		t.Fatalf("stats %+v sent %d", st, len(dc.sent))
	}
	for _, m := range dc.sent {
		if d, err := tiles.Decode(m); err != nil || d.Epoch != 4 {
			t.Fatalf("bad msg %v %+v", err, d)
		}
	}
}

func TestSendEpisodeClosedChannel(t *testing.T) {
	dc := &fakeTileDC{state: webrtc.DataChannelStateClosed}
	st := sendEpisode(context.Background(), dc, redTextImage(128, 64), 1, 1, tiles.SelectConfig{}, 1<<30, nil, func(uint32) bool { return true }, nil)
	if st.Sent != 0 || !st.Aborted {
		t.Fatalf("sent on a closed channel: %+v", st)
	}
}

func TestTilesFlagOffIsInert(t *testing.T) {
	prev := textTilesEnabled
	textTilesEnabled = false
	defer func() { textTilesEnabled = prev }()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if err := addTilesChannel(pc); err != nil {
		t.Fatal(err)
	}
	called := false
	tilesStatic(context.Background(), time.Now(), func() ([]byte, int, int, error) {
		called = true
		return nil, 0, 0, nil
	}, nil)
	tilesStill() // без прапорця й каналу — нічого, без паніки
	if called {
		t.Fatal("readback with the flag off")
	}
}

// Софт-шлях (refine вимкнено) отримує тайли за власним таймером простою;
// апаратний — лише після завершення refine; без жодного кадру — ніколи.
func TestTilesReadyTrigger(t *testing.T) {
	prevFlag, prevMotion := textTilesEnabled, tilesLastMotion
	defer func() { textTilesEnabled, tilesLastMotion = prevFlag, prevMotion }()
	textTilesEnabled = true
	tilesLastMotion = time.Time{}
	now := time.Now()
	if tilesReady(now, false, true) || tilesReady(now, true, true) {
		t.Fatal("ready before any frame")
	}
	// перший кадр (зокрема GDI на нерухомому екрані) заводить таймер
	tilesMotion()
	t0 := tilesLastMotion
	if t0.IsZero() {
		t.Fatal("tilesMotion did not arm the idle timer")
	}
	if tilesReady(t0.Add(tilesIdleAfter-time.Millisecond), false, false) {
		t.Fatal("software path fired before idle")
	}
	if !tilesReady(t0.Add(tilesIdleAfter), false, false) {
		t.Fatal("software path never fires (no refine)")
	}
	if tilesReady(t0.Add(time.Hour), true, false) {
		t.Fatal("hardware path fired before refine completed")
	}
	if !tilesReady(t0, true, true) {
		t.Fatal("hardware path ignores refine completion")
	}
	textTilesEnabled = false
	if tilesReady(t0.Add(time.Hour), false, true) {
		t.Fatal("ready with the flag off")
	}
}

func TestTilesCursorExclude(t *testing.T) {
	if tilesCursorExclude(false, 10, 10) != nil {
		t.Fatal("hidden pointer excluded something")
	}
	ex := tilesCursorExclude(true, 150, 10)
	dc := &fakeTileDC{state: webrtc.DataChannelStateOpen}
	img := redTextImage(256, 128)
	sendEpisode(context.Background(), dc, img, 1, 1, tiles.SelectConfig{Exclude: ex}, 1<<30, nil,
		func(uint32) bool { return true }, nil)
	if len(dc.sent) == 0 {
		t.Fatal("nothing sent")
	}
	r := ex[0]
	for _, b := range dc.sent {
		m, _ := tiles.Decode(b)
		x, y, w, h := int(m.X), int(m.Y), int(m.W), int(m.H)
		if x < r.X+r.W && r.X < x+w && y < r.Y+r.H && r.Y < y+h {
			t.Fatalf("tile %d,%d under the pointer sent", x, y)
		}
	}
}

// Друга серія на тому самому екрані: лише TypeKeep, жодного PNG; змінений
// тайл — одним TypeTile; новий канал (Reset) — знову повний набір.
func TestSendEpisodeDedup(t *testing.T) {
	held := &tiles.Held{}
	img := redTextImage(256, 128)
	run := func(epoch uint32) (keep int, tilesN int) {
		dc := &fakeTileDC{state: webrtc.DataChannelStateOpen}
		sendEpisode(context.Background(), dc, img, epoch, 1, tiles.SelectConfig{}, 1<<30, nil,
			func(uint32) bool { return true }, held)
		for i, b := range dc.sent {
			m, err := tiles.Decode(b)
			if err != nil || m.Epoch != epoch {
				t.Fatalf("bad msg %v %+v", err, m)
			}
			if m.Type == tiles.TypeKeep {
				if i != 0 {
					t.Fatal("keep not first")
				}
				keep = len(tiles.KeepRects(m.Payload))
			} else {
				tilesN++
			}
		}
		return
	}
	k, n := run(1)
	if k != 0 || n == 0 {
		t.Fatalf("first: keep %d tiles %d", k, n)
	}
	k2, n2 := run(2)
	if k2 != n || n2 != 0 {
		t.Fatalf("second: keep %d tiles %d (want %d/0)", k2, n2, n)
	}
	img.Pix[(10*img.Stride)+10*4] ^= 0xFF // touch tile (0,0)
	k3, n3 := run(3)
	if n3 != 1 || k3 != n-1 {
		t.Fatalf("changed: keep %d tiles %d", k3, n3)
	}
	held.Reset()
	if k4, n4 := run(4); k4 != 0 || n4 != n {
		t.Fatalf("after reset: keep %d tiles %d", k4, n4)
	}
}
