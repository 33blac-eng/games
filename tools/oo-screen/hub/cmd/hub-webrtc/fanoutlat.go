package main

// Діагностика B7: затримка ВСЕРЕДИНІ хаба — від входу пакета агента у
// forwardToViewers до моменту, коли його SRTP-копія для глядача пішла в
// сокет (egressConn). Лише за OO_SCREEN_FANOUT_LAT=1; без нього — один
// atomic.Bool на пакет.
//
// Як парується: egress seq (переписаний хабом) однаковий для всіх ніг ноди і
// лежить у відкритому заголовку SRTP, тож forwardToViewers кладе час у
// слот [seq], а писар egress читає його за seq відправленого пакета. Це
// точно для однієї ноди (бенч 1×N); на кількох нодах seq-и можуть збігтися
// — тоді семпли брудні, тому діагностика і не для проду.

import (
	"encoding/binary"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var fanoutLatOn = os.Getenv("OO_SCREEN_FANOUT_LAT") == "1"

var fanoutLat struct {
	at  [1 << 16]atomic.Int64 // egress seq -> unix-нс входу в forwardToViewers
	mu  sync.Mutex
	smp []time.Duration
}

func init() {
	if !fanoutLatOn {
		return
	}
	go func() {
		for range time.Tick(10 * time.Second) {
			fanoutLat.mu.Lock()
			s := fanoutLat.smp
			fanoutLat.smp = nil
			fanoutLat.mu.Unlock()
			if len(s) == 0 {
				continue
			}
			sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
			q := func(f float64) float64 { return float64(s[int(float64(len(s)-1)*f)]) / 1e6 }
			log.Printf(`{"leg":"fanout_lat","n":%d,"p50_ms":%.3f,"p95_ms":%.3f,"p99_ms":%.3f,"max_ms":%.3f}`,
				len(s), q(.5), q(.95), q(.99), q(1))
		}
	}()
}

// fanoutLatMark — у forwardToViewers, на вихідний seq.
func fanoutLatMark(seq uint16, now time.Time) {
	if fanoutLatOn {
		fanoutLat.at[seq].Store(now.UnixNano())
	}
}

// fanoutLatSent — у писарі egress, на кожен відправлений RTP-пакет (сегмент).
func fanoutLatSent(b []byte, now int64) {
	if !fanoutLatOn || len(b) < 12 || b[0]>>6 != 2 {
		return
	}
	if pt := b[1] & 0x7f; pt >= 64 && pt <= 95 { // RTCP
		return
	}
	t := fanoutLat.at[binary.BigEndian.Uint16(b[2:4])].Load()
	if t == 0 || now < t || now-t > int64(5*time.Second) {
		return
	}
	fanoutLat.mu.Lock()
	fanoutLat.smp = append(fanoutLat.smp, time.Duration(now-t))
	fanoutLat.mu.Unlock()
}
