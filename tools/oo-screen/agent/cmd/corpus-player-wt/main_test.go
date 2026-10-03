package main

import (
	"testing"

	"github.com/organicoils/oo-screen/internal/h264"
)

func TestIdrIndicesAndNextIDR(t *testing.T) {
	// Синтетичний корпус: IDR кожні 120 AU (як реальний бенч-корпус),
	// 3 GOP-и по 120 кадрів = 360 AU.
	const gop = 120
	const nGop = 3
	aus := make([]h264.AU, gop*nGop)
	for i := range aus {
		aus[i] = h264.AU{Keyframe: i%gop == 0}
	}

	idr := idrIndices(aus)
	wantIDR := []int{0, 120, 240}
	if len(idr) != len(wantIDR) {
		t.Fatalf("idrIndices: got %v, want %v", idr, wantIDR)
	}
	for i, v := range wantIDR {
		if idr[i] != v {
			t.Fatalf("idrIndices[%d] = %d, want %d (full: %v)", i, idr[i], v, idr)
		}
	}

	cases := []struct {
		cur  int
		want int
	}{
		{0, 120},   // на самому IDR -> наступний, а не поточний
		{1, 120},   // одразу після IDR
		{119, 120}, // прямо перед наступним IDR
		{120, 240},
		{239, 240},
		{240, 0}, // останній GOP -> циклічний врап на перший IDR корпусу
		{359, 0},
	}
	for _, c := range cases {
		got := nextIDR(idr, c.cur)
		if got != c.want {
			t.Errorf("nextIDR(idr, %d) = %d, want %d", c.cur, got, c.want)
		}
	}
}

// TestKeyframeRequestChannelJump відтворює сценарій "hub -> player" з
// GATE-вимоги: сигнал keyframe_request з control-читача (симульований тут
// прямим записом у канал, як робить справжня control-горутина) мусить
// призвести до того, що основний цикл програвання перестрибне індекс i на
// найближчий наступний IDR — рівно та логіка, що в main() виводить лог
// "keyframe_request -> jump to AU N".
func TestKeyframeRequestChannelJump(t *testing.T) {
	const gop = 120
	aus := make([]h264.AU, gop*2)
	for i := range aus {
		aus[i] = h264.AU{Keyframe: i%gop == 0}
	}
	idr := idrIndices(aus)

	keyframeReqCh := make(chan struct{}, 1)
	i := 42 // десь у середині першого GOP

	keyframeReqCh <- struct{}{} // те саме неблокуюче відправлення, що робить control-горутина

	select {
	case <-keyframeReqCh:
		target := nextIDR(idr, i%len(aus))
		i = target
	default:
		t.Fatal("expected pending keyframe request in channel")
	}

	if i != 120 {
		t.Fatalf("expected jump to AU 120, got %d", i)
	}
}
