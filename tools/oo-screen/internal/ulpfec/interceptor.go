package ulpfec

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// Params — узгоджені PT для однієї доріжки.
type Params struct {
	RedPT, FecPT uint8
	// HighestOut (опційно) — сюди пишеться найновіший ВИХІДНИЙ seq: після
	// FEC-вставок він випереджає вхідний, і хто судить про буфер NACK
	// responder-а за seq (hub nack.go), мусить дивитись сюди.
	HighestOut *atomic.Uint32
	// OutCount (опційно) — скільки пакетів (медіа + FEC) пішло у вихідний
	// просторі seq: це наповнення буфера NACK responder-а, а не лише медіа.
	OutCount *atomic.Uint64
}

// Config — налаштування генератора. Нульові поля — дефолти.
type Config struct {
	// Lookup повертає PT red/ulpfec для медіа-SSRC, якщо глядач їх узгодив.
	// false — доріжка йде як без FEC (жодного RED). Питається на кожен пакет,
	// доки не дасть true.
	Lookup func(ssrc uint32) (Params, bool)
	// Forget (опційно) — доріжку відвʼязано; прибрати те, що віддавав Lookup.
	Forget func(ssrc uint32)
	// Target — допустима ймовірність, що групу (кадр) FEC не врятує.
	Target float64 // 0.01
	// MaxRate — стеля m/k (частка FEC-пакетів до медіа).
	MaxRate float64 // 0.5
	// MinLoss — підлога оцінки втрат: >0 тримає FEC увімкненим і на чистій
	// лінії (захист від першої секунди нових втрат ціною накладних).
	MinLoss float64 // 0
	// Window — вікно, за яке рахується частка NACK-нутих пакетів.
	Window time.Duration // 500 мс
	// Decay — множник оцінки за вікно, коли нові втрати менші (повільний спад:
	// FEC, що працює, сам гасить NACK-и, і без інерції оцінка б коливалась).
	Decay float64 // 0.95
	Now   func() time.Time
}

func (c *Config) defaults() {
	if c.Target <= 0 {
		c.Target = 0.01
	}
	if c.MaxRate <= 0 {
		c.MaxRate = 0.5
	}
	if c.Window <= 0 {
		c.Window = 500 * time.Millisecond
	}
	if c.Decay <= 0 || c.Decay >= 1 {
		c.Decay = 0.95
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Factory — interceptor.Factory. Ставити ВСЕРЕДИНУ ланцюга відносно
// TWCC/розширень (тобто реєструвати ПЕРЕД ними) і ЗОВНІ відносно NACK
// responder-а (реєструвати ПІСЛЯ нього): responder має кешувати пакети вже з
// новими seq і в RED-обгортці, а захищені байти — збігатися з тим, що піде в
// мережу.
type Factory struct {
	Cfg Config

	mu   sync.Mutex
	last *Interceptor
}

func (f *Factory) NewInterceptor(string) (interceptor.Interceptor, error) {
	c := f.Cfg
	c.defaults()
	i := &Interceptor{cfg: c, streams: map[uint32]*stream{}}
	f.mu.Lock()
	f.last = i
	f.mu.Unlock()
	return i, nil
}

// Last — останній створений інтерсептор (для метрик/тестів).
func (f *Factory) Last() *Interceptor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// Stats — лічильники генератора.
type Stats struct {
	Media, FEC uint64
	Loss       float64 // поточна оцінка втрат
}

type Interceptor struct {
	interceptor.NoOp
	cfg     Config
	mu      sync.Mutex
	streams map[uint32]*stream
}

type inj struct {
	x int64 // розширений вхідний seq, ПІСЛЯ якого вставлено n FEC
	n int64
}

type stream struct {
	mu       sync.Mutex
	ssrc     uint32
	mediaPT  uint8
	p        Params
	resolved bool

	haveIn  bool
	lastIn  int64
	total   int64
	injs    []inj
	group   [][]byte
	pending [][]byte // FEC-payload-и закритих груп поточного кадру
	gBase   uint16
	gTS     uint32
	lastOut uint16

	sent      [4096]bool // out seq відправлено у поточному вікні (для NACK-обліку)
	nacked    [4096]bool
	winStart  time.Time
	winSent   int
	winNacked int
	est       float64

	stats Stats
}

func (s *stream) unwrap(seq uint16) int64 {
	if !s.haveIn {
		return int64(seq)
	}
	d := int64(int16(seq - uint16(s.lastIn)))
	return s.lastIn + d
}

func (s *stream) offset(e int64) int64 {
	off := s.total
	for k := len(s.injs) - 1; k >= 0 && s.injs[k].x >= e; k-- {
		off -= s.injs[k].n
	}
	return off
}

// Stats повертає знімок лічильників для SSRC.
func (i *Interceptor) Stats(ssrc uint32) (Stats, bool) {
	i.mu.Lock()
	s := i.streams[ssrc]
	i.mu.Unlock()
	if s == nil {
		return Stats{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Loss = s.est
	return st, true
}

func (i *Interceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	i.mu.Lock()
	delete(i.streams, info.SSRC)
	i.mu.Unlock()
	if i.cfg.Forget != nil {
		i.cfg.Forget(info.SSRC)
	}
}

func (i *Interceptor) BindLocalStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	if info.MimeType == "" || len(info.MimeType) < 6 || info.MimeType[:6] != "video/" || i.cfg.Lookup == nil {
		return w
	}
	s := &stream{ssrc: info.SSRC, mediaPT: info.PayloadType}
	i.mu.Lock()
	i.streams[info.SSRC] = s
	i.mu.Unlock()
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		if h.SSRC != s.ssrc || h.PayloadType != s.mediaPT {
			return w.Write(h, payload, a)
		}
		s.mu.Lock()
		if !s.resolved {
			if p, ok := i.cfg.Lookup(s.ssrc); ok {
				s.p, s.resolved = p, true
			}
		}
		if !s.resolved {
			s.mu.Unlock()
			return w.Write(h, payload, a)
		}
		late := s.haveIn && s.unwrap(h.SequenceNumber) <= s.lastIn
		out := i.process(s, h, payload)
		if s.p.HighestOut != nil {
			s.p.HighestOut.Store(uint32(s.lastOut))
		}
		// Пізній/повторний вхід (ретрансмісія, дубль проби) займає вже наявний
		// вихідний seq — у наповнення буфера NACK і preLoss його не рахуємо.
		if s.p.OutCount != nil && !late {
			s.p.OutCount.Add(uint64(len(out)))
		}
		s.mu.Unlock()
		var n int
		var err error
		for k, p := range out {
			m, e := w.Write(&p.Header, p.Payload, a)
			if k == 0 {
				n, err = m, e
			} else if err == nil && e != nil {
				err = e
			}
		}
		return n, err
	})
}

// process — під s.mu. Повертає пакети до запису: медіа в RED (перший), далі,
// якщо група закрилась, — FEC у RED.
func (i *Interceptor) process(s *stream, h *rtp.Header, payload []byte) []rtp.Packet {
	now := i.cfg.Now()
	e := s.unwrap(h.SequenceNumber)
	var out []rtp.Packet
	if s.haveIn && e <= s.lastIn {
		// Пізній/повторний вхід (ретрансмісія з ноги агента): своє місце в
		// просторі seq він має, але в групу вже не потрапляє.
		hh := *h
		hh.SequenceNumber = uint16(e + s.offset(e))
		s.markSent(hh.SequenceNumber)
		return append(out, i.red(hh, s.mediaPT, payload, s))
	}
	// Кадр без marker-а (хвіст загублено на нозі агента): нова мітка часу
	// закриває попередній.
	if len(s.group) > 0 && h.Timestamp != s.gTS {
		out = append(out, i.flush(s, now)...)
	}
	// Великий кадр (IDR) — кілька груп по <= 48, але FEC УСІХ груп іде після
	// marker-а: FEC посеред кадру ділив би його seq-діапазон, і втрачений
	// FEC-пакет виглядав би для збирача кадрів як дірка в кадрі.
	if len(s.group) > 0 && uint16(e+s.total)-s.gBase >= MaxGroup {
		i.encodeChunk(s, now)
	}
	s.haveIn = true
	s.lastIn = e
	hh := *h
	hh.SequenceNumber = uint16(e + s.total)
	raw, err := (&rtp.Packet{Header: hh, Payload: payload}).Marshal()
	if err == nil {
		if len(s.group) == 0 {
			s.gBase = hh.SequenceNumber
		}
		s.group = append(s.group, raw)
	}
	s.gTS = hh.Timestamp
	s.lastOut = hh.SequenceNumber
	s.markSent(hh.SequenceNumber)
	s.stats.Media++
	out = append(out, i.red(hh, s.mediaPT, payload, s))
	if h.Marker {
		out = append(out, i.flush(s, now)...)
	}
	return out
}

func (i *Interceptor) encodeChunk(s *stream, now time.Time) {
	i.updateEst(s, now)
	k := len(s.group)
	if k == 0 {
		return
	}
	m := FECCount(k, math.Max(s.est, i.cfg.MinLoss), i.cfg.Target, i.cfg.MaxRate)
	if m > 0 {
		s.pending = append(s.pending, Encode(s.group, m)...)
	}
	s.group = s.group[:0]
}

// flush закриває кадр: FEC усіх його груп отримує seq одразу після медіа.
func (i *Interceptor) flush(s *stream, now time.Time) []rtp.Packet {
	i.encodeChunk(s, now)
	if len(s.pending) == 0 {
		return nil
	}
	out := make([]rtp.Packet, 0, len(s.pending))
	for _, f := range s.pending {
		s.lastOut++
		h := rtp.Header{Version: 2, PayloadType: s.p.RedPT, SequenceNumber: s.lastOut, Timestamp: s.gTS, SSRC: s.ssrc}
		s.markSent(s.lastOut)
		out = append(out, i.red(h, s.p.FecPT, f, s))
	}
	s.pending = s.pending[:0]
	s.total += int64(len(out))
	s.injs = append(s.injs, inj{x: s.lastIn, n: int64(len(out))})
	if len(s.injs) > 256 {
		s.injs = s.injs[len(s.injs)-256:]
	}
	s.stats.FEC += uint64(len(out))
	return out
}

func (s *stream) markSent(seq uint16) {
	s.sent[seq%4096] = true
	s.nacked[seq%4096] = false
	s.winSent++
}

func (i *Interceptor) red(h rtp.Header, blockPT uint8, payload []byte, s *stream) rtp.Packet {
	h.PayloadType = s.p.RedPT
	b := make([]byte, 1+len(payload))
	b[0] = blockPT & 0x7f
	copy(b[1:], payload)
	return rtp.Packet{Header: h, Payload: b}
}

func (i *Interceptor) updateEst(s *stream, now time.Time) {
	if s.winStart.IsZero() {
		s.winStart = now
		return
	}
	if now.Sub(s.winStart) < i.cfg.Window {
		return
	}
	if s.winSent > 0 {
		f := float64(s.winNacked) / float64(s.winSent)
		if f > s.est {
			s.est = f
		} else {
			s.est = math.Max(f, s.est*i.cfg.Decay)
		}
	}
	s.winStart, s.winSent, s.winNacked = now, 0, 0
}

// BindRTCPReader рахує унікальні NACK-нуті seq — оцінку втрат ДО
// ретрансмісій (RR FractionLost її не дає: ретрансмісія, що встигла,
// рахується отриманою).
func (i *Interceptor) BindRTCPReader(r interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attr, err := r.Read(b, a)
		if err != nil {
			return n, attr, err
		}
		if attr == nil {
			attr = interceptor.Attributes{}
		}
		pkts, perr := attr.GetRTCPPackets(b[:n])
		if perr != nil {
			return n, attr, err
		}
		for _, p := range pkts {
			nk, ok := p.(*rtcp.TransportLayerNack)
			if !ok {
				continue
			}
			i.mu.Lock()
			s := i.streams[nk.MediaSSRC]
			i.mu.Unlock()
			if s == nil {
				continue
			}
			s.mu.Lock()
			for _, pair := range nk.Nacks {
				for _, seq := range pair.PacketList() {
					k := seq % 4096
					if s.sent[k] && !s.nacked[k] {
						s.nacked[k] = true
						s.winNacked++
					}
				}
			}
			s.mu.Unlock()
		}
		return n, attr, err
	})
}
