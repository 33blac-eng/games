package impair

import (
	"math"
	"sort"
	"time"
)

// Pkt — один прийнятий viewer-ом RTP-пакет (перша копія; дублікати відкидає Recorder).
type Pkt struct {
	Ext    uint64 // розгорнутий seq
	TS     uint32
	Marker bool
	IDR    bool // несе NAL IDR (5): одиночний, у STAP-A або старт FU-A
	Start  bool // починає NAL (не продовження FU-A)
	At     time.Time
	Sent   time.Time // коли пакет вийшов із хаба (вхід реле); нуль — невідомо
	Size   int
}

// IsIDRPayload — чи несе H.264 RTP-payload (RFC 6184) зріз IDR.
func IsIDRPayload(p []byte) bool {
	if len(p) < 2 {
		return false
	}
	switch t := p[0] & 0x1f; t {
	case 5:
		return true
	case 24: // STAP-A
		for i := 1; i+2 < len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			if i+2 < len(p) && p[i+2]&0x1f == 5 {
				return true
			}
			i += 2 + n
		}
	case 28: // FU-A
		return p[1]&0x1f == 5
	}
	return false
}

// IsNALStart — пакет починає NAL: одиночний NAL, STAP-A або FU-A з S-бітом.
func IsNALStart(p []byte) bool {
	if len(p) < 2 {
		return false
	}
	if p[0]&0x1f == 28 {
		return p[1]&0x80 != 0
	}
	return true
}

// Recorder збирає пакети з розгортанням seq і відкиданням дублікатів.
type Recorder struct {
	u    unwrapper
	seen map[uint64]bool
	Pkts []Pkt
}

func NewRecorder() *Recorder { return &Recorder{seen: map[uint64]bool{}} }

func (r *Recorder) Add(seq uint16, ts uint32, marker bool, payload []byte, at, sent time.Time) {
	e := r.u.ext(seq)
	if r.seen[e] {
		return
	}
	r.seen[e] = true
	r.Pkts = append(r.Pkts, Pkt{Ext: e, TS: ts, Marker: marker, IDR: IsIDRPayload(payload), Start: IsNALStart(payload), At: at, Sent: sent, Size: len(payload)})
}

// Frame — кадр (AU) на приймачі.
type Frame struct {
	TS        uint32
	Send      time.Time // вихід першого пакета кадру з хаба (вхід реле); без нього — оцінка по ts
	Complete  bool
	Decodable bool
	IDR       bool
	Done      time.Time // прихід останнього пакета кадру (для повного)
}

const tsStep = 1500 // 90 кГц / 60 к/с

// Frames розкладає пакети на кадри. Межі кадрів — зміна мітки ts у
// послідовності seq; кадр повний, якщо його seq-діапазон без дірок, останній
// пакет має marker, і пакет перед ним — marker попереднього кадру (інакше
// втрачено початок). Кадри, від яких не дійшло нічого, додаються за дірками в
// ts (крок 1500) як неповні. Декодовний = повний і ланцюг від останнього
// повного IDR не рвався.
func Frames(pkts []Pkt) []Frame {
	if len(pkts) == 0 {
		return nil
	}
	ps := append([]Pkt(nil), pkts...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Ext < ps[j].Ext })
	// База часу: мін(прихід - ts/90k) — найшвидший шлях.
	ts0 := ps[0].TS
	rel := func(ts uint32) float64 { return float64(int32(ts-ts0)) / 90000 }
	base := math.Inf(1)
	for _, p := range ps {
		if v := float64(p.At.UnixNano())/1e9 - rel(p.TS); v < base {
			base = v
		}
	}
	type acc struct {
		minE, maxE uint64
		n          int
		idr        bool
		start      bool // пакет із minE починає NAL
		last       time.Time
		sent       time.Time
	}
	var order []uint32
	m := map[uint32]*acc{}
	for _, p := range ps {
		a := m[p.TS]
		if a == nil {
			a = &acc{minE: p.Ext, maxE: p.Ext}
			m[p.TS] = a
			order = append(order, p.TS)
		}
		if p.Ext <= a.minE {
			a.minE = p.Ext
			a.start = p.Start
		}
		if p.Ext > a.maxE {
			a.maxE = p.Ext
		}
		a.n++
		if p.IDR {
			a.idr = true
		}
		if p.At.After(a.last) {
			a.last = p.At
		}
		if !p.Sent.IsZero() && (a.sent.IsZero() || p.Sent.Before(a.sent)) {
			a.sent = p.Sent
		}
	}
	sort.Slice(order, func(i, j int) bool { return int32(order[i]-order[j]) < 0 })
	// marker кожного кадру — за пакетом із найбільшим seq.
	markerAt := map[uint64]bool{}
	for _, p := range ps {
		if p.Marker {
			markerAt[p.Ext] = true
		}
	}
	var out []Frame
	ok := false
	var prevTS uint32
	var prevSend time.Time
	for i, ts := range order {
		a := m[ts]
		send := tsTime(base, rel(ts))
		if !a.sent.IsZero() {
			send = a.sent
		}
		if i > 0 {
			// Кадри, від яких не дійшло нічого: час — інтерполяція між сусідами.
			var miss []uint32
			for gap := prevTS + tsStep; int32(ts-gap) > tsStep/2; gap += tsStep {
				miss = append(miss, gap)
			}
			for k, gap := range miss {
				st := prevSend.Add(send.Sub(prevSend) * time.Duration(k+1) / time.Duration(len(miss)+1))
				out = append(out, Frame{TS: gap, Send: st})
				ok = false
			}
		}
		prevTS, prevSend = ts, send
		// Початок кадру цілий, якщо перед ним marker попереднього кадру, або
		// (евристика, коли той загублений) перший пакет кадру починає NAL —
		// для x264 без зрізів кадр = один VCL NAL, тож так і є.
		complete := uint64(a.n) == a.maxE-a.minE+1 && markerAt[a.maxE] && (i == 0 || markerAt[a.minE-1] || a.start)
		f := Frame{TS: ts, Send: send, Complete: complete, IDR: a.idr}
		if complete {
			f.Done = a.last
			if a.idr {
				ok = true
			}
			f.Decodable = ok
		} else {
			ok = false
		}
		out = append(out, f)
	}
	return out
}

func tsTime(base, rel float64) time.Time {
	s := base + rel
	return time.Unix(0, int64(s*1e9))
}

// FrameStats — підсумок по вікну часу відправки [from, to).
type FrameStats struct {
	Expected, Complete, Decodable, IDR int
	FreezeMs                           float64 // сума розривів > 200 мс між показаними кадрами
	Freezes                            int
	MaxGapMs                           float64
	LatP50Ms, LatP95Ms                 float64 // Done - Send декодовних кадрів (над найшвидшим шляхом)
}

// Summarize рахує по кадрах із Send у [from, to). Показ кадру — Done
// декодовного кадру, але не раніше за попередній показ (декодер монотонний).
func Summarize(fr []Frame, from, to time.Time) FrameStats {
	var s FrameStats
	var lat []float64
	var lastShow time.Time
	first := true
	for _, f := range fr {
		if f.Send.Before(from) || !f.Send.Before(to) {
			continue
		}
		s.Expected++
		if f.Complete {
			s.Complete++
		}
		if f.IDR && f.Complete {
			s.IDR++
		}
		if !f.Decodable {
			continue
		}
		s.Decodable++
		show := f.Done
		if show.Before(lastShow) {
			show = lastShow
		}
		lat = append(lat, float64(show.Sub(f.Send))/1e6)
		if !first {
			if g := float64(show.Sub(lastShow)) / 1e6; g > 200 {
				s.FreezeMs += g
				s.Freezes++
				if g > s.MaxGapMs {
					s.MaxGapMs = g
				}
			} else if g > s.MaxGapMs {
				s.MaxGapMs = g
			}
		} else {
			// розрив від початку вікна до першого показу
			if g := float64(show.Sub(from)) / 1e6; g > 200 {
				s.FreezeMs += g
				s.Freezes++
				s.MaxGapMs = g
			}
		}
		first = false
		lastShow = show
	}
	if first {
		s.FreezeMs = float64(to.Sub(from)) / 1e6
		s.Freezes = 1
		s.MaxGapMs = s.FreezeMs
	} else if g := float64(to.Sub(lastShow)) / 1e6; g > 200 {
		s.FreezeMs += g
		s.Freezes++
		if g > s.MaxGapMs {
			s.MaxGapMs = g
		}
	}
	sort.Float64s(lat)
	s.LatP50Ms = pct(lat, 0.5)
	s.LatP95Ms = pct(lat, 0.95)
	return s
}

func pct(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return v[int(q*float64(len(v)-1))]
}
