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

// Aligned: ціль чекає природного IDR цільового щабля, без IDR-скидання.
func TestLadderAlignedSwitchesOnNaturalIDR(t *testing.T) {
	mk := func(tag byte) []h264.AU {
		return []h264.AU{{Data: []byte{tag, 0}, Keyframe: true}, {Data: []byte{tag, 1}}, {Data: []byte{tag, 2}}}
	}
	l := &ladder{levels: []ladderLevel{{500_000, mk('a')}, {8_000_000, mk('b')}}, cur: 1, want: 1}
	if err := l.setAligned(); err != nil {
		t.Fatal(err)
	}
	l.next() // b0
	l.onCtl([]byte(`{"v":1,"type":"bitrate_target","seq":1,"bitrate_bps":700000}`))
	for i, want := range []string{"b1", "b2", "a0", "a1"} {
		d := l.next()
		if got := string([]byte{d[0], '0' + d[1]}); got != want {
			t.Fatalf("кадр %d: %s, want %s", i, got, want)
		}
	}
	l.levels[1].aus = l.levels[1].aus[:2]
	if l.setAligned() == nil {
		t.Fatal("різна довжина щаблів мала бути помилкою")
	}
}
