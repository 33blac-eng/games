package main

import (
	"testing"

	"github.com/organicoils/oo-screen/internal/h264"
)

func TestLadderSwitchesOnIDR(t *testing.T) {
	mk := func(tag byte) []h264.AU {
		return []h264.AU{{Data: []byte{tag, 0}}, {Data: []byte{tag, 1}}, {Data: []byte{tag, 2}}}
	}
	l := &ladder{levels: []ladderLevel{{500_000, mk('a')}, {8_000_000, mk('b')}}, cur: 1}
	l.next()
	l.next()
	l.onCtl([]byte(`{"v":1,"type":"bitrate_target","seq":1,"bitrate_bps":700000}`))
	if d := l.next(); d[0] != 'a' || d[1] != 0 {
		t.Fatalf("після цілі 700k чекали a/0, маємо %c/%d", d[0], d[1])
	}
	l.next()
	l.onCtl([]byte(`{"v":1,"type":"keyframe_request","seq":2}`))
	if d := l.next(); d[1] != 0 {
		t.Fatalf("keyframe_request мав почати з кадру 0, маємо %d", d[1])
	}
	if l.pick(100) != 0 || l.pick(9_000_000) != 1 {
		t.Fatal("pick")
	}
}
