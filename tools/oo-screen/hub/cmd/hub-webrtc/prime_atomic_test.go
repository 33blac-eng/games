package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

// TestPrimeAllOrNothing — якщо черга ноги не вміщає весь GOP-кеш, priming не
// кладе НІЧОГО (інакше глядач отримав би обрізаний кеш + різані живі пакети).
func TestPrimeAllOrNothing(t *testing.T) {
	quietNDJSON(t, nil)
	var g gopCache
	g.setBitrate(8_000_000)
	n := gopStream(&g, 8_000_000, time.Second, 1200)
	if n < 4 {
		t.Fatalf("замалий кеш: %d", n)
	}
	ns := &nodeSession{nodeID: "prime-atomic"}
	ns.gop = g
	vl := &viewerLeg{out: make(chan *rtp.Packet, n-1), live: true}
	ns.mu.Lock()
	ok := primeViewerLocked(ns, vl)
	ns.mu.Unlock()
	if ok || len(vl.out) != 0 || vl.primeSlack != 0 {
		t.Fatalf("prime=%v, у черзі %d, slack %d; want false, 0, 0", ok, len(vl.out), vl.primeSlack)
	}

	vl = &viewerLeg{out: make(chan *rtp.Packet, n), live: true}
	ns.mu.Lock()
	ok = primeViewerLocked(ns, vl)
	ns.mu.Unlock()
	if !ok || len(vl.out) != n || vl.primeSlack != n {
		t.Fatalf("prime=%v, у черзі %d, slack %d; want true, %d, %d", ok, len(vl.out), vl.primeSlack, n, n)
	}
}
