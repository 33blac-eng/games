package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/contentmode"
	"github.com/organicoils/oo-screen/internal/control"
)

func TestVideoTickFPS(t *testing.T) {
	if videoTickFPS(false, 30, 60) != 30 || videoTickFPS(true, 30, 60) != 60 || videoTickFPS(true, 60, 30) != 60 {
		t.Fatal("tick fps")
	}
}

func TestVideoModeGap(t *testing.T) {
	hw := contentmode.FPSInput{BaseFPS: 30, VideoFPS: 60, Hardware: true, EncSec: 0.004}
	if g := videoModeGap(false, contentmode.Video, hw); g != 0 {
		t.Fatalf("disabled: %v", g)
	}
	if g := videoModeGap(true, contentmode.Normal, hw); g != slackGap(30) {
		t.Fatalf("normal: %v", g)
	}
	if g := videoModeGap(true, contentmode.Video, hw); g != slackGap(60) {
		t.Fatalf("video hw: %v", g)
	}
	sw := hw
	sw.Hardware = false
	if g := videoModeGap(true, contentmode.Video, sw); g != slackGap(30) {
		t.Fatalf("video sw must stay at base: %v", g)
	}
}

func TestEncEWMA(t *testing.T) {
	v := encEWMA(0, 10*time.Millisecond)
	if v != 0.01 {
		t.Fatalf("first sample: %v", v)
	}
	v = encEWMA(v, 20*time.Millisecond)
	if v < 0.0119 || v > 0.0121 {
		t.Fatalf("ewma: %v", v)
	}
	if encEWMA(0, -time.Second) != 0 {
		t.Fatal("negative")
	}
}

func TestVideoCtlDue(t *testing.T) {
	if videoCtlDue(false, true, contentmode.Video, time.Hour) {
		t.Fatal("disabled must never send")
	}
	if !videoCtlDue(true, true, contentmode.Normal, 0) {
		t.Fatal("flip must send")
	}
	if videoCtlDue(true, false, contentmode.Video, time.Second) || !videoCtlDue(true, false, contentmode.Video, videoCtlRepeat) {
		t.Fatal("repeat in video")
	}
	if videoCtlDue(true, false, contentmode.Normal, time.Hour) {
		t.Fatal("no repeat outside video")
	}
}

type fakeCtl struct{ got []control.Msg }

func (f *fakeCtl) sendCtl(m control.Msg) error { f.got = append(f.got, m); return nil }

func TestSendContentMode(t *testing.T) {
	f := &fakeCtl{}
	if err := sendContentMode(f, 7, contentmode.Video); err != nil || len(f.got) != 1 {
		t.Fatalf("send: %v %v", err, f.got)
	}
	b, _ := json.Marshal(f.got[0])
	if string(b) != `{"v":1,"type":"content_mode","seq":7,"mode":"video"}` {
		t.Fatalf("wire: %s", b)
	}
	if err := sendContentMode(struct{}{}, 1, contentmode.Video); !errors.Is(err, errNoCtl) {
		t.Fatalf("non-sender: %v", err)
	}
	if err := (&webrtcTransport{}).sendCtl(control.ContentMode(1, "video")); !errors.Is(err, errNoCtl) {
		t.Fatalf("nil dc: %v", err)
	}
}

func slackGap(fps int) time.Duration {
	k := videoGapSlack
	return time.Duration(float64(time.Second/time.Duration(fps)) * k)
}
