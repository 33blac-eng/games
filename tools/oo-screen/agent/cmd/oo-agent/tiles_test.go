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
	st := sendEpisode(context.Background(), dc, img, 4, 1, 1<<30, nil, func(e uint32) bool {
		n++
		return n < 5 && e == 4
	})
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
	st := sendEpisode(context.Background(), dc, redTextImage(128, 64), 1, 1, 1<<30, nil, func(uint32) bool { return true })
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
	})
	if called {
		t.Fatal("readback with the flag off")
	}
}
