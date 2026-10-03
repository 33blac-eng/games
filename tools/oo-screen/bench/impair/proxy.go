package impair

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy — UDP-реле між viewer-ом і hub-ом. Viewer бачить у answer лише сокет
// Down, hub у offer — лише сокет Up (SDP переписує Rewrite). Вади: Downlink
// (hub->viewer: медіа, SR) і Uplink (viewer->hub: RR, NACK, PLI).
// STUN/DTLS (перший байт поза 128..191, RFC 7983) не чіпаються — інакше ми
// ламали б ICE, а не моделювали мережу.
type Proxy struct {
	Up, Down         *net.UDPConn
	Downlink, Uplink *Link
	hub, viewer      atomic.Pointer[net.UDPAddr]

	downQ, upQ *sched
	Stats      *RTPStats
	// RTCPDown — скільки (S)RTCP-пакетів прийшло від hub-а (NACK, PLI, RR,
	// SR...). Зашифрований SRTCP не розібрати, тож це верхня межа NACK-ів.
	RTCPDown atomic.Int64
}

// NewProxy слухає два сокети на 0.0.0.0 і запускає насоси.
func NewProxy(down, up Config, seed int64) (*Proxy, error) {
	any := &net.UDPAddr{IP: net.IPv4zero}
	u, err := net.ListenUDP("udp4", any)
	if err != nil {
		return nil, err
	}
	d, err := net.ListenUDP("udp4", any)
	if err != nil {
		u.Close()
		return nil, err
	}
	// 8 Мбіт/с із бурстами IDR переповнюють дефолтні 208 КБ буфера сокета —
	// втрати, яких сценарій не замовляв.
	for _, c := range []*net.UDPConn{u, d} {
		_ = c.SetReadBuffer(4 << 20)
		_ = c.SetWriteBuffer(4 << 20)
	}
	p := &Proxy{Up: u, Down: d, Downlink: NewLink(down, seed), Uplink: NewLink(up, seed+1), Stats: NewRTPStats()}
	p.downQ = newSched(func(b []byte) {
		if dst := p.viewer.Load(); dst != nil {
			_, _ = p.Down.WriteToUDP(b, dst)
			p.Stats.Delivered(b, time.Now())
		}
	})
	p.upQ = newSched(func(b []byte) {
		if dst := p.hub.Load(); dst != nil {
			_, _ = p.Up.WriteToUDP(b, dst)
		}
	})
	go p.pump(d, &p.viewer, &p.hub, p.Uplink, p.upQ, false)
	go p.pump(u, &p.hub, &p.viewer, p.Downlink, p.downQ, true)
	return p, nil
}

func (p *Proxy) Close() {
	p.Up.Close()
	p.Down.Close()
	p.downQ.close()
	p.upQ.close()
}

// IsMedia — RTP/RTCP (і SRTP/SRTCP) за RFC 7983.
func IsMedia(b []byte) bool { return len(b) > 0 && b[0] >= 128 && b[0] <= 191 }

// IsRTP — медіа, але не RTCP (RFC 5761: PT 64..95 після маски — RTCP).
func IsRTP(b []byte) bool {
	if !IsMedia(b) || len(b) < 12 {
		return false
	}
	pt := b[1] & 0x7f
	return pt < 64 || pt > 95
}

func (p *Proxy) pump(in *net.UDPConn, from, to *atomic.Pointer[net.UDPAddr], l *Link, q *sched, down bool) {
	buf := make([]byte, 2048)
	for {
		n, src, err := in.ReadFromUDP(buf)
		if err != nil {
			return
		}
		from.Store(src)
		dst := to.Load()
		if dst == nil {
			continue
		}
		b := buf[:n]
		if !IsMedia(b) {
			if down {
				_, _ = p.Down.WriteToUDP(b, dst)
			} else {
				_, _ = p.Up.WriteToUDP(b, dst)
			}
			continue
		}
		now := time.Now()
		if down {
			p.Stats.Ingress(b, now)
			if !IsRTP(b) {
				p.RTCPDown.Add(1)
			}
		}
		at, why := l.Decide(now, n)
		if why != Delivered {
			if down {
				p.Stats.Dropped(b, why)
			}
			continue
		}
		cp := make([]byte, n)
		copy(cp, b)
		q.push(at, cp)
	}
}

// --- планувальник доставки (мін-купа за часом) ---

type item struct {
	at  time.Time
	seq uint64
	b   []byte
}
type pq []item

func (h pq) Len() int { return len(h) }
func (h pq) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h pq) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pq) Push(x any)   { *h = append(*h, x.(item)) }
func (h *pq) Pop() any     { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

type sched struct {
	mu   sync.Mutex
	h    pq
	n    uint64
	wake chan struct{}
	done chan struct{}
	out  func([]byte)
}

func newSched(out func([]byte)) *sched {
	s := &sched{wake: make(chan struct{}, 1), done: make(chan struct{}), out: out}
	go s.run()
	return s
}

func (s *sched) push(at time.Time, b []byte) {
	s.mu.Lock()
	s.n++
	heap.Push(&s.h, item{at, s.n, b})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *sched) close() { close(s.done) }

func (s *sched) run() {
	t := time.NewTimer(time.Hour)
	for {
		s.mu.Lock()
		var wait time.Duration = time.Hour
		for len(s.h) > 0 {
			d := time.Until(s.h[0].at)
			if d > 0 {
				wait = d
				break
			}
			it := heap.Pop(&s.h).(item)
			s.mu.Unlock()
			s.out(it.b)
			s.mu.Lock()
		}
		s.mu.Unlock()
		t.Reset(wait)
		select {
		case <-s.done:
			return
		case <-s.wake:
		case <-t.C:
		}
	}
}

// --- RTP-облік: що дропнуто реле і чи доїхала ретрансмісія ---

// RTPStats рахує за seq на SSRC (заголовок SRTP відкритий): скільки
// унікальних пакетів реле дропнуло і скільки з них потім таки доставлено
// (NACK-ретрансмісія pion іде тим самим SSRC/seq).
type RTPStats struct {
	mu        sync.Mutex
	unwrap    map[uint32]*unwrapper
	dropped   map[uint64]time.Time // ssrc<<32|extseq -> перший дроп
	recovered map[uint64]time.Duration
	seen      map[uint64]struct{}    // уже доставлені: їхній повторний дроп — не втрата
	ingress   map[uint16][]time.Time // коли seq уперше прийшов від хаба (усі обгортки)
	DropLoss  int
	DropQueue int
	Sent      int
	Bytes     int64
}

func NewRTPStats() *RTPStats {
	return &RTPStats{unwrap: map[uint32]*unwrapper{}, dropped: map[uint64]time.Time{}, recovered: map[uint64]time.Duration{}, seen: map[uint64]struct{}{}, ingress: map[uint16][]time.Time{}}
}

// Unwrapper — розгортання 16-бітного seq у монотонний (для споживачів поза пакетом).
type Unwrapper struct{ u unwrapper }

func (w *Unwrapper) Ext(seq uint16) uint64 { return w.u.ext(seq) }

// QueueDrops — скільки RTP-пакетів відкинула черга вузького місця.
func (s *RTPStats) QueueDrops() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.DropQueue
}

type unwrapper struct {
	init bool
	high uint64
}

func (u *unwrapper) ext(seq uint16) uint64 {
	if !u.init {
		u.init, u.high = true, uint64(seq)+1<<16
		return u.high
	}
	cand := (u.high &^ 0xffff) | uint64(seq)
	switch {
	case cand+0x8000 < u.high:
		cand += 1 << 16
	case cand > u.high+0x8000:
		cand -= 1 << 16
	}
	if cand > u.high {
		u.high = cand
	}
	return cand
}

func (s *RTPStats) key(b []byte) uint64 {
	ssrc := binary.BigEndian.Uint32(b[8:12])
	u := s.unwrap[ssrc]
	if u == nil {
		u = &unwrapper{}
		s.unwrap[ssrc] = u
	}
	return uint64(ssrc)<<32 | u.ext(binary.BigEndian.Uint16(b[2:4]))&0xffffffff
}

func (s *RTPStats) Dropped(b []byte, why DropReason) {
	if !IsRTP(b) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if why == DropQueue {
		s.DropQueue++
	} else {
		s.DropLoss++
	}
	k := s.key(b)
	if _, ok := s.seen[k]; ok {
		return
	}
	if _, ok := s.dropped[k]; !ok {
		s.dropped[k] = time.Now()
	}
}

func (s *RTPStats) Delivered(b []byte, now time.Time) {
	if !IsRTP(b) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sent++
	s.Bytes += int64(len(b))
	k := s.key(b)
	s.seen[k] = struct{}{}
	if t, ok := s.dropped[k]; ok {
		if _, done := s.recovered[k]; !done {
			s.recovered[k] = now.Sub(t)
		}
	}
}

// Ingress запамʼятовує перший вхід seq у реле (ретрансмісії не перезаписують).
func (s *RTPStats) Ingress(b []byte, now time.Time) {
	if !IsRTP(b) {
		return
	}
	seq := binary.BigEndian.Uint16(b[2:4])
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.ingress[seq]
	if n := len(v); n > 0 && now.Sub(v[n-1]) < 20*time.Second {
		return // та сама обгортка — це ретрансмісія
	}
	s.ingress[seq] = append(v, now)
}

// SentAt — коли пакет із seq, прийнятий viewer-ом у at, уперше вийшов із хаба.
func (s *RTPStats) SentAt(seq uint16, at time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best time.Time
	for _, t := range s.ingress[seq] {
		if !t.After(at) {
			best = t
		}
	}
	return best
}

// Snapshot — унікальні дропнуті, відновлені, і затримки відновлення.
func (s *RTPStats) Snapshot() (dropped, recovered int, delays []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.recovered {
		delays = append(delays, d)
	}
	return len(s.dropped), len(s.recovered), delays
}

// --- SDP: лишити одного кандидата, що вказує на реле ---

// LearnHub дістає з answer-а адресу hub-а.
func (p *Proxy) LearnHub(sdp string) error {
	f := firstIPv4HostCandidate(sdp)
	if f == nil {
		return fmt.Errorf("в answer немає IPv4 UDP host-кандидата")
	}
	port, err := strconv.Atoi(f[5])
	if err != nil {
		return err
	}
	ip := net.ParseIP(f[4])
	if ip == nil {
		return fmt.Errorf("адреса кандидата %q", f[4])
	}
	p.hub.Store(&net.UDPAddr{IP: ip, Port: port})
	return nil
}

// Rewrite лишає в описі одного кандидата: той самий IP, порт сокета реле.
func Rewrite(sdp string, sock *net.UDPConn) (string, error) {
	f := firstIPv4HostCandidate(sdp)
	if f == nil {
		return "", fmt.Errorf("немає IPv4 UDP host-кандидата")
	}
	f[5] = strconv.Itoa(sock.LocalAddr().(*net.UDPAddr).Port)
	cand := strings.Join(f, " ")
	lines := strings.Split(sdp, "\r\n")
	out := make([]string, 0, len(lines)+1)
	inserted := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "a=candidate:") {
			continue
		}
		if ln == "a=end-of-candidates" && !inserted {
			out, inserted = append(out, cand), true
		}
		out = append(out, ln)
	}
	if !inserted {
		if n := len(out); n > 0 && out[n-1] == "" {
			out = append(out[:n-1], cand, "")
		} else {
			out = append(out, cand)
		}
	}
	return strings.Join(out, "\r\n"), nil
}

func firstIPv4HostCandidate(sdp string) []string {
	for _, ln := range strings.Split(sdp, "\r\n") {
		if !strings.HasPrefix(ln, "a=candidate:") || !strings.Contains(ln, "typ host") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) < 8 || !strings.EqualFold(f[2], "udp") || strings.Contains(f[4], ":") {
			continue
		}
		return f
	}
	return nil
}
