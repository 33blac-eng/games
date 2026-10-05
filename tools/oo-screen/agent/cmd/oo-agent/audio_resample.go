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
// частоту в 48 кГц float32 лінійною інтерполяцією зі станом між пакетами.
// ponytail: лінійна інтерполяція без фільтра — для мови/системних звуків
// досить; музику з високими частотами краще через полі-фазний фільтр.
type resampledCapturer struct {
	src  audioCapturer
	in   audio.Format
	out  audio.Format
	pos  float64   // дробова позиція наступного вихідного семпла у вхідних
	last []float64 // останній вхідний кадр попереднього пакета (по каналах)
	have bool
}

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
	return &resampledCapturer{src: src, in: f, out: out, last: make([]float64, f.Channels)}
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
func (r *resampledCapturer) convert(in []byte) ([]byte, int) {
	ch, bps := r.in.Channels, r.in.BitsPerSample/8
	step := r.in.BytesPerFrame
	if step <= 0 {
		step = ch * bps
	}
	frames := len(in) / step
	if frames == 0 || bps == 0 {
		return nil, 0
	}
	at := func(i, c int) float64 { // i == -1 — останній кадр попереднього пакета
		if i < 0 {
			return r.last[c]
		}
		return audio.Sample(in[i*step+c*bps:], r.in)
	}
	ratio := float64(r.in.SampleRate) / audioTargetRate
	start := -1.0
	if !r.have {
		start = 0
	}
	if r.pos < start {
		r.pos = start
	}
	out := make([]byte, 0, int(float64(frames)/ratio+2)*4*ch)
	n := 0
	for r.pos <= float64(frames-1) {
		i0 := int(math.Floor(r.pos))
		frac := r.pos - float64(i0)
		for c := 0; c < ch; c++ {
			a := at(i0, c)
			b := a
			if i0+1 <= frames-1 {
				b = at(i0+1, c)
			}
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(a+(b-a)*frac)))
		}
		n++
		r.pos += ratio
	}
	for c := 0; c < ch; c++ {
		r.last[c] = at(frames-1, c)
	}
	r.have = true
	r.pos -= float64(frames) // позиція відносно наступного пакета
	return out, n
}
