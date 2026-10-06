package main

import (
	"math"
	"os"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// R4 (хвиля 7): власний NACK-responder для ніг глядачів замість pion
// nack.ResponderInterceptor.
//
// ЧОМУ. Pion на КОЖЕН вихідний пакет КОЖНОЇ ноги бере буфер із пулу і копіює
// в нього payload (PacketFactoryCopy: ~1,2 КБ memmove + sync.Pool + алокація
// RetainablePacket), а кільце на 1024 пакети тримає ці копії живими —
// ~1,5 МБ на ногу. Хаб же шле ВСІМ ногам ноди той самий незмінний payload
// (forwardToViewers: один *rtp.Packet на всіх, буфер із ReadRTP більше ніхто
// не пише; RED/FEC-обгортки ulpfec — свіжі буфери на кожен виклик). Тож
// кільцю досить ПОСИЛАННЯ на payload: копія заголовка в слот кільця без
// алокацій, payload — спільний для ніг. Профіль 10×10 до змін:
// NewPacket+Release ≈ 6 % CPU хаба, 20 % усіх алокацій (bench/RESULTS-hub.md,
// «R4 (хвиля 7)»).
//
// ЧОГО НЕ ВМІЄ (і не треба хабу): RTX. Хаб не реєструє video/rtx (newAPI), тож
// ретрансмісія йде тим самим SSRC/PT, як і в pion без RTX. Якщо колись RTX
// узгодять, нога з SSRCRetransmission != 0 обслуговується pion-ом (див.
// configureHubNack) — тут вона пропускається.
//
// ЗАГОЛОВОК. Pion викликає writer з &packet.Header ПУЛЬНОГО пакета
// (TrackLocalStaticRTP.WriteRTP), тож вказівник зберігати не можна — лише
// копію. Extensions копіюємо поелементно у власний масив слота: SetExtension
// інших інтерсепторів замінює елемент (зріз payload розширення), а не байти.
//
// OO_SCREEN_NACK_RESPONDER=pion повертає responder pion.

const sharedNackSize = 1024 // як дефолт pion: ~1,4 с історії на 8 Мбіт/с

// Q-12: межі кільця, якщо його розмір рахується з бітрейту.
const (
	nackRingMax     = 8192 // ×8 памʼяті заголовків на ногу; payload спільний
	nackPacketBytes = 1200 // типовий RTP-пакет відео (MTU мінус SRTP/ext)
)

// nackRingSize — Q-12: скільки пакетів тримати, щоб покрити window історії
// на bps. 1024 на 30 Мбіт/с — лише ~0,3 с: NACK на RTT > 300 мс уже марний.
// Округлення вгору до степеня двійки: seq uint16 по модулю розміру має
// ділити 65536, інакше слот після обертання seq зсувається. Межі
// [sharedNackSize, nackRingMax].
func nackRingSize(bps uint64, window time.Duration) int {
	if bps == 0 || window <= 0 {
		return sharedNackSize
	}
	need := int(math.Ceil(float64(bps) * window.Seconds() / 8 / nackPacketBytes))
	n := sharedNackSize
	for n < need && n < nackRingMax {
		n <<= 1
	}
	return n
}

// nackRingSlots — розмір кілець ніг. OO_SCREEN_NACK_WINDOW=<тривалість>
// (напр. 1s) вмикає розмір за бітрейтом; вхід — спільна стеля хаба
// startBitrateBps (фабрика інтерсепторів не знає ноди). Типово ВИМКНЕНО:
// 1024, як у pion.
var nackRingSlots = nackRingSlotsFromEnv(os.Getenv("OO_SCREEN_NACK_WINDOW"), startBitrateBps)

func nackRingSlotsFromEnv(v string, bps uint64) int {
	w, err := time.ParseDuration(v)
	if v == "" || err != nil || w <= 0 {
		return sharedNackSize
	}
	return nackRingSize(bps, w)
}

func newNackRing(size int) *nackRing { return &nackRing{slots: make([]nackSlot, size)} }

var nackResponderPion = os.Getenv("OO_SCREEN_NACK_RESPONDER") == "pion"

type nackSlot struct {
	hdr     rtp.Header
	ext     []rtp.Extension // власне сховище hdr.Extensions
	payload []byte          // спільний із іншими ногами, не мутується
	ok      bool
}

type nackRing struct {
	mu      sync.Mutex
	slots   []nackSlot // len — степінь двійки (nackRingSize)
	highest uint16
	started bool
	w       interceptor.RTPWriter
}

func (r *nackRing) add(h *rtp.Header, payload []byte) {
	r.mu.Lock()
	s := &r.slots[int(h.SequenceNumber)%len(r.slots)]
	s.ext = append(s.ext[:0], h.Extensions...)
	s.hdr = *h
	s.hdr.Extensions = s.ext
	s.payload = payload
	s.ok = true
	if !r.started || int16(h.SequenceNumber-r.highest) > 0 {
		r.highest, r.started = h.SequenceNumber, true
	}
	r.mu.Unlock()
}

// get копіює слот (під локом) — запис у мережу вже без лока.
func (r *nackRing) get(seq uint16, h *rtp.Header, ext *[]rtp.Extension) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || int(r.highest-seq) >= len(r.slots) {
		return nil, false
	}
	s := &r.slots[int(seq)%len(r.slots)]
	if !s.ok || s.hdr.SequenceNumber != seq {
		return nil, false
	}
	*ext = append((*ext)[:0], s.ext...)
	*h = s.hdr
	h.Extensions = *ext
	return s.payload, true
}

func (r *nackRing) clear() {
	r.mu.Lock()
	for i := range r.slots {
		r.slots[i] = nackSlot{}
	}
	r.started = false
	r.mu.Unlock()
}

type sharedNackFactory struct{}

func (sharedNackFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &sharedNackResponder{streams: map[uint32]*nackRing{}}, nil
}

type sharedNackResponder struct {
	interceptor.NoOp
	mu      sync.Mutex
	streams map[uint32]*nackRing
}

func streamHasNack(info *interceptor.StreamInfo) bool {
	for _, fb := range info.RTCPFeedback {
		if fb.Type == "nack" && fb.Parameter == "" {
			return true
		}
	}
	return false
}

func (n *sharedNackResponder) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		i, attr, err := reader.Read(b, a)
		if err != nil {
			return 0, nil, err
		}
		if attr == nil {
			attr = make(interceptor.Attributes)
		}
		pkts, err := attr.GetRTCPPackets(b[:i])
		if err != nil {
			return 0, nil, err
		}
		for _, p := range pkts {
			if nk, ok := p.(*rtcp.TransportLayerNack); ok {
				go n.resend(nk) // як pion: не тримати читача RTCP на записі
			}
		}
		return i, attr, err
	})
}

func (n *sharedNackResponder) BindLocalStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	if !streamHasNack(info) || info.SSRCRetransmission != 0 {
		return w
	}
	r := newNackRing(nackRingSlots)
	r.w = w
	n.mu.Lock()
	n.streams[info.SSRC] = r
	n.mu.Unlock()
	ssrc := info.SSRC
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		if h.SSRC == ssrc {
			r.add(h, payload)
		}
		return w.Write(h, payload, a)
	})
}

func (n *sharedNackResponder) UnbindLocalStream(info *interceptor.StreamInfo) {
	n.mu.Lock()
	r := n.streams[info.SSRC]
	delete(n.streams, info.SSRC)
	n.mu.Unlock()
	if r != nil {
		r.clear()
	}
}

func (n *sharedNackResponder) Close() error {
	n.mu.Lock()
	streams := n.streams
	n.streams = map[uint32]*nackRing{}
	n.mu.Unlock()
	for _, r := range streams {
		r.clear()
	}
	return nil
}

func (n *sharedNackResponder) resend(nk *rtcp.TransportLayerNack) {
	n.mu.Lock()
	r := n.streams[nk.MediaSSRC]
	n.mu.Unlock()
	if r == nil {
		return
	}
	var (
		h   rtp.Header
		ext []rtp.Extension
	)
	for i := range nk.Nacks {
		nk.Nacks[i].Range(func(seq uint16) bool {
			if payload, ok := r.get(seq, &h, &ext); ok {
				_, _ = r.w.Write(&h, payload, interceptor.Attributes{})
			}
			return true
		})
	}
}
