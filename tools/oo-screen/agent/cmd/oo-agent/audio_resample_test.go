package main

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/organicoils/oo-screen/agent/audio"
)

type fakeCap struct {
	f      audio.Format
	frames [][]byte
}

func (f *fakeCap) Format() audio.Format { return f.f }
func (f *fakeCap) Close() error         { return nil }
func (f *fakeCap) NextFrame(context.Context) (*audio.Frame, error) {
	d := f.frames[0]
	f.frames = f.frames[1:]
	return &audio.Frame{Data: d, Format: f.f}, nil
}

// 44.1 кГц (Windows 7 без AUTOCONVERTPCM) -> 48 кГц: кількість кадрів і форма
// синуса мусять зберегтись, інакше Opus отримає прискорений/рваний звук.
func TestResample441To48(t *testing.T) {
	in := audio.Format{SampleRate: 44100, Channels: 2, SampleFormat: audio.SampleFormatPCM,
		BitsPerSample: 16, ValidBitsPerSample: 16, BytesPerFrame: 4}
	const tone = 440.0
	var pkts [][]byte
	k := 0
	for p := 0; p < 100; p++ { // 100 пакетів по 441 кадр = 1 с
		b := make([]byte, 0, 441*4)
		for i := 0; i < 441; i++ {
			v := int16(10000 * math.Sin(2*math.Pi*tone*float64(k)/44100))
			b = binary.LittleEndian.AppendUint16(b, uint16(v))
			b = binary.LittleEndian.AppendUint16(b, uint16(v))
			k++
		}
		pkts = append(pkts, b)
	}
	src := &fakeCap{f: in, frames: pkts}
	w := wrapResample(src)
	if w.Format().SampleRate != 48000 || w.Format().SampleFormat != audio.SampleFormatFloat {
		t.Fatalf("формат після обгортки %+v", w.Format())
	}
	if _, _, ok := audioLayout(w.Format(), 48000); !ok {
		t.Fatal("audioLayout не бере результат обгортки")
	}
	total, maxErr := 0, 0.0
	for p := 0; p < 100; p++ {
		fr, _ := w.NextFrame(context.Background())
		for i := 0; i < fr.Frames; i++ {
			got := float64(math.Float32frombits(binary.LittleEndian.Uint32(fr.Data[i*8:])))
			want := 10000.0 / 32768 * math.Sin(2*math.Pi*tone*float64(total)/48000)
			maxErr = math.Max(maxErr, math.Abs(got-want))
			total++
		}
	}
	// Фільтр вирівняний у часі й тримає сталу затримку Latency() вхідних
	// кадрів, тож за першу секунду виходить на стільки ж менше кадрів.
	lat := (w.(*resampledCapturer).Latency()*48000 + 44099) / 44100
	if total < 47990-lat || total > 48010 {
		t.Fatalf("1 с на 44.1 кГц дала %d кадрів на 48 кГц, чекали ~48000", total)
	}
	if maxErr > 0.01 {
		t.Fatalf("синус спотворено: макс. похибка %.4f", maxErr)
	}
	// Частота, кратна 48 кГц, лишається без обгортки.
	if wrapResample(&fakeCap{f: audio.Format{SampleRate: 48000, Channels: 2}}).Format().SampleRate != 48000 {
		t.Fatal("48 кГц не мусить обгортатись")
	}
}

// floatPkts — моно/стерео float32 сигнал gen(k) нарізаний на пакети розмірів sizes (по колу).
func floatPkts(ch, total int, sizes []int, gen func(k int) float64) [][]byte {
	var pkts [][]byte
	k, si := 0, 0
	for k < total {
		n := sizes[si%len(sizes)]
		si++
		if k+n > total {
			n = total - k
		}
		b := make([]byte, 0, n*4*ch)
		for i := 0; i < n; i++ {
			v := float32(gen(k))
			for c := 0; c < ch; c++ {
				b = binary.LittleEndian.AppendUint32(b, math.Float32bits(v))
			}
			k++
		}
		pkts = append(pkts, b)
	}
	return pkts
}

func floatFmt(rate, ch int) audio.Format {
	return audio.Format{SampleRate: rate, Channels: ch, SampleFormat: audio.SampleFormatFloat,
		BitsPerSample: 32, ValidBitsPerSample: 32, BytesPerFrame: 4 * ch}
}

// runResample — проганяє всі пакети, повертає сирі байти виходу й перший канал.
func runResample(t testing.TB, f audio.Format, pkts [][]byte) ([]byte, []float64) {
	w := wrapResample(&fakeCap{f: f, frames: pkts})
	var raw []byte
	var y []float64
	n := len(pkts)
	for p := 0; p < n; p++ {
		fr, _ := w.NextFrame(context.Background())
		raw = append(raw, fr.Data...)
		for i := 0; i < fr.Frames; i++ {
			y = append(y, float64(math.Float32frombits(binary.LittleEndian.Uint32(fr.Data[i*4*f.Channels:]))))
		}
	}
	return raw, y
}

// thdN — THD+N у дБ: МНК-підгонка синуса відомої частоти, решта = шум+спотворення.
func thdN(y []float64, freq, rate float64) float64 {
	var ss, sc, cc, sy, cy float64
	for n, v := range y {
		s, c := math.Sin(2*math.Pi*freq*float64(n)/rate), math.Cos(2*math.Pi*freq*float64(n)/rate)
		ss += s * s
		sc += s * c
		cc += c * c
		sy += s * v
		cy += c * v
	}
	det := ss*cc - sc*sc
	a, b := (sy*cc-cy*sc)/det, (cy*ss-sy*sc)/det
	var sig, res float64
	for n, v := range y {
		f := a*math.Sin(2*math.Pi*freq*float64(n)/rate) + b*math.Cos(2*math.Pi*freq*float64(n)/rate)
		sig += f * f
		res += (v - f) * (v - f)
	}
	return 10 * math.Log10(res/sig)
}

// THD+N 44.1 -> 48 кГц: лінійна інтерполяція давала -62 дБ (1 кГц) і -20 дБ
// (10 кГц) — дзеркальні частоти; полі-фазний sinc мусить бути < -80 дБ.
func TestResampleTHDN(t *testing.T) {
	for _, freq := range []float64{1000, 10000} {
		pkts := floatPkts(2, 44100, []int{441}, func(k int) float64 {
			return 0.5 * math.Sin(2*math.Pi*freq*float64(k)/44100)
		})
		_, y := runResample(t, floatFmt(44100, 2), pkts)
		d := thdN(y[2000:len(y)-2000], freq, 48000)
		t.Logf("%.0f Гц: THD+N %.1f дБ", freq, d)
		if d > -80 {
			t.Errorf("%.0f Гц: THD+N %.1f дБ, чекали < -80", freq, d)
		}
	}
}

// 176.4 -> 48 кГц: тон 30 кГц вище нової Найквіста мусить бути придушений > 60 дБ,
// а не завернутий у 18 кГц.
func TestResampleAliasRejection(t *testing.T) {
	rms := func(freq float64) float64 {
		pkts := floatPkts(2, 176400, []int{1764}, func(k int) float64 {
			return 0.5 * math.Sin(2*math.Pi*freq*float64(k)/176400)
		})
		_, y := runResample(t, floatFmt(176400, 2), pkts)
		var s float64
		y = y[2000 : len(y)-2000]
		for _, v := range y {
			s += v * v
		}
		return math.Sqrt(s / float64(len(y)))
	}
	att := 20 * math.Log10(rms(1000)/rms(30000))
	t.Logf("придушення 30 кГц: %.1f дБ", att)
	if att < 60 {
		t.Fatalf("придушення 30 кГц лише %.1f дБ, чекали > 60", att)
	}
}

// Вихід не мусить залежати від нарізки на пакети (включно з 1-кадровими).
func TestResamplePacketInvariance(t *testing.T) {
	gen := func(k int) float64 { return 0.3*math.Sin(float64(k)*0.05) + 0.2*math.Sin(float64(k)*1.3) }
	for _, rate := range []int{44100, 176400, 8000} {
		ref, _ := runResample(t, floatFmt(rate, 2), floatPkts(2, 5000, []int{441}, gen))
		for _, sizes := range [][]int{{1}, {7}, {1, 3, 1000, 2, 17}, {5000}} {
			got, _ := runResample(t, floatFmt(rate, 2), floatPkts(2, 5000, sizes, gen))
			if string(got) != string(ref) {
				t.Fatalf("%d Гц, пакети %v: вихід (%d Б) відрізняється від еталону (%d Б)", rate, sizes, len(got), len(ref))
			}
		}
	}
}

// Кількість вихідних кадрів точна: після N вхідних рівно
// ceil((N-затримка)*48000/rate), без дрейфу на 10 с.
func TestResampleFrameCountExact(t *testing.T) {
	for _, rate := range []int{8000, 22050, 44100, 88200, 176400, 11025, 32000} {
		w := wrapResample(&fakeCap{f: floatFmt(rate, 1)}).(*resampledCapturer)
		lat := w.Latency()
		in, out := 0, 0
		sizes := []int{1, 480, 7, 1023, 33}
		for i := 0; in < 10*rate; i++ {
			n := sizes[i%len(sizes)]
			_, got := w.convert(make([]byte, 4*n))
			in += n
			out += got
			want := 0
			if in > lat {
				want = int((int64(in-lat)*audioTargetRate + int64(rate) - 1) / int64(rate))
			}
			if out != want {
				t.Fatalf("%d Гц: після %d вхідних %d вихідних, чекали %d", rate, in, out, want)
			}
		}
	}
}

func BenchmarkResample441To48(b *testing.B) {
	in := audio.Format{SampleRate: 44100, Channels: 2, SampleFormat: audio.SampleFormatPCM,
		BitsPerSample: 16, ValidBitsPerSample: 16, BytesPerFrame: 4}
	pkt := make([]byte, 441*4)
	for i := 0; i < 441; i++ {
		v := uint16(int16(10000 * math.Sin(float64(i)*0.1)))
		binary.LittleEndian.PutUint16(pkt[i*4:], v)
		binary.LittleEndian.PutUint16(pkt[i*4+2:], v)
	}
	w := wrapResample(&fakeCap{f: in}).(*resampledCapturer)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.convert(pkt)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/480, "ns/outframe")
}
