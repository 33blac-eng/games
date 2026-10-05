// F1: Opus 48 кГц стерео на агентській стороні. Енкодер — internal/opusenc
// (чистий Go). Той самий контракт, що в μ-law-енкодера (audio.go): пакет
// WASAPI довільної довжини на вході, рівні кадри 20 мс на виході, власний
// годинник доріжки в семплах (тут — 48 кГц) для audioGap.
package main

import (
	"log"

	"github.com/organicoils/oo-screen/agent/audio"
	"github.com/organicoils/oo-screen/internal/opusenc"
	"github.com/organicoils/oo-screen/internal/pcmu"
)

// frameEncoder — те, що audioCapture просить у кодека.
type frameEncoder interface {
	encode(data []byte, f audio.Format) [][]byte
	silence(n int64) [][]byte
	clock() int64 // семплів (на канал), уже відданих у доріжку
	rate() int    // частота годинника доріжки
}

func newFrameEncoder(c opusenc.Codec) (frameEncoder, error) {
	if c == opusenc.CodecPCMU {
		return &audioEncoder{}, nil
	}
	enc, err := opusenc.NewEncoder(0)
	if err != nil {
		return nil, err
	}
	return &opusAudioEncoder{enc: enc}, nil
}

func audioCodecLabel() string {
	if audioCodec == opusenc.CodecPCMU {
		return "PCMU 8кГц моно"
	}
	return "Opus 48кГц стерео"
}

// Методи інтерфейсу для μ-law-енкодера (audio.go).
func (e *audioEncoder) clock() int64 { return e.emitted }
func (e *audioEncoder) rate() int    { return pcmu.Rate }

// opusAudioEncoder — 48 кГц × N каналів -> 48 кГц стерео -> Opus 20 мс.
type opusAudioEncoder struct {
	enc     *opusenc.Encoder
	accL    float64
	accR    float64
	n       int
	pcm     []float32 // недобраний кадр, interleaved L,R
	emitted int64
	errs    int
}

func (e *opusAudioEncoder) clock() int64 { return e.emitted }
func (e *opusAudioEncoder) rate() int    { return opusenc.Rate }

// push — один стерео-семпл вихідної частоти. factor>1 лише для 96/192 кГц
// (box-децимація, той самий ponytail, що в μ-law: на 48 кГц factor=1 і
// фільтра немає взагалі — найчастіший mix format іде без жодних втрат).
func (e *opusAudioEncoder) push(l, r float64, factor int) []byte {
	e.accL += l
	e.accR += r
	e.n++
	if e.n < factor {
		return nil
	}
	l, r = clamp1(e.accL/float64(factor)), clamp1(e.accR/float64(factor))
	e.accL, e.accR, e.n = 0, 0, 0
	e.pcm = append(e.pcm, float32(l), float32(r))
	return e.take()
}

func clamp1(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// take кодує кадр, щойно набралось 960 стерео-семплів. Помилка енкодера
// кадр губить, але годинник рухає: інакше audioGap залив би цю «дірку»
// тишею вдруге.
func (e *opusAudioEncoder) take() []byte {
	const need = opusenc.FrameSamples * opusenc.Channels
	if len(e.pcm) < need {
		return nil
	}
	pkt, err := e.enc.Encode(e.pcm[:need])
	e.pcm = append(e.pcm[:0], e.pcm[need:]...)
	e.emitted += opusenc.FrameSamples
	if err != nil {
		if e.errs++; e.errs == 1 || e.errs%500 == 0 {
			log.Printf("oo-agent: opus encode: %v (помилок %d)", err, e.errs)
		}
		return nil
	}
	return pkt
}

func (e *opusAudioEncoder) silence(n int64) [][]byte {
	var out [][]byte
	for i := int64(0); i < n; i++ {
		e.pcm = append(e.pcm, 0, 0)
		if f := e.take(); f != nil {
			out = append(out, f)
		}
	}
	return out
}

// encode: моно дублюється в обидва канали; з >2 каналів беруться перші два
// (у WAVEFORMATEXTENSIBLE це завжди FL/FR) — центр/тил 5.1 губляться, ціну
// названо чесно, апгрейд — downmix-матриця тут же.
func (e *opusAudioEncoder) encode(data []byte, f audio.Format) [][]byte {
	factor, step, ok := audioLayout(f, opusenc.Rate)
	if !ok {
		return nil
	}
	bps := f.BitsPerSample / 8
	var out [][]byte
	for off := 0; off+step <= len(data); off += step {
		l := audioSample(data[off:], f)
		r := l
		if f.Channels >= 2 {
			r = audioSample(data[off+bps:], f)
		}
		if fr := e.push(l, r, factor); fr != nil {
			out = append(out, fr)
		}
	}
	return out
}
