package bwe

import (
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// linkSim — пакети по 1200 Б з темпом sendBps через вузьке місце capBps
// (FIFO без дропів). Повертає моменти відправки й приходу.
func linkSim(t0 time.Time, n int, sendBps, capBps float64) (send, arr []time.Time) {
	const size = 1200
	gap := time.Duration(float64(size*8) / sendBps * float64(time.Second))
	svc := time.Duration(float64(size*8) / capBps * float64(time.Second))
	var free time.Time
	for i := 0; i < n; i++ {
		s := t0.Add(time.Duration(i) * gap)
		start := s
		if free.After(start) {
			start = free
		}
		free = start.Add(svc)
		send = append(send, s)
		arr = append(arr, free.Add(20*time.Millisecond))
	}
	return
}

func fbFor(base uint16, arr []time.Time) *rtcp.TransportLayerCC {
	ref := arr[0].Truncate(64 * time.Millisecond)
	fb := &rtcp.TransportLayerCC{
		BaseSequenceNumber: base,
		PacketStatusCount:  uint16(len(arr)),
		ReferenceTime:      uint32(ref.Sub(time.Unix(0, 0)) / (64 * time.Millisecond)),
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{
			PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: uint16(len(arr)),
		}},
	}
	prev := ref
	for _, x := range arr {
		fb.RecvDeltas = append(fb.RecvDeltas, &rtcp.RecvDelta{
			Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: int64(x.Sub(prev) / time.Microsecond),
		})
		prev = x
	}
	return fb
}

func TestTrendline(t *testing.T) {
	tr := NewTrendline()
	s, a := linkSim(time.Unix(1000, 0), 3000, 1.5e6, 2e6)
	for i := range s {
		if tr.Add(s[i], a[i]); tr.TakeFired() {
			t.Fatalf("OVERUSE нижче стелі на %d", i)
		}
	}
	tr = NewTrendline()
	s, a = linkSim(time.Unix(1000, 0), 3000, 4e6, 2e6)
	for i := range s {
		if tr.Add(s[i], a[i]); tr.TakeFired() {
			if s[i].Sub(s[0]) > 600*time.Millisecond {
				t.Fatalf("OVERUSE запізно")
			}
			return
		}
	}
	t.Fatal("OVERUSE не спрацював")
}

// Recorder (агент) -> фідбек -> OVERUSE, acked ≈ стелі, sent ≈ темпу.
func TestRecorderAndFeedback(t *testing.T) {
	tw := NewTWCC()
	s, a := linkSim(time.Unix(1000, 0), 1000, 4e6, 2e6)
	i := 0
	f := &RecorderFactory{TWCC: tw, Now: func() time.Time { return s[i] }}
	ic, _ := f.NewInterceptor("")
	var got []uint16
	w := ic.BindLocalStream(&interceptor.StreamInfo{
		MimeType:            "video/H264",
		RTPHeaderExtensions: []interceptor.RTPHeaderExtension{{URI: "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01", ID: 3}},
	}, interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		b := h.GetExtension(3)
		got = append(got, uint16(b[0])<<8|uint16(b[1]))
		return 0, nil
	}))
	for i = range s {
		_, _ = w.Write(&rtp.Header{Version: 2}, make([]byte, 1180), nil)
	}
	if len(got) != len(s) || got[0] != 0 || got[999] != 999 {
		t.Fatalf("seq: %d %v", len(got), got[:3])
	}
	over := false
	var fb Feedback
	for j := 0; j+50 <= len(a); j += 50 {
		fb = tw.OnFeedback(fbFor(uint16(j), a[j:j+50]))
		over = over || fb.Over
	}
	if !over || fb.Acked < 1.8e6 || fb.Acked > 2.2e6 || fb.Sent < 3.4e6 {
		t.Fatalf("over=%v acked=%d sent=%d", over, fb.Acked, fb.Sent)
	}
	// Аудіо і пакети без узгодженого розширення — як є.
	pass := ic.BindLocalStream(&interceptor.StreamInfo{MimeType: "audio/opus",
		RTPHeaderExtensions: []interceptor.RTPHeaderExtension{{URI: "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01", ID: 3}},
	}, interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		if h.Extension {
			t.Fatal("аудіо зі штампом")
		}
		return 0, nil
	}))
	_, _ = pass.Write(&rtp.Header{Version: 2}, nil, nil)
}

func TestLegCtlLossAIMD(t *testing.T) {
	now := time.Unix(5000, 0)
	c := NewLegCtl(4_000_000, 4_000_000)
	if b, ok := c.OnLoss(0.1, now); !ok || b != 2_800_000 {
		t.Fatalf("зріз: %d %v", b, ok)
	}
	if _, ok := c.OnLoss(0.1, now.Add(time.Second)); ok {
		t.Fatal("зріз у межах DownDebounce")
	}
	// Підйом: GoodStreak чистоти і UpDebounce від останньої зміни.
	var up uint64
	for s := 2; s <= 12; s++ {
		if b, ok := c.OnLoss(0, now.Add(time.Duration(s)*time.Second)); ok {
			up = b
			if s < 10 {
				t.Fatalf("підйом на %d с — раніше за UpDebounce", s)
			}
			break
		}
	}
	if up != 2_940_000 {
		t.Fatalf("підйом %d, хотіли +5%%", up)
	}
	// Сіра зона тримає ціль.
	if _, ok := c.OnLoss(0.01, now.Add(30*time.Second)); ok {
		t.Fatal("сіра зона змінила ціль")
	}
}

func TestLegCtlRembAndCeil(t *testing.T) {
	now := time.Unix(5000, 0)
	c := NewLegCtl(8_000_000, 4_000_000)
	if b, _ := c.Target(); b != 4_000_000 {
		t.Fatalf("старт вище стелі: %d", b)
	}
	if b, ok := c.OnRemb(1_500_000, now); !ok || b != 1_500_000 {
		t.Fatalf("REMB: %d %v", b, ok)
	}
	if _, ok := c.OnRemb(3_000_000, now.Add(5*time.Second)); ok {
		t.Fatal("REMB підняв ціль")
	}
	// Підйом по втратах не перестрибує REMB-стелю.
	c2 := NewLegCtl(1_000_000, 4_000_000)
	c2.OnRemb(1_020_000, now)
	for s := 0; s <= 30; s++ {
		c2.OnLoss(0, now.Add(time.Duration(s)*time.Second))
	}
	if b, _ := c2.Target(); b > 1_020_000 {
		t.Fatalf("ціль %d над REMB", b)
	}
	if b, ok := NewLegCtl(100, 0).Target(); b != MinBitrateBps || ok != "" {
		t.Fatalf("підлога: %d", b)
	}
}

func TestLegCtlDelayCutAndHold(t *testing.T) {
	now := time.Unix(5000, 0)
	c := NewLegCtl(8_000_000, 8_000_000)
	b, ok := c.OnTWCC(Feedback{Over: true, Acked: 1_900_000, Sent: 7_200_000, WinStart: now.Add(-time.Second)}, now)
	if !ok || b != uint64(0.85*1_900_000) {
		t.Fatalf("зріз по затримці: %d %v", b, ok)
	}
	if _, r := c.Target(); r != "delay" {
		t.Fatal(r)
	}
	// IDR-сплеск (доставлено все) — нічого.
	c2 := NewLegCtl(1_500_000, 8_000_000)
	if _, ok := c2.OnTWCC(Feedback{Over: true, Acked: 1_480_000, Sent: 1_500_000, WinStart: now}, now); ok {
		t.Fatal("зріз за IDR-сплеск")
	}
	// Під утриманням DelayUpHold підйому нема навіть після довгої чистоти.
	c3 := NewLegCtl(2_000_000, 8_000_000)
	c3.OnTWCC(Feedback{Over: true, Acked: 1_000_000, Sent: 2_000_000, WinStart: now}, now)
	c3.mu.Lock()
	c3.lastSent = now.Add(-time.Minute)
	c3.goodSince = now.Add(-time.Minute)
	c3.mu.Unlock()
	if _, ok := c3.OnLoss(0, now.Add(time.Second)); ok {
		t.Fatal("підйом під час епізоду затримки")
	}
}

// DelayCut — паритет із хабом: каскад, поки енкодер не догнав, заборонено.
func TestDelayCutEncoderLag(t *testing.T) {
	now := time.Unix(5000, 0)
	_, cong, cut := DelayCut(DelayIn{Over: true, Acked: 1_000_000, Sent: 4_000_000, WinStart: now,
		Now: now.Add(time.Second), Target: 1_600_000, LastSent: now.Add(-time.Second), LastWasDelay: true, Floor: MinBitrateBps})
	if !cong || cut {
		t.Fatalf("cong=%v cut=%v", cong, cut)
	}
}
