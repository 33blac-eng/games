package main

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/organicoils/oo-screen/agent/audio"
)

// audioTargetRate — частота, яку бачить решта звукового конвеєра. Opus 48 кГц,
// PCMU 8 кГц = 48000/6: обидва беруться за неї цілочисельним проріджуванням.
const audioTargetRate = 48000

// resampledCapturer — Windows 7: WASAPI там не має AUTOCONVERTPCM, тож
// loopback відкривається у ВЛАСНОМУ форматі пристрою (часто 44.1 кГц), а
// audioLayout дробову частоту не проріджує — звуку не було зовсім
// (Chief-Accountant_PC 06.10.2026, 0x88890008). Обгортка перераховує будь-яку
// частоту в 48 кГц float32 полі-фазним windowed-sinc фільтром (вікно Кайзера)
// з раціональним кроком L/M (44.1 -> 48 = 160/147) і станом між пакетами.
// Лінійна інтерполяція давала THD+N -20 дБ на 10 кГц і не мала антиаліасингу.
type resampledCapturer struct {
	src audioCapturer
	in  audio.Format
	out audio.Format

	l, m   int64     // вихідний кадр n відповідає вхідному часу n*m/l
	k      int       // відводів на фазу (парне)
	phases int       // кількість фаз у таблиці (== l, або 1024 для «дивних» частот)
	coef   []float64 // [phases][k], нормовані до одиничного підсилення на DC
	ip     int64     // floor(n*m/l) для наступного вихідного кадру (абсолютний)
	rem    int64     // (n*m) mod l
	base   int64     // абсолютний індекс вхідного кадру buf[0]
	buf    []float64 // історія входу, interleaved по каналах
}

const resampleMaxPhases = 1024

// wrapResample повертає src без змін, якщо частота й так кратна 48 кГц.
func wrapResample(src audioCapturer) audioCapturer {
	f := src.Format()
	if f.SampleRate <= 0 || f.Channels <= 0 || f.SampleRate%audioTargetRate == 0 {
		return src
	}
	out := audio.Format{
		SampleRate: audioTargetRate, Channels: f.Channels,
		SampleFormat: audio.SampleFormatFloat, BitsPerSample: 32, ValidBitsPerSample: 32,
		ChannelMask: f.ChannelMask, BytesPerFrame: 4 * f.Channels,
	}
	r := &resampledCapturer{src: src, in: f, out: out}
	r.design()
	return r
}

// design — таблиця коефіцієнтів. Зріз = половина меншої з частот, перехідна
// смуга 10% від неї (смуга пропускання до 0.45*min), загасання ~100 дБ.
// Для 44.1 -> 48 це 64 відводи на фазу, для 176.4 -> 48 — 212 (антиаліасинг).
func (r *resampledCapturer) design() {
	in := int64(r.in.SampleRate)
	g := gcd(in, audioTargetRate)
	r.l, r.m = audioTargetRate/g, in/g
	r.phases = int(r.l)
	if r.phases > resampleMaxPhases {
		r.phases = resampleMaxPhases // найближча фаза: похибка часу < 1/2048 кадру
	}
	minRate := math.Min(float64(in), audioTargetRate)
	fc := 0.5 * minRate / float64(in)               // зріз у частках вхідної частоти
	dw := 2 * math.Pi * 0.1 * minRate / float64(in) // ширина переходу, рад/кадр
	const atten = 100.0
	k := int(math.Ceil((atten-8)/(2.285*dw))) + 1
	k = (k + 3) &^ 3
	r.k = k
	beta := 0.1102 * (atten - 8.7)
	i0b := besselI0(beta)
	half := float64(k / 2)
	r.coef = make([]float64, r.phases*k)
	for p := 0; p < r.phases; p++ {
		frac := float64(p) / float64(r.phases)
		row := r.coef[p*k : (p+1)*k]
		sum := 0.0
		for j := range row {
			d := frac + half - 1 - float64(j) // відстань від точки виходу до вхідного кадру
			x := d / half
			w := 0.0
			if x > -1 && x < 1 {
				w = besselI0(beta*math.Sqrt(1-x*x)) / i0b
			}
			v := 2 * fc * w
			if a := 2 * math.Pi * fc * d; a != 0 {
				v *= math.Sin(a) / a
			}
			row[j] = v
			sum += v
		}
		for j := range row {
			row[j] /= sum
		}
	}
	// Історія затравлена нулями: вихід вирівняний у часі з входом (лінійна
	// фаза), ціною сталої затримки Latency() вхідних кадрів.
	r.base = -int64(k/2 - 1)
	r.buf = make([]float64, (k/2-1)*r.in.Channels)
}

// Latency — стала затримка фільтра у вхідних кадрах: після N вхідних кадрів
// видано рівно ceil((N-Latency)*48000/rate) вихідних.
func (r *resampledCapturer) Latency() int { return r.k / 2 }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// besselI0 — модифікована функція Бесселя I0 (ряд), для вікна Кайзера.
func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	for i := 1; i < 50; i++ {
		term *= (x / (2 * float64(i))) * (x / (2 * float64(i)))
		sum += term
		if term < sum*1e-17 {
			break
		}
	}
	return sum
}

func (r *resampledCapturer) Format() audio.Format { return r.out }
func (r *resampledCapturer) Close() error         { return r.src.Close() }

func (r *resampledCapturer) NextFrame(ctx context.Context) (*audio.Frame, error) {
	fr, err := r.src.NextFrame(ctx)
	if err != nil || fr == nil {
		return fr, err
	}
	data, n := r.convert(fr.Data)
	return &audio.Frame{Data: data, Frames: n, Format: r.out, Timestamp: fr.Timestamp, RMS: fr.RMS}, nil
}

// convert — один пакет вхідних кадрів у 48 кГц float32 (interleaved LE).
// Результат не залежить від нарізки входу на пакети.
func (r *resampledCapturer) convert(in []byte) ([]byte, int) {
	ch, bps := r.in.Channels, r.in.BitsPerSample/8
	step := r.in.BytesPerFrame
	if step <= 0 {
		step = ch * bps
	}
	if bps == 0 {
		return nil, 0
	}
	frames := len(in) / step
	if frames == 0 {
		return nil, 0
	}
	for i := 0; i < frames; i++ {
		for c := 0; c < ch; c++ {
			r.buf = append(r.buf, audio.Sample(in[i*step+c*bps:], r.in))
		}
	}
	avail := r.base + int64(len(r.buf)/ch) // абсолютний індекс за останнім кадром
	half := int64(r.k / 2)
	k := r.k
	est := int((avail-half-r.ip)*r.l/r.m) + 2
	if est < 0 {
		est = 0
	}
	out := make([]byte, 0, est*4*ch)
	n := 0
	for r.ip+half <= avail-1 {
		p := r.rem
		if int64(r.phases) != r.l {
			p = (r.rem*int64(r.phases) + r.l/2) / r.l
		}
		var row []float64
		start := int(r.ip-half+1-r.base) * ch
		if int(p) == r.phases { // округлення до наступного кадру
			row = r.coef[:k]
			start += ch
		} else {
			row = r.coef[int(p)*k : int(p+1)*k]
		}
		if start+k*ch > len(r.buf) {
			break // наступний кадр ще не прийшов — доробимо в наступному пакеті
		}
		win := r.buf[start : start+k*ch]
		if ch == 2 {
			var a0, a1 float64
			for j, w := range row {
				a0 += w * win[2*j]
				a1 += w * win[2*j+1]
			}
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(a0)))
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(a1)))
		} else {
			for c := 0; c < ch; c++ {
				acc := 0.0
				for j, w := range row {
					acc += w * win[j*ch+c]
				}
				out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(acc)))
			}
		}
		n++
		r.rem += r.m
		r.ip += r.rem / r.l
		r.rem %= r.l
	}
	// Відкидаємо кадри, що вже не знадобляться (запас +1 на «округлену» фазу).
	if drop := int(r.ip - half + 1 - r.base); drop > 0 {
		r.buf = append(r.buf[:0], r.buf[drop*ch:]...)
		r.base += int64(drop)
	}
	return out, n
}
