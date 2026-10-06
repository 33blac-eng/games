package refine

import "testing"

func TestLargeFrameMinQP(t *testing.T) {
	const px = 1920 * 1080
	cases := []struct {
		changed  float64
		bps, min int
		want     int
	}{
		{1.0, 2_000_000, 32, 32}, // повна зміна, вузький канал — правило діє
		{1.0, 8_000_000, 32, 0},  // бюджет достатній
		{0.3, 2_000_000, 32, 0},  // кадр не великий
		{1.0, 2_000_000, 0, 0},   // вимкнено
		{0.6, 1_000_000, 99, 51}, // межа H.264
		{1.0, 0, 32, 0},          // невідомий бітрейт
	}
	for _, c := range cases {
		if got := LargeFrameMinQP(c.changed, c.bps, px, c.min); got != c.want {
			t.Errorf("%+v: %d", c, got)
		}
	}
}
