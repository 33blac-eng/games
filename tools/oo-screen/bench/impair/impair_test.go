package impair

import (
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"
)

var t0 = time.Unix(1000, 0)

func TestUniformLossRate(t *testing.T) {
	l := NewLink(Config{Loss: 0.05}, 1)
	drop := 0
	const n = 200000
	for i := 0; i < n; i++ {
		if _, why := l.Decide(t0.Add(time.Duration(i)*time.Millisecond), 1200); why == DropLoss {
			drop++
		}
	}
	if got := float64(drop) / n; math.Abs(got-0.05) > 0.003 {
		t.Fatalf("втрати %.4f, чекали 0.05", got)
	}
}

func TestGilbertElliottBursts(t *testing.T) {
	// p=0.01 r=0.25: середні втрати 0.01/0.26 ≈ 3.85%, пачка ≈ 4 пакети.
	l := NewLink(Config{Burst: &GE{P: 0.01, R: 0.25, LossBad: 1}}, 2)
	const n = 400000
	drop, runs, cur := 0, 0, 0
	var runLen []int
	for i := 0; i < n; i++ {
		_, why := l.Decide(t0, 100)
		if why == DropLoss {
			drop++
			cur++
		} else if cur > 0 {
			runs++
			runLen = append(runLen, cur)
			cur = 0
		}
	}
	rate := float64(drop) / n
	if math.Abs(rate-0.0385) > 0.004 {
		t.Fatalf("GE втрати %.4f, чекали ~0.0385", rate)
	}
	mean := float64(drop) / float64(runs)
	if mean < 3.3 || mean > 4.7 {
		t.Fatalf("середня пачка %.2f, чекали ~4", mean)
	}
}

func TestDelayAndJitterBounds(t *testing.T) {
	l := NewLink(Config{Delay: 50 * time.Millisecond, Jitter: 10 * time.Millisecond}, 3)
	reordered := 0
	var prev time.Time
	for i := 0; i < 10000; i++ {
		now := t0.Add(time.Duration(i) * time.Millisecond)
		at, why := l.Decide(now, 100)
		if why != Delivered {
			t.Fatal("без втрат")
		}
		d := at.Sub(now)
		if d < 40*time.Millisecond || d > 60*time.Millisecond {
			t.Fatalf("затримка %v поза 50±10", d)
		}
		if at.Before(prev) {
			reordered++
		}
		prev = at
	}
	if reordered == 0 {
		t.Fatal("джитер ±10 мс на 1 пак/мс мав переставляти пакети (як netem)")
	}
}

func TestDelayKeepsOrderWithoutJitter(t *testing.T) {
	l := NewLink(Config{Delay: 20 * time.Millisecond}, 4)
	var prev time.Time
	for i := 0; i < 1000; i++ {
		now := t0.Add(time.Duration(i) * 100 * time.Microsecond)
		at, _ := l.Decide(now, 100)
		if at.Before(prev) || at.Sub(now) != 20*time.Millisecond {
			t.Fatalf("i=%d: at-now=%v", i, at.Sub(now))
		}
		prev = at
	}
}

func TestReorderOvertakes(t *testing.T) {
	l := NewLink(Config{Delay: 30 * time.Millisecond, Reorder: 0.1}, 5)
	over := 0
	var prev time.Time
	for i := 0; i < 5000; i++ {
		at, _ := l.Decide(t0.Add(time.Duration(i)*time.Millisecond), 100)
		if at.Before(prev) {
			over++
		}
		if at.After(prev) {
			prev = at
		}
	}
	if over < 300 || over > 700 {
		t.Fatalf("обгонів %d з 5000, чекали ~500", over)
	}
}

func TestRateCapThroughputAndQueueDrop(t *testing.T) {
	// 8 Мбіт/с на вході в трубу 2 Мбіт/с: доставлено ~2 Мбіт/с, решта — tail-drop.
	l := NewLink(Config{RateBps: 2e6, QueueBytes: 25000}, 6)
	const size = 1000
	var last time.Time
	bytes, qdrop := 0, 0
	for i := 0; i < 10000; i++ { // 1 пакет / 1 мс = 8 Мбіт/с, 10 с
		now := t0.Add(time.Duration(i) * time.Millisecond)
		at, why := l.Decide(now, size)
		switch why {
		case Delivered:
			bytes += size
			if at.Sub(now) > 101*time.Millisecond {
				t.Fatalf("черга 25 КБ на 2 Мбіт/с = 100 мс, а затримка %v", at.Sub(now))
			}
			last = at
		case DropQueue:
			qdrop++
		}
	}
	bps := float64(bytes*8) / last.Sub(t0).Seconds()
	if math.Abs(bps-2e6) > 0.05e6 {
		t.Fatalf("пропускна %.0f, чекали 2e6", bps)
	}
	if qdrop < 7000 {
		t.Fatalf("tail-drop %d, чекали ~7500", qdrop)
	}
}

func TestRateCapNoDropUnderCap(t *testing.T) {
	l := NewLink(Config{RateBps: 8e6}, 7)
	for i := 0; i < 5000; i++ { // 4 Мбіт/с
		if _, why := l.Decide(t0.Add(time.Duration(i)*2*time.Millisecond), 1000); why != Delivered {
			t.Fatal("під стелею дропів бути не має")
		}
	}
}

func TestUnwrap(t *testing.T) {
	var u unwrapper
	a := u.ext(65534)
	b := u.ext(65535)
	c := u.ext(1)
	d := u.ext(65533) // запізніла ретрансмісія до обгортки
	if b != a+1 || c != a+3 || d != a-1 {
		t.Fatalf("%d %d %d %d", a, b, c, d)
	}
}

func rtp(seq uint16) []byte {
	b := make([]byte, 20)
	b[0], b[1] = 0x80, 102
	binary.BigEndian.PutUint16(b[2:], seq)
	binary.BigEndian.PutUint32(b[8:], 0xabcd)
	return b
}

// Живе реле: «hub» шле 2000 RTP-пакетів, 10% втрат; «hub» перешле дропнуті
// повторно (як NACK-responder) — RTPStats має побачити відновлення.
func TestProxyEndToEnd(t *testing.T) {
	p, err := NewProxy(Config{Loss: 0.1, Delay: 5 * time.Millisecond}, Config{}, 9)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	lo := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	hub, _ := net.ListenUDP("udp4", lo)
	viewer, _ := net.ListenUDP("udp4", lo)
	defer hub.Close()
	defer viewer.Close()
	p.hub.Store(hub.LocalAddr().(*net.UDPAddr))
	upAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p.Up.LocalAddr().(*net.UDPAddr).Port}
	downAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p.Down.LocalAddr().(*net.UDPAddr).Port}
	// viewer «озивається» першим — реле дізнається його адресу (як ICE).
	viewer.WriteToUDP([]byte{0, 1}, downAddr)
	buf := make([]byte, 64)
	hub.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := hub.ReadFromUDP(buf); err != nil {
		t.Fatalf("не-медіа viewer->hub мало пройти: %v", err)
	}
	got := map[uint16]bool{}
	done := make(chan struct{})
	go func() {
		b := make([]byte, 64)
		for {
			viewer.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, _, err := viewer.ReadFromUDP(b)
			if err != nil {
				close(done)
				return
			}
			got[binary.BigEndian.Uint16(b[2:n])] = true
		}
	}()
	for i := 0; i < 2000; i++ {
		hub.WriteToUDP(rtp(uint16(i)), upAddr)
		if i%50 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(100 * time.Millisecond)
	dropped, _, _ := p.Stats.Snapshot()
	if dropped < 120 || dropped > 300 {
		t.Fatalf("дропнуто %d з 2000 при 10%%", dropped)
	}
	// «Ретрансмісія» всіх пакетів ще раз — частина знову згубиться (10%).
	for i := 0; i < 2000; i++ {
		hub.WriteToUDP(rtp(uint16(i)), upAddr)
		if i%50 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	<-done
	d2, rec, _ := p.Stats.Snapshot()
	if d2 != dropped {
		// унікальні дропи могли дорости другим проходом — це нормально
		t.Logf("унікальних дропів після повтору: %d", d2)
	}
	if r := float64(rec) / float64(d2); r < 0.8 {
		t.Fatalf("відновлено %d/%d", rec, d2)
	}
	if len(got) < 1950 {
		t.Fatalf("viewer отримав %d унікальних seq", len(got))
	}
}

func TestRewrite(t *testing.T) {
	sdp := "v=0\r\na=candidate:1 1 udp 2130706431 10.0.0.5 50000 typ host\r\na=candidate:2 1 udp 2130706431 ::1 50001 typ host\r\na=end-of-candidates\r\n"
	sock, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer sock.Close()
	out, err := Rewrite(sdp, sock)
	if err != nil {
		t.Fatal(err)
	}
	f := firstIPv4HostCandidate(out)
	if f == nil || f[4] != "10.0.0.5" || f[5] == "50000" {
		t.Fatalf("%q", out)
	}
}
