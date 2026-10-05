// Пункт 41, байтова стеля GOP-кешу: кеш рахує байти (бітрейт × проміжок, не
// більше gopMaxBytes), а пакетна стеля лишилась лише запобіжником.
package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

// gopStream кладе в кеш один GOP: ключовий пакет і далі неключові, рівномірно
// розкладені на span RTP-часу. Повертає, скільки пакетів пішло в кеш.
func gopStream(g *gopCache, bps uint64, span time.Duration, payload int) int {
	pps := int(float64(bps) / 8 / float64(payload) * span.Seconds())
	if pps < 2 {
		pps = 2
	}
	step := uint32(span.Seconds()*gopClockRate) / uint32(pps)
	key := make([]byte, payload)
	copy(key, []byte{0x78, 0, 2, 0x67, 0x42, 0, 2, 0x68, 0xCE, 0, 2, 0x65, 0x88}) // STAP-A: SPS+PPS+IDR
	g.note(&rtp.Packet{Header: rtp.Header{SequenceNumber: 1, Timestamp: 1000}, Payload: key})
	for i := 1; i < pps; i++ {
		p := make([]byte, payload)
		p[0] = 0x21
		g.note(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i + 1), Timestamp: 1000 + uint32(i)*step}, Payload: p})
	}
	return pps
}

// TestGopServes8Mbit2sGop — раніше 512 пакетів (~0.6 с на 8 Мбіт/с) не
// вміщали навіть штатний GOP 2 с: кеш переповнювався й новий глядач чекав IDR.
func TestGopServes8Mbit2sGop(t *testing.T) {
	quietNDJSON(t, nil)
	var g gopCache
	g.setBitrate(8_000_000)
	n := gopStream(&g, 8_000_000, 2*time.Second, 1200)
	if n <= 512 {
		t.Fatalf("тест не перевіряє старий сценарій: лише %d пакетів", n)
	}
	got := g.replay()
	if len(got) != n {
		t.Fatalf("кеш віддав %d пакетів, want %d (overflow=%v)", len(got), n, g.overflow)
	}
	if !h264KeyPart(got[0].Payload) {
		t.Fatalf("кеш починається не з ключового пакета")
	}

	// І такий кеш реально влазить у чергу свіжої ноги, а наступний живий
	// пакет не вважається відставанням.
	ns := &nodeSession{nodeID: "gop-8m"}
	ns.gop = g
	vl := &viewerLeg{out: make(chan *rtp.Packet, viewerQueueDepth+gopMaxPackets), live: true}
	ns.mu.Lock()
	ok := primeViewerLocked(ns, vl)
	ns.mu.Unlock()
	if !ok || len(vl.out) != n || vl.primeSlack != n {
		t.Fatalf("prime=%v, у черзі %d, slack %d; want true, %d, %d", ok, len(vl.out), vl.primeSlack, n, n)
	}
}

// TestGopHugePacketsHitByteCap — величезні пакети впираються в gopMaxBytes
// задовго до пакетної стелі; кеш чесно стає порожнім (нічого не віддає,
// пам'ять відпущено) і відновлюється з наступного IDR.
func TestGopHugePacketsHitByteCap(t *testing.T) {
	quietNDJSON(t, nil)
	var g gopCache
	g.setBitrate(1 << 40) // абсурдний бітрейт — бюджет однаково не вище за стелю
	if b := gopByteBudget(1 << 40); b != gopMaxBytes {
		t.Fatalf("бюджет %d, want жорстку стелю %d", b, gopMaxBytes)
	}
	const sz = 64 << 10
	key := make([]byte, sz)
	copy(key, []byte{0x78, 0, 2, 0x67, 0x42, 0, 2, 0x68, 0xCE, 0, 2, 0x65, 0x88})
	g.note(&rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: key})
	i := 1
	for ; !g.overflow && i < gopMaxPackets; i++ {
		p := make([]byte, sz)
		p[0] = 0x21
		g.note(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i)}, Payload: p})
		if g.bytes > gopMaxBytes {
			t.Fatalf("у кеші %d байтів, понад стелю %d", g.bytes, gopMaxBytes)
		}
	}
	if !g.overflow {
		t.Fatalf("байтова стеля не спрацювала за %d пакетів по %d Б", i, sz)
	}
	if i > gopMaxBytes/sz+1 {
		t.Fatalf("переповнення лише на %d-му пакеті, want ≈%d", i, gopMaxBytes/sz)
	}
	if g.replay() != nil || g.bytes != 0 || len(g.pkts) != 0 {
		t.Fatalf("переповнений кеш не спорожнено: replay=%d bytes=%d", len(g.replay()), g.bytes)
	}
	// Хвіст після переповнення не накопичується (він уже не від IDR)...
	g.note(nonKeyPacket(9000))
	if g.replay() != nil {
		t.Fatalf("після overflow кеш набирає неключові пакети")
	}
	// ...а новий IDR починає кеш заново.
	g.note(keyPacket(9001))
	g.note(nonKeyPacket(9002))
	if n := len(g.replay()); n != 2 {
		t.Fatalf("після нового IDR у кеші %d пакетів, want 2", n)
	}
}

// TestGopLongSpan — довгий GOP агента: зі стелею 3 с GOP 8 с не кешується,
// з OO_SCREEN_GOP_SPAN=10s і з типовою (12 с) — віддається цілком. Межі
// прапорця обрізаються.
func TestGopLongSpan(t *testing.T) {
	quietNDJSON(t, nil)
	old := gopMaxSpan
	t.Cleanup(func() { gopMaxSpan = old })

	gopMaxSpan = clampGopSpan(3 * time.Second)
	var g gopCache
	g.setBitrate(4_000_000)
	gopStream(&g, 4_000_000, 8*time.Second, 1200)
	if g.replay() != nil {
		t.Fatalf("GOP 8 с при стелі 3 с усе одно закешовано")
	}

	gopMaxSpan = clampGopSpan(10 * time.Second)
	g = gopCache{}
	g.setBitrate(4_000_000)
	n := gopStream(&g, 4_000_000, 8*time.Second, 1200)
	if got := len(g.replay()); got != n {
		t.Fatalf("GOP 8 с при стелі 10 с: віддано %d, want %d (overflow=%v)", got, n, g.overflow)
	}

	gopMaxSpan = gopDefaultSpan
	g = gopCache{}
	g.setBitrate(4_000_000)
	n = gopStream(&g, 4_000_000, 10*time.Second, 1200)
	if got := len(g.replay()); got != n {
		t.Fatalf("GOP 10 с при типовій стелі: віддано %d, want %d (overflow=%v)", got, n, g.overflow)
	}

	if d := clampGopSpan(time.Second); d != gopMinSpan {
		t.Fatalf("clamp(1s)=%s, want %s", d, gopMinSpan)
	}
	if d := clampGopSpan(time.Hour); d != gopSpanLimit {
		t.Fatalf("clamp(1h)=%s, want %s", d, gopSpanLimit)
	}
}
