package main

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
)

// mkReport складає RR так, ніби приймач отримав наш SR sentAgo тому й протримав
// його в себе delay перед відповіддю. Тоді справжній RTT = sentAgo - delay.
func mkReport(now time.Time, sentAgo, delay time.Duration) rtcp.ReceptionReport {
	return rtcp.ReceptionReport{
		LastSenderReport: ntpMiddle32(now.Add(-sentAgo)),
		Delay:            uint32(delay.Seconds() * 65536),
	}
}

func TestRTTFromReport(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name    string
		rr      rtcp.ReceptionReport
		wantOK  bool
		wantRTT time.Duration
	}{
		{"120мс у мережі, 30мс у приймача", mkReport(now, 150*time.Millisecond, 30*time.Millisecond), true, 120 * time.Millisecond},
		{"миттєва відповідь", mkReport(now, 20*time.Millisecond, 0), true, 20 * time.Millisecond},

		// --- санітарний кламп: усе це приходить З МЕРЕЖІ ---
		{"SR ще не бачили (LSR=0)", rtcp.ReceptionReport{Delay: 1000}, false, 0},
		{"LSR давніший за межу здорового глузду", mkReport(now, 30*time.Second, 0), false, 0},
		{"LSR із майбутнього (годинник поїхав)", mkReport(now, -5*time.Second, 0), false, 0},
		{"Delay більший за вік SR", mkReport(now, 50*time.Millisecond, 10*time.Second), false, 0},
		{"сміття в обох полях", rtcp.ReceptionReport{LastSenderReport: 0xFFFFFFFF, Delay: 0xFFFFFFFF}, false, 0},
	}
	for _, c := range cases {
		got, ok := rttFromReport(c.rr, now)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v, чекали %v (rtt=%v)", c.name, ok, c.wantOK, got)
			continue
		}
		// Похибка формату: одиниця NTP-32 це 1/65536 с ≈ 15 мкс, беремо 1 мс.
		if ok && (got < c.wantRTT-time.Millisecond || got > c.wantRTT+time.Millisecond) {
			t.Errorf("%s: rtt=%v, чекали %v", c.name, got, c.wantRTT)
		}
	}
}

// НЕГАТИВНИЙ КОНТРОЛЬ до санітарного клампу: без верхньої межі rttSaneMax
// «сміття в обох полях» дало б контролеру гігантський приріст RTT і зрізало б
// ціль ні за що. Доводимо, що саме кламп це ловить, а не збіг.
func TestRTTSaneMaxNegativeControl(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rr := rtcp.ReceptionReport{LastSenderReport: 0xFFFFFFFF, Delay: 0xFFFFFFFF}
	ticks := ntpMiddle32(now) - rr.LastSenderReport - rr.Delay
	raw := time.Duration(ticks) * time.Second / 65536
	if raw <= rttSaneMax {
		t.Fatalf("негативний контроль: сире значення %v і так у межах — кламп нічого не ловить, перевірка порожня", raw)
	}
}

// ntpMiddle32 має збігатися з тим, що кладе в SR sender-report interceptor
// pion-а (ntp.ToNTP32). Перевіряємо не абсолют, а те, що РІЗНИЦЯ двох міток
// дорівнює різниці часу — саме різниця й іде в RTT.
func TestNTPMiddle32Delta(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	for _, d := range []time.Duration{0, time.Millisecond, 250 * time.Millisecond, time.Second, time.Minute} {
		ticks := ntpMiddle32(base.Add(d)) - ntpMiddle32(base)
		got := time.Duration(ticks) * time.Second / 65536
		if got < d-time.Millisecond || got > d+time.Millisecond {
			t.Errorf("дельта %v -> %v (розбіжність %v)", d, got, got-d)
		}
	}
	// Перехід через межу 16-бітних секунд (раз на ~18 год) різницю не ламає:
	// віднімання в uint32 обгортається саме як треба.
	wrap := time.Unix(1_700_000_000, 0)
	for ntpMiddle32(wrap)>>16 < 0xFFFF {
		wrap = wrap.Add(time.Second)
	}
	ticks := ntpMiddle32(wrap.Add(2*time.Second)) - ntpMiddle32(wrap)
	if got := time.Duration(ticks) * time.Second / 65536; got < 2*time.Second-time.Millisecond || got > 2*time.Second+time.Millisecond {
		t.Errorf("на межі 16-бітних секунд дельта 2с вийшла %v", got)
	}
}
