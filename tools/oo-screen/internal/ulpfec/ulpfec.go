// Package ulpfec — ULPFEC (RFC 5109) у RED (RFC 2198), так, як їх шле і
// приймає libwebrtc: відеопакет загортається в RED з блоком PT медіа, а
// FEC-пакет — у RED із блоком PT ulpfec, у тому ж SSRC і в тому ж просторі
// seq. Chrome приймає red+ulpfec за замовчуванням (на відміну від FlexFEC-03,
// що на прийомі за field trial), а pion receiver бачить їх як звичайні пакети
// треку — тому і стенд (bench/impair/cmd/netbench) може їх декодувати.
//
// Захищається ОРИГІНАЛЬНИЙ медіапакет (PT медіа, не RED) — як у libwebrtc
// UlpfecGenerator: відновлений пакет одразу йде в депакетизатор.
//
// Маска — до 48 пакетів від SN base (L=1). Розкладка «інтерлівінг»: медіапакет
// i групи покриває FEC (i mod m). Один FEC рятує одну втрату у своїй підгрупі.
// Опційно (Encode2D) — 2D-парність стовпці+рядки з перекриттям масок.
package ulpfec

import (
	"encoding/binary"
	"errors"
	"math"
)

const (
	rtpHeader = 12
	fecHeader = 10
	// MaxGroup — скільки медіапакетів покриває одна маска з L=1.
	MaxGroup = 48
)

// Encode будує ULPFEC-payload-и (без RED-байта) для групи медіапакетів pkts
// (повні марш. RTP-пакети з ОСТАТОЧНИМИ seq; seq — у межах 48 від першого,
// не обов'язково підряд). m — кількість FEC-пакетів, 1..len(pkts).
// Повертає m payload-ів.
func Encode(pkts [][]byte, m int) [][]byte {
	k := len(pkts)
	if k == 0 || m <= 0 {
		return nil
	}
	if m > k {
		m = k
	}
	covers := make([][]int, m)
	for f := 0; f < m; f++ {
		for i := f; i < k; i += m {
			covers[f] = append(covers[f], i)
		}
	}
	return encodeCovers(pkts, covers)
}

// Grid2D — розмір сітки для Encode2D: cols стовпців, rows рядків, cols+rows
// FEC-пакетів на k медіа.
func Grid2D(k int) (cols, rows int) {
	if k <= 0 {
		return 0, 0
	}
	cols = int(math.Ceil(math.Sqrt(float64(k))))
	rows = (k + cols - 1) / cols
	return cols, rows
}

// Encode2D — 2D-парність (OO_SCREEN_FEC_LAYOUT=2d): медіапакет i покриває
// FEC-стовпець (i mod cols) І FEC-рядок (i / cols). Маски перекриваються,
// тож декодер, що відновлює ітеративно (libwebrtc, Decoder.attempt), рятує
// будь-які 2 втрати в групі і більшість 3+, на відміну від 1D-інтерлівінгу,
// де 2 втрати в одній підгрупі — вже NACK. Повертає cols+rows payload-ів:
// спершу стовпці, потім рядки.
func Encode2D(pkts [][]byte) [][]byte {
	k := len(pkts)
	cols, rows := Grid2D(k)
	if k == 0 {
		return nil
	}
	covers := make([][]int, 0, cols+rows)
	for c := 0; c < cols; c++ {
		var cv []int
		for i := c; i < k; i += cols {
			cv = append(cv, i)
		}
		covers = append(covers, cv)
	}
	for r := 0; r < rows; r++ {
		var cv []int
		for i := r * cols; i < k && i < (r+1)*cols; i++ {
			cv = append(cv, i)
		}
		covers = append(covers, cv)
	}
	return encodeCovers(pkts, covers)
}

// encodeCovers — по одному FEC-payload-у на кожен набір індексів covers.
func encodeCovers(pkts [][]byte, covers [][]int) [][]byte {
	base := binary.BigEndian.Uint16(pkts[0][2:4])
	out := make([][]byte, 0, len(covers))
	for _, idx := range covers {
		var maxLen int
		var cover [][]byte
		for _, i := range idx {
			cover = append(cover, pkts[i])
			if l := len(pkts[i]) - rtpHeader; l > maxLen {
				maxLen = l
			}
		}
		long := false
		for _, p := range cover {
			if binary.BigEndian.Uint16(p[2:4])-base >= 16 {
				long = true
			}
		}
		maskLen := 2
		if long {
			maskLen = 6
		}
		hl := fecHeader + 2 + maskLen
		b := make([]byte, hl+maxLen)
		var lenRec uint16
		var mask uint64
		for _, p := range cover {
			b[0] ^= p[0]
			b[1] ^= p[1]
			for j := 4; j < 8; j++ {
				b[j] ^= p[j]
			}
			lenRec ^= uint16(len(p) - rtpHeader)
			for j := rtpHeader; j < len(p); j++ {
				b[hl+j-rtpHeader] ^= p[j]
			}
			off := binary.BigEndian.Uint16(p[2:4]) - base
			mask |= 1 << (47 - uint(off))
		}
		b[0] &= 0x3f // E=0
		if long {
			b[0] |= 0x40 // L
		}
		binary.BigEndian.PutUint16(b[2:4], base)
		binary.BigEndian.PutUint16(b[8:10], lenRec)
		binary.BigEndian.PutUint16(b[10:12], uint16(maxLen))
		var mb [8]byte
		binary.BigEndian.PutUint64(mb[:], mask<<16)
		copy(b[12:12+maskLen], mb[:maskLen])
		out = append(out, b)
	}
	return out
}

// parsed — розібраний ULPFEC-payload.
type parsed struct {
	base uint16
	seqs []uint16
	body []byte // b[0..10) — header-recovery, далі — payload-recovery
	plen int
}

var errBad = errors.New("ulpfec: bad packet")

func parse(b []byte) (parsed, error) {
	if len(b) < fecHeader+4 || b[0]&0x80 != 0 {
		return parsed{}, errBad
	}
	maskLen := 2
	if b[0]&0x40 != 0 {
		maskLen = 6
	}
	hl := fecHeader + 2 + maskLen
	if len(b) < hl {
		return parsed{}, errBad
	}
	plen := int(binary.BigEndian.Uint16(b[10:12]))
	if len(b) < hl+plen {
		return parsed{}, errBad
	}
	var mb [8]byte
	copy(mb[:], b[12:12+maskLen])
	mask := binary.BigEndian.Uint64(mb[:]) >> 16
	base := binary.BigEndian.Uint16(b[2:4])
	p := parsed{base: base, plen: plen}
	for i := 0; i < 48; i++ {
		if mask&(1<<(47-uint(i))) != 0 {
			p.seqs = append(p.seqs, base+uint16(i))
		}
	}
	p.body = make([]byte, rtpHeader+plen)
	copy(p.body[:fecHeader], b[:fecHeader])
	copy(p.body[rtpHeader:], b[hl:hl+plen])
	return p, nil
}

// Decoder — приймальна сторона: тримає останні медіапакети і FEC, відновлює
// пакет, коли в підгрупі FEC бракує рівно одного.
type Decoder struct {
	SSRC  uint32
	media map[uint16][]byte
	order []uint16
	fecs  []parsed
	// Recovered — скільки пакетів відновлено (для звітів).
	Recovered int
}

const keep = 1024

func NewDecoder(ssrc uint32) *Decoder {
	return &Decoder{SSRC: ssrc, media: map[uint16][]byte{}}
}

func (d *Decoder) addMedia(seq uint16, raw []byte) bool {
	if _, ok := d.media[seq]; ok {
		return false
	}
	d.media[seq] = raw
	d.order = append(d.order, seq)
	if len(d.order) > keep {
		delete(d.media, d.order[0])
		d.order = d.order[1:]
	}
	return true
}

// AddMedia — отриманий медіапакет (вже без RED, повний RTP із PT медіа).
// Повертає відновлені пакети (повні RTP).
func (d *Decoder) AddMedia(raw []byte) [][]byte {
	if len(raw) < rtpHeader {
		return nil
	}
	cp := append([]byte(nil), raw...)
	if !d.addMedia(binary.BigEndian.Uint16(cp[2:4]), cp) {
		return nil
	}
	return d.attempt()
}

// AddFEC — ULPFEC-payload (вміст RED-блоку з PT ulpfec).
func (d *Decoder) AddFEC(payload []byte) [][]byte {
	p, err := parse(payload)
	if err != nil {
		return nil
	}
	d.fecs = append(d.fecs, p)
	if len(d.fecs) > 256 {
		d.fecs = d.fecs[len(d.fecs)-256:]
	}
	return d.attempt()
}

func (d *Decoder) attempt() [][]byte {
	var out [][]byte
	for again := true; again; {
		again = false
		kept := d.fecs[:0]
		for _, f := range d.fecs {
			missing := -1
			n := 0
			for i, s := range f.seqs {
				if _, ok := d.media[s]; !ok {
					missing = i
					n++
				}
			}
			if n == 0 {
				continue // усе вже є — FEC більше не потрібен
			}
			if n > 1 {
				kept = append(kept, f)
				continue
			}
			if pkt := d.recover(f, f.seqs[missing]); pkt != nil {
				d.addMedia(f.seqs[missing], pkt)
				d.Recovered++
				out = append(out, pkt)
				again = true
			}
		}
		d.fecs = kept
	}
	return out
}

func (d *Decoder) recover(f parsed, seq uint16) []byte {
	b := append([]byte(nil), f.body...)
	lenRec := binary.BigEndian.Uint16(b[8:10])
	for _, s := range f.seqs {
		if s == seq {
			continue
		}
		p := d.media[s]
		b[0] ^= p[0]
		b[1] ^= p[1]
		for j := 4; j < 8; j++ {
			b[j] ^= p[j]
		}
		lenRec ^= uint16(len(p) - rtpHeader)
		for j := rtpHeader; j < len(p) && j < len(b); j++ {
			b[j] ^= p[j]
		}
	}
	if int(lenRec) > f.plen {
		return nil
	}
	b[0] = b[0]&0x3f | 0x80
	binary.BigEndian.PutUint16(b[2:4], seq)
	binary.BigEndian.PutUint32(b[8:12], d.SSRC)
	return b[:rtpHeader+int(lenRec)]
}

// FECCount — скільки FEC на групу з k медіапакетів, щоб імовірність
// «у якійсь підгрупі ≥ 2 втрат» (тобто FEC не рятує, лишається NACK) була
// ≤ target при незалежних втратах p. Втрата самого FEC теж рахується.
// maxRate обмежує m/k. 0 — FEC не потрібен.
func FECCount(k int, p, target, maxRate float64) int {
	if k <= 0 || p <= 0 {
		return 0
	}
	limit := int(math.Floor(float64(k) * maxRate))
	if limit < 1 {
		limit = 1
	}
	if limit > k {
		limit = k
	}
	for m := 1; m <= limit; m++ {
		if Fail1D(k, p, m) <= target {
			return m
		}
	}
	return limit
}

// Fail1D — імовірність, що 1D-інтерлівінг з m FEC на k медіа не врятує
// групу (у якійсь підгрупі ≥ 2 втрат, рахуючи сам FEC) при незалежних
// втратах p.
func Fail1D(k int, p float64, m int) float64 {
	if m <= 0 {
		return 1 - math.Pow(1-p, float64(k))
	}
	ok := 1.0
	for f := 0; f < m; f++ {
		n := (k-f+m-1)/m + 1 // медіа підгрупи + сам FEC
		// P(≤1 втрат з n)
		ok *= math.Pow(1-p, float64(n)) + float64(n)*p*math.Pow(1-p, float64(n-1))
	}
	return 1 - ok
}
