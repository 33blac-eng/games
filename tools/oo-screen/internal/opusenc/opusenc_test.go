package opusenc

import (
	"math"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]Codec{"": CodecOpus, "opus": CodecOpus, " OPUS ": CodecOpus, "pcmu": CodecPCMU, "g711": CodecPCMU} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q)=%v,%v want %v", in, got, err, want)
		}
	}
	if c, err := Parse("aac"); err == nil || c != CodecOpus {
		t.Errorf("Parse(aac)=%v,%v: want opus + error", c, err)
	}
}

func TestCapability(t *testing.T) {
	c := CodecOpus.Capability()
	if c.ClockRate != 48000 || c.Channels != 2 || !CodecOpus.Matches("audio/OPUS") {
		t.Fatalf("opus cap %+v", c)
	}
	if p := CodecPCMU.Parameters(); p.PayloadType != 0 || p.ClockRate != 8000 || p.Channels != 1 {
		t.Fatalf("pcmu params %+v", p)
	}
	if CodecOpus.Parameters().PayloadType != 111 {
		t.Fatal("opus PT")
	}
}

func TestPacketDuration(t *testing.T) {
	cases := []struct {
		p    []byte
		want time.Duration
	}{
		{[]byte{31 << 3}, 20 * time.Millisecond},      // CELT FB 20 мс
		{[]byte{28 << 3}, 2500 * time.Microsecond},    // CELT 2.5 мс
		{[]byte{1<<3 | 1}, 40 * time.Millisecond},     // SILK 20 мс x2
		{[]byte{3<<3 | 3, 3}, 180 * time.Millisecond}, // SILK 60 мс x3
		{[]byte{15 << 3}, 20 * time.Millisecond},      // Hybrid 20
		{nil, 0},
		{[]byte{3}, 0},
	}
	for _, c := range cases {
		if got := PacketDuration(c.p); got != c.want {
			t.Errorf("PacketDuration(%v)=%v want %v", c.p, got, c.want)
		}
	}
}

func TestOpusHead(t *testing.T) {
	h := OpusHead(312)
	if len(h) != 19 || string(h[:8]) != "OpusHead" || h[9] != 2 || int(h[10])|int(h[11])<<8 != 312 {
		t.Fatalf("OpusHead %v", h)
	}
	if r := int(h[12]) | int(h[13])<<8 | int(h[14])<<16 | int(h[15])<<24; r != 48000 {
		t.Fatalf("rate %d", r)
	}
}

// stereoTone — L і R різні частоти: перевіряє, що стерео не схлопується в моно.
func stereoTone(frame int, fl, fr float64) []float32 {
	pcm := make([]float32, FrameSamples*Channels)
	for i := 0; i < FrameSamples; i++ {
		n := float64(frame*FrameSamples + i)
		pcm[2*i] = float32(0.3 * math.Sin(2*math.Pi*fl*n/Rate))
		pcm[2*i+1] = float32(0.3 * math.Sin(2*math.Pi*fr*n/Rate))
	}
	return pcm
}

// goertzel — потужність частоти f у сигналі x.
func goertzel(x []float64, f float64) float64 {
	w := 2 * math.Pi * f / Rate
	c := 2 * math.Cos(w)
	var s1, s2 float64
	for _, v := range x {
		s1, s2 = v+c*s1-s2, s1
	}
	return s1*s1 + s2*s2 - c*s1*s2
}

// RoundTrip міряє те, чого PCMU не вміє фізично: 10 кГц у лівому і 1 кГц у
// правому каналі мусять дожити до декодера, кожна у своєму каналі.
func TestRoundTripStereoWideband(t *testing.T) {
	enc, err := NewEncoder(0)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	const frames = 50
	var l, r []float64
	bytes := 0
	out := make([]float32, 5760*2)
	for i := 0; i < frames; i++ {
		pkt, err := enc.Encode(stereoTone(i, 10000, 1000))
		if err != nil {
			t.Fatal(err)
		}
		if d := PacketDuration(pkt); d != FrameDuration {
			t.Fatalf("frame %d: duration %v", i, d)
		}
		bytes += len(pkt)
		n, err := dec.Decode(pkt, out)
		if err != nil || n != FrameSamples {
			t.Fatalf("decode n=%d err=%v", n, err)
		}
		if i >= 10 { // прогрів + lookahead
			for k := 0; k < n; k++ {
				l = append(l, float64(out[2*k]))
				r = append(r, float64(out[2*k+1]))
			}
		}
	}
	kbps := float64(bytes*8) / (frames * 0.02) / 1000
	lSep := 10 * math.Log10(goertzel(l, 10000)/goertzel(l, 1000))
	rSep := 10 * math.Log10(goertzel(r, 1000)/goertzel(r, 10000))
	t.Logf("bitrate=%.1f kbps, L 10k/1k=%.1f dB, R 1k/10k=%.1f dB", kbps, lSep, rSep)
	if lSep < 20 || rSep < 20 {
		t.Fatalf("канали змішались: L %.1f dB, R %.1f dB", lSep, rSep)
	}
	if kbps < 64 || kbps > 200 {
		t.Fatalf("bitrate %.1f kbps поза очікуваним", kbps)
	}
}

func TestEncodeRejectsWrongSize(t *testing.T) {
	enc, _ := NewEncoder(0)
	if _, err := enc.Encode(make([]float32, 100)); err == nil {
		t.Fatal("want error")
	}
}

// BenchmarkEncode20ms — ціна кодування одного кадру 20 мс стерео. Реальний
// час = 20 мс; ns/op / 20e6 — частка одного ядра.
func BenchmarkEncode20ms(b *testing.B) {
	enc, _ := NewEncoder(0)
	frames := make([][]float32, 50)
	for i := range frames {
		frames[i] = stereoTone(i, 440, 3000)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := enc.Encode(frames[i%50]); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/20e6*100, "%core")
}
