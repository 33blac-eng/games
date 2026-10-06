package main

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// Кільце віддає рівно той пакет, що був записаний, і не віддає витіснений
// або застарілий після обертання seq.
func TestNackRingGetEvictWrap(t *testing.T) {
	r := newNackRing(sharedNackSize)
	pay := func(i int) []byte { return []byte{byte(i), byte(i >> 8)} }
	start := uint16(65000) // перехід через 0 посеред історії
	for i := 0; i < 3000; i++ {
		seq := start + uint16(i)
		r.add(&rtp.Header{SequenceNumber: seq, Timestamp: uint32(i)}, pay(i))
	}
	var h rtp.Header
	var ext []rtp.Extension
	last := start + 2999
	for back := 0; back < sharedNackSize; back++ {
		seq := last - uint16(back)
		p, ok := r.get(seq, &h, &ext)
		i := 2999 - back
		if !ok || h.SequenceNumber != seq || !bytes.Equal(p, pay(i)) || h.Timestamp != uint32(i) {
			t.Fatalf("seq %d: ok=%v hdr=%d", seq, ok, h.SequenceNumber)
		}
	}
	if _, ok := r.get(last-sharedNackSize, &h, &ext); ok {
		t.Fatal("витіснений пакет віддано")
	}
	if _, ok := r.get(last+1, &h, &ext); ok {
		t.Fatal("ще не відправлений пакет віддано")
	}
	r.clear()
	if _, ok := r.get(last, &h, &ext); ok {
		t.Fatal("після clear кільце не порожнє")
	}
}

// Заголовок копіюється: pion передає вказівник на ПУЛЬНИЙ пакет, і його
// перезапис (разом з елементами Extensions) не має псувати кільце.
func TestNackRingHeaderIsCopied(t *testing.T) {
	r := newNackRing(sharedNackSize)
	h := &rtp.Header{SequenceNumber: 7, Extension: true, ExtensionProfile: 0xBEDE}
	_ = h.SetExtension(1, []byte{0xAA})
	r.add(h, []byte{1})
	h.SequenceNumber = 99
	h.Extensions = h.Extensions[:0] // reuse пулу: той самий масив елементів
	_ = h.SetExtension(1, []byte{0xBB})
	var got rtp.Header
	var ext []rtp.Extension
	if _, ok := r.get(7, &got, &ext); !ok {
		t.Fatal("нема пакета 7")
	}
	if got.SequenceNumber != 7 || !bytes.Equal(got.GetExtension(1), []byte{0xAA}) {
		t.Fatalf("заголовок зіпсовано: seq=%d ext=%x", got.SequenceNumber, got.GetExtension(1))
	}
}

type capWriter struct {
	mu   sync.Mutex
	seqs []uint16
	pays [][]byte
}

func (c *capWriter) Write(h *rtp.Header, p []byte, _ interceptor.Attributes) (int, error) {
	c.mu.Lock()
	c.seqs = append(c.seqs, h.SequenceNumber)
	c.pays = append(c.pays, p)
	c.mu.Unlock()
	return len(p), nil
}

// Наскрізно через інтерфейс інтерсептора: NACK із RTCP -> ретрансмісія
// тих самих байтів тим самим SSRC; потік без nack-feedback не кешується.
func TestSharedNackResponderResends(t *testing.T) {
	ic, err := sharedNackFactory{}.NewInterceptor("")
	if err != nil {
		t.Fatal(err)
	}
	const ssrc = 1234
	info := &interceptor.StreamInfo{SSRC: ssrc, RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}}}
	cw := &capWriter{}
	w := ic.BindLocalStream(info, cw)
	payload := []byte("спільний payload")
	for s := uint16(10); s < 20; s++ {
		if _, err := w.Write(&rtp.Header{SSRC: ssrc, SequenceNumber: s}, payload, nil); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := (&rtcp.TransportLayerNack{MediaSSRC: ssrc, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{12, 15, 40})}).Marshal()
	rd := ic.BindRTCPReader(interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		return copy(b, raw), a, nil
	}))
	if _, _, err := rd.Read(make([]byte, 1500), nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		cw.mu.Lock()
		n := len(cw.seqs)
		cw.mu.Unlock()
		if n >= 12 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if len(cw.seqs) != 12 || cw.seqs[10] != 12 || cw.seqs[11] != 15 {
		t.Fatalf("ретрансмісії: %v", cw.seqs)
	}
	if &cw.pays[10][0] != &payload[0] {
		t.Fatal("payload скопійовано, а мав бути спільний")
	}

	// Без nack у feedback — writer повертається як є.
	plain := &interceptor.StreamInfo{SSRC: 9}
	if got := ic.BindLocalStream(plain, cw); got != interceptor.RTPWriter(cw) {
		t.Fatal("потік без NACK обгорнуто")
	}
	ic.UnbindLocalStream(info)
	_ = ic.Close()
}

// A/B з pion: go test -run X -bench NackResponderWrite -benchmem
func BenchmarkNackResponderWrite(b *testing.B) {
	b.Run("shared", func(b *testing.B) { benchNackResp(b, sharedNackFactory{}) })
	b.Run("pion", func(b *testing.B) {
		f, _ := nack.NewResponderInterceptor()
		benchNackResp(b, f)
	})
}

func benchNackResp(b *testing.B, f interceptor.Factory) {
	payload := make([]byte, 1200)
	info := &interceptor.StreamInfo{SSRC: 1, RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}}}
	sink := interceptor.RTPWriterFunc(func(_ *rtp.Header, p []byte, _ interceptor.Attributes) (int, error) { return len(p), nil })
	ic, _ := f.NewInterceptor("")
	w := ic.BindLocalStream(info, sink)
	h := &rtp.Header{SSRC: 1}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.SequenceNumber = uint16(i)
		_, _ = w.Write(h, payload, nil)
	}
}

// Q-12: кільце за бітрейтом × вікно, степінь двійки (uint16 seq по модулю
// має ділити 65536), у межах [1024, 8192].
func TestNackRingSize(t *testing.T) {
	for _, c := range []struct {
		bps    uint64
		window time.Duration
		min    int
		max    int
	}{
		{30_000_000, time.Second, 2048, 4096},
		{8_000_000, time.Second, 1024, 1024},
		{1_000_000_000, 2 * time.Second, 8192, 8192},
		{0, time.Second, 1024, 1024},
	} {
		n := nackRingSize(c.bps, c.window)
		if n < c.min || n > c.max || n&(n-1) != 0 {
			t.Fatalf("nackRingSize(%d, %v) = %d, хочу степінь двійки в [%d, %d]", c.bps, c.window, n, c.min, c.max)
		}
	}
}

// Кільце на 4096 тримає пакет, старший за 2000 seq (з 1024 — уже витіснено).
func TestNackRingSized(t *testing.T) {
	r := newNackRing(4096)
	for i := 0; i < 5000; i++ {
		r.add(&rtp.Header{SequenceNumber: uint16(65000 + i)}, []byte{byte(i)})
	}
	var h rtp.Header
	var ext []rtp.Extension
	start, want := uint16(65000), 2999
	last := start + 4999
	if p, ok := r.get(last-2000, &h, &ext); !ok || h.SequenceNumber != last-2000 || p[0] != byte(want) {
		t.Fatalf("пакет 2000 seq тому в кільці на 4096 не віддано: ok=%v", ok)
	}
	if _, ok := r.get(last-4096, &h, &ext); ok {
		t.Fatal("витіснений пакет віддано")
	}
}
