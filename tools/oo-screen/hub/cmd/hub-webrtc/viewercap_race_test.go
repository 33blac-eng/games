package main

import (
	"sync"
	"testing"
)

// Ранній viewerCapReached у handleOffer і реєстрацію ноги розділяє
// ICE-gathering: стелю мусить тримати сама реєстрація (addViewerLimit).
func TestAddViewerLimitAtomic(t *testing.T) {
	ns := &nodeSession{nodeID: "cap"}
	const limit, n = 3, 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	var added []*viewerLeg
	for i := 0; i < n; i++ {
		pc, trk := newPC(t), newViewerTrack(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if vl := addViewerLimit(ns, pc, trk, "u", limit); vl != nil {
				mu.Lock()
				added = append(added, vl)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, vl := range added {
		removeViewer(ns, vl)
	}
	if len(added) != limit {
		t.Fatalf("added=%d, want %d", len(added), limit)
	}
}
