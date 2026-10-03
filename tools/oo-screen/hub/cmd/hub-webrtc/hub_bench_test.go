package main

import (
	"fmt"
	"io"
	"log"
	"math/rand"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
)

// Мікробенчі гарячих шляхів хаба: усе, що робиться НА КОЖЕН RTP-пакет (gop
// кеш, фанаут на N глядачів), на кожен NACK/RR (nack, контролер бітрейту) і на
// кожну подію вводу. Модель потоку — 8 Мбіт/с ≈ 830 пакетів/с по ~1200 байтів,
// 30 fps, IDR кожні 2 с (як у агента).

const (
	benchPktPayload = 1200
	benchPktsPerSec = 830 // 8 Мбіт/с / (1200 Б × 8)
	benchFPS        = 30
	benchGOPSec     = 2
)

// benchStream — один GOP пакетів: перший кадр — IDR (STAP-A SPS/PPS + FU-A
// IDR старт + середини), решта — FU-A не-ключові. ts росте по кадрах 30 fps.
func benchStream() []*rtp.Packet {
	n := benchPktsPerSec * benchGOPSec
	perFrame := benchPktsPerSec / benchFPS
	rng := rand.New(rand.NewSource(1))
	body := make([]byte, benchPktPayload)
	rng.Read(body)
	out := make([]*rtp.Packet, 0, n)
	for i := 0; i < n; i++ {
		p := make([]byte, benchPktPayload)
		copy(p, body)
		switch {
		case i == 0:
			// STAP-A із SPS(7) і PPS(8).
			p = append([]byte{24, 0, 4, 0x67, 0x4d, 0x40, 0x28, 0, 4, 0x68, 0xee, 0x3c, 0x80}, p[:benchPktPayload-13]...)
		case i == 1:
			p[0], p[1] = 28, 0x80|5 // FU-A, S=1, IDR
		case i < perFrame*4: // IDR важчий — кілька кадрів обʼєму
			p[0], p[1] = 28, 5
		default:
			p[0], p[1] = 28, 1 // FU-A середина не-IDR
		}
		out = append(out, &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 102, SequenceNumber: uint16(i), Timestamp: uint32(i/perFrame) * (90000 / benchFPS), SSRC: 1},
			Payload: p,
		})
	}
	return out
}

func benchQuietLog(b *testing.B) {
	b.Helper()
	prev := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(prev) })
}

// gopCache.note на кожен пакет 8 Мбіт/с потоку, з циклічними IDR (скид кешу).
func BenchmarkGopNote8Mbps(b *testing.B) {
	stream := benchStream()
	var g gopCache
	g.setBitrate(8_000_000)
	gop := uint32(len(stream)/benchPktsPerSec*benchGOPSec) * 90000
	b.ReportAllocs()
	i, round := 0, uint32(0)
	for b.Loop() {
		src := stream[i]
		pkt := &rtp.Packet{Header: src.Header, Payload: src.Payload}
		pkt.Timestamp += round * gop
		g.note(pkt)
		i++
		if i == len(stream) {
			i, round = 0, round+1
		}
	}
}

// Відтворення повного 2-секундного GOP-кешу (~1660 пакетів) новому глядачеві:
// primeViewerLocked кладе його в чергу ноги. Чергу вичищаємо поза таймером.
func BenchmarkGopReplay2s(b *testing.B) {
	benchQuietLog(b)
	ns := &nodeSession{nodeID: "bench"}
	ns.gop.setBitrate(8_000_000)
	for _, p := range benchStream() {
		ns.gop.note(p)
	}
	n := len(ns.gop.replay())
	if n == 0 {
		b.Fatal("gop cache empty")
	}
	vl := &viewerLeg{out: make(chan *rtp.Packet, viewerQueueDepth+gopMaxPackets)}
	b.ReportAllocs()
	for b.Loop() {
		if !primeViewerLocked(ns, vl) {
			b.Fatal("prime failed")
		}
		b.StopTimer()
		for len(vl.out) > 0 {
			<-vl.out
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(n), "pkts")
}

// forwardToViewers — повний пер-пакетний шлях хаба: перепис seq/ts, gop.note,
// неблокуючий send у черги N живих ніг. Pump-ів немає: черги вичищаються поза
// таймером кожні viewerQueueDepth/2 пакетів, тож міряється рівно ціна
// форвардингу (WriteRTP pion-а в треку — окрема, пер-нога, у pump-горутині).
func BenchmarkForwardToViewers(b *testing.B) {
	benchQuietLog(b)
	prevOut := ndjsonOut
	ndjsonOut = io.Discard
	b.Cleanup(func() { ndjsonOut = prevOut })
	stream := benchStream()
	for _, n := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			ns := &nodeSession{nodeID: "bench", startBps: 8_000_000}
			ns.agentPC = &webrtc.PeerConnection{}
			ns.viewers = make(map[*webrtc.PeerConnection]*viewerLeg)
			legs := make([]*viewerLeg, n)
			for i := range legs {
				vl := &viewerLeg{ready: true, out: make(chan *rtp.Packet, viewerQueueDepth+gopMaxPackets), done: make(chan struct{})}
				ns.viewers[&webrtc.PeerConnection{}] = vl
				legs[i] = vl
			}
			recomputeBinding(ns)
			drain := func() {
				for _, vl := range legs {
					for len(vl.out) > 0 {
						<-vl.out
					}
				}
			}
			drain()
			b.ReportAllocs()
			i := 0
			seq, ts := uint16(0), uint32(0)
			for b.Loop() {
				src := stream[i%len(stream)]
				// Реальний вхід: seq +1, ts по кадрах — без розривів.
				pkt := &rtp.Packet{Header: src.Header, Payload: src.Payload}
				pkt.SequenceNumber = seq
				if i%(benchPktsPerSec/benchFPS) == 0 {
					ts += 90000 / benchFPS
				}
				pkt.Timestamp = ts
				seq++
				i++
				forwardToViewers(ns, agentGen1, pkt)
				if i%(viewerQueueDepth/2) == 0 {
					b.StopTimer()
					drain()
					b.StartTimer()
				}
			}
			b.StopTimer()
			for _, vl := range legs {
				if !vl.live || vl.discarding {
					b.Fatalf("leg not forwarding: live=%v discarding=%v", vl.live, vl.discarding)
				}
			}
		})
	}
}

// onNack — один NACK з 16 seq (типовий пакет втрати) на ногу з повним кільцем.
func BenchmarkNackLookup(b *testing.B) {
	ns := &nodeSession{nodeID: "bench"}
	vl := &viewerLeg{sent: 100_000, lastSeq: 5000}
	nack := &rtcp.TransportLayerNack{Nacks: []rtcp.NackPair{{PacketID: 4900, LostPackets: 0xFFFF}}}
	now := time.Unix(1000, 0)
	b.ReportAllocs()
	for b.Loop() {
		now = now.Add(10 * time.Millisecond)
		_ = onNack(ns, vl, nack, now)
	}
}

// Один крок контролера бітрейту на RR (лосс + RTT-надлишок).
func BenchmarkBitrateStep(b *testing.B) {
	c := newBitrateCtl(8_000_000)
	now := time.Unix(1000, 0)
	losses := []float64{0, 0, 0.01, 0.06, 0, 0.12, 0}
	i := 0
	b.ReportAllocs()
	for b.Loop() {
		now = now.Add(time.Second)
		c, _ = c.step(losses[i%len(losses)], time.Duration(i%5)*10*time.Millisecond, now)
		i++
	}
}

// judgeInput — повний шлях однієї події вводу від браузера (JSON-конверт,
// перевірка тікета, ліміт). Ліміт безмежний, щоб міряти саме шлях accept.
func BenchmarkJudgeInput(b *testing.B) {
	data := []byte(`{"ticket":"tkt-Ab12","event":{"v":1,"type":"mouse_move","x":0.5,"y":0.25}}`)
	lim := rate.NewLimiter(rate.Inf, 1)
	now := time.Unix(1000, 0)
	b.ReportAllocs()
	for b.Loop() {
		if v, _, why := judgeInput(data, "tkt-Ab12", grantControl, lim, now); v != inputAccept {
			b.Fatal(why)
		}
	}
}
