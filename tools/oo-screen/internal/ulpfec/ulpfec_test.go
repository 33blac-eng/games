package ulpfec

import (
	"bytes"
	"math/rand"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

func mkPkt(t testing.TB, seq uint16, ts uint32, marker bool, n int, r *rand.Rand) []byte {
	pl := make([]byte, n)
	r.Read(pl)
	h := rtp.Header{Version: 2, PayloadType: 102, SequenceNumber: seq, Timestamp: ts, SSRC: 0xabc, Marker: marker}
	if seq%3 == 0 {
		_ = h.SetExtension(1, []byte{byte(seq), 2, 3})
		h.Extension, h.ExtensionProfile = true, 0xBEDE
	}
	b, err := (&rtp.Packet{Header: h, Payload: pl}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTripEveryLoss(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, k := range []int{1, 2, 5, 15, 16, 17, 30, 48} {
		for m := 1; m <= 4 && m <= k; m++ {
			var pk [][]byte
			for i := 0; i < k; i++ {
				pk = append(pk, mkPkt(t, uint16(65530+i), 9000, i == k-1, 50+r.Intn(1100), r))
			}
			fec := Encode(pk, m)
			if len(fec) != m {
				t.Fatalf("k=%d m=%d: %d fec", k, m, len(fec))
			}
			for lost := 0; lost < k; lost++ {
				d := NewDecoder(0xabc)
				var got [][]byte
				for i, p := range pk {
					if i != lost {
						got = append(got, d.AddMedia(p)...)
					}
				}
				for _, f := range fec {
					got = append(got, d.AddFEC(f)...)
				}
				if len(got) != 1 || !bytes.Equal(got[0], pk[lost]) {
					t.Fatalf("k=%d m=%d lost=%d: recovered %d, eq=%v", k, m, lost, len(got),
						len(got) == 1 && bytes.Equal(got[0], pk[lost]))
				}
			}
		}
	}
}

func TestTwoLossesSameSubgroupNotRecovered(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	var pk [][]byte
	for i := 0; i < 6; i++ {
		pk = append(pk, mkPkt(t, uint16(i), 1, false, 200, r))
	}
	fec := Encode(pk, 2) // підгрупи {0,2,4} {1,3,5}
	d := NewDecoder(0xabc)
	for _, i := range []int{1, 3, 5, 4} { // втрачено 0 і 2 — обидва в FEC#0
		d.AddMedia(pk[i])
	}
	if got := d.AddFEC(fec[0]); len(got) != 0 {
		t.Fatal("must not recover two losses with one FEC")
	}
	// 0 і 3 втрачено (різні підгрупи) — обидва рятуються.
	d = NewDecoder(0xabc)
	for _, i := range []int{1, 2, 4, 5} {
		d.AddMedia(pk[i])
	}
	got := append(d.AddFEC(fec[0]), d.AddFEC(fec[1])...)
	if len(got) != 2 {
		t.Fatalf("recovered %d, want 2", len(got))
	}
}

func TestFECCount(t *testing.T) {
	cases := []struct {
		k    int
		p    float64
		want int
	}{
		{13, 0, 0},
		{13, 0.01, 1},
		{13, 0.02, 5},
		{1, 0.02, 1},
	}
	for _, c := range cases {
		if got := FECCount(c.k, c.p, 0.01, 0.5); got != c.want {
			t.Errorf("FECCount(%d,%v)=%d want %d", c.k, c.p, got, c.want)
		}
	}
	if got := FECCount(10, 0.3, 0.01, 0.5); got != 5 {
		t.Errorf("cap: %d", got)
	}
}

type capW struct{ pk []rtp.Packet }

func (c *capW) Write(h *rtp.Header, p []byte, _ interceptor.Attributes) (int, error) {
	c.pk = append(c.pk, rtp.Packet{Header: *h, Payload: append([]byte(nil), p...)})
	return len(p), nil
}

// Через інтерсептор: RED-обгортка, перенумерація seq із FEC-вставками,
// пізній пакет на своєму місці, і декодер відновлює втрачене.
func TestInterceptorEndToEnd(t *testing.T) {
	now := time.Unix(0, 0)
	f := &Factory{Cfg: Config{Lookup: func(uint32) (Params, bool) { return Params{RedPT: 116, FecPT: 117}, true }, MinLoss: 0.05, Now: func() time.Time { return now }}}
	ii, _ := f.NewInterceptor("")
	c := &capW{}
	w := ii.BindLocalStream(&interceptor.StreamInfo{SSRC: 0xabc, PayloadType: 102, MimeType: "video/H264"}, c)
	r := rand.New(rand.NewSource(3))
	var orig []rtp.Packet
	seq := uint16(100)
	for fr := 0; fr < 20; fr++ {
		for i := 0; i < 10; i++ {
			if fr == 5 && i == 3 {
				seq++ // дірка на вході (втрата на нозі агента)
				continue
			}
			p := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 102, SequenceNumber: seq, Timestamp: uint32(fr * 1500), SSRC: 0xabc, Marker: i == 9}}
			p.Payload = make([]byte, 100+r.Intn(900))
			r.Read(p.Payload)
			seq++
			orig = append(orig, p)
			if _, err := w.Write(&p.Header, p.Payload, nil); err != nil {
				t.Fatal(err)
			}
		}
		now = now.Add(16 * time.Millisecond)
	}
	// пізній вхідний 100+53
	late := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 102, SequenceNumber: 153, Timestamp: 5 * 1500, SSRC: 0xabc}, Payload: []byte{1, 2, 3}}
	w.Write(&late.Header, late.Payload, nil)

	var media, fec int
	outSeqs := map[uint16]bool{}
	for _, p := range c.pk {
		if p.PayloadType != 116 {
			t.Fatalf("PT %d", p.PayloadType)
		}
		if outSeqs[p.SequenceNumber] {
			t.Fatalf("dup out seq %d", p.SequenceNumber)
		}
		outSeqs[p.SequenceNumber] = true
		switch p.Payload[0] {
		case 102:
			media++
		case 117:
			fec++
		}
	}
	if media != len(orig)+1 || fec < 20 {
		t.Fatalf("media %d fec %d", media, fec)
	}
	// Вихід суцільний, окрім дірки, яку заповнив пізній.
	first := c.pk[0].SequenceNumber
	for s := 0; s < len(c.pk); s++ {
		if !outSeqs[first+uint16(s)] {
			t.Fatalf("hole in out seq at +%d", s)
		}
	}
	// Губимо кожен 7-й медіапакет, декодуємо.
	d := NewDecoder(0xabc)
	var lost, rec int
	for n, p := range c.pk {
		if p.Payload[0] == 117 {
			rec += len(d.AddFEC(p.Payload[1:]))
			continue
		}
		if n%7 == 0 {
			lost++
			continue
		}
		h := p.Header
		h.PayloadType = 102
		raw, _ := (&rtp.Packet{Header: h, Payload: p.Payload[1:]}).Marshal()
		rec += len(d.AddMedia(raw))
	}
	if rec == 0 || rec < lost*2/3 {
		t.Fatalf("recovered %d of %d", rec, lost)
	}
	st, _ := ii.(*Interceptor).Stats(0xabc)
	t.Logf("media=%d fec=%d overhead=%.0f%% lost=%d recovered=%d", st.Media, st.FEC, 100*float64(st.FEC)/float64(st.Media), lost, rec)
}

func TestNackDrivesEstimate(t *testing.T) {
	now := time.Unix(0, 0)
	f := &Factory{Cfg: Config{Lookup: func(uint32) (Params, bool) { return Params{RedPT: 116, FecPT: 117}, true }, Now: func() time.Time { return now }}}
	ii, _ := f.NewInterceptor("")
	c := &capW{}
	w := ii.BindLocalStream(&interceptor.StreamInfo{SSRC: 1, PayloadType: 102, MimeType: "video/H264"}, c)
	nackR := ii.BindRTCPReader(interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		raw, _ := rtcp.Marshal([]rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 1, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{c.pk[len(c.pk)-3].SequenceNumber})}})
		return copy(b, raw), a, nil
	}))
	buf := make([]byte, 1500)
	seq := uint16(0)
	for fr := 0; fr < 300; fr++ {
		for i := 0; i < 10; i++ {
			h := rtp.Header{Version: 2, PayloadType: 102, SequenceNumber: seq, SSRC: 1, Marker: i == 9}
			seq++
			w.Write(&h, make([]byte, 500), nil)
		}
		if fr%5 == 0 { // 1 NACK на 50 пакетів = 2 %
			nackR.Read(buf, nil)
		}
		now = now.Add(16 * time.Millisecond)
	}
	st, _ := ii.(*Interceptor).Stats(1)
	if st.Loss < 0.01 || st.Loss > 0.03 || st.FEC == 0 {
		t.Fatalf("est %.3f fec %d", st.Loss, st.FEC)
	}
	t.Logf("est=%.3f overhead=%.1f%%", st.Loss, 100*float64(st.FEC)/float64(st.Media))
}

// 2D-парність рятує будь-які 2 втрати в групі (1D з тим самим бюджетом — ні).
func TestEncode2DAnyTwoLosses(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	const k = 25
	var pkts [][]byte
	for i := 0; i < k; i++ {
		pkts = append(pkts, mkPkt(t, uint16(1000+i), 90000, i == k-1, 200+r.Intn(900), r))
	}
	fecs := Encode2D(pkts)
	if c, rw := Grid2D(k); len(fecs) != c+rw {
		t.Fatalf("fec=%d want %d", len(fecs), c+rw)
	}
	for a := 0; a < k; a++ {
		for b := a + 1; b < k; b++ {
			d := NewDecoder(0xabc)
			for i, p := range pkts {
				if i != a && i != b {
					d.AddMedia(p)
				}
			}
			for _, f := range fecs {
				d.AddFEC(f)
			}
			if d.Recovered != 2 {
				t.Fatalf("lost %d,%d: recovered %d", a, b, d.Recovered)
			}
			if string(d.media[uint16(1000+a)]) != string(pkts[a]) || string(d.media[uint16(1000+b)]) != string(pkts[b]) {
				t.Fatalf("lost %d,%d: bytes differ", a, b)
			}
		}
	}
}

func TestUse2D(t *testing.T) {
	if use2D(3, 1, 0.02, 0.01, 0.5) {
		t.Fatal("k<4 must stay 1D")
	}
	if !use2D(25, 12, 0.02, 0.01, 0.5) {
		t.Fatal("capped 1D at 2% must switch to 2D")
	}
	if use2D(25, 1, 0.0001, 0.01, 0.5) {
		t.Fatal("cheap 1D that meets target must stay 1D")
	}
}
