package pacer

import (
	"sync"
	"testing"
	"time"
)

const pkt = 1200

var t0 = time.Unix(1000, 0)

// drain runs the pacer on a fake clock until empty; returns send times
// relative to t0 in order of the packet ids.
func drain(t *testing.T, p *Pacer, now time.Time) (ids []int, at []time.Duration) {
	t.Helper()
	for i := 0; i < 100000; i++ {
		v, ok, wait := p.Pop(now)
		if ok {
			ids = append(ids, v.(int))
			at = append(at, now.Sub(t0))
			continue
		}
		if wait < 0 {
			return
		}
		if wait == 0 {
			t.Fatal("wait 0 without a packet")
		}
		now = now.Add(wait)
	}
	t.Fatal("pacer did not drain")
	return
}

func pushFrame(p *Pacer, now time.Time, first, n int, key bool) {
	for i := 0; i < n; i++ {
		p.Push(now, pkt, key && i == 0, first+i)
	}
}

func TestPassthroughWithoutTarget(t *testing.T) {
	p := New(Config{})
	pushFrame(p, t0, 0, 200, true)
	ids, at := drain(t, p, t0)
	if len(ids) != 200 || at[199] != 0 {
		t.Fatalf("passthrough: %d packets, last at %v", len(ids), at[len(at)-1])
	}
}

func TestSmallFrameLeavesImmediately(t *testing.T) {
	p := New(Config{})
	p.SetTarget(4_000_000)
	pushFrame(p, t0, 0, 5, false) // 6 KB < 8-packet minimum bucket
	_, at := drain(t, p, t0)
	for i, d := range at {
		if d != 0 {
			t.Fatalf("packet %d delayed %v", i, d)
		}
	}
}

func TestBigFrameIsSpreadAtPacingRate(t *testing.T) {
	p := New(Config{Factor: 2, MaxDelay: time.Second, KeyBurst: -1})
	p.SetTarget(4_000_000) // pacing 8 Mbit/s = 1 MB/s -> 1200 B per 1.2 ms
	pushFrame(p, t0, 0, 100, false)
	ids, at := drain(t, p, t0)
	for i := range ids {
		if ids[i] != i {
			t.Fatalf("reordered: %v", ids)
		}
	}
	// 8 packets ride the bucket, the remaining 92 at 1.2 ms each.
	want := time.Duration(92*pkt) * time.Second / 1_000_000
	last := at[99]
	if last < want-2*time.Millisecond || last > want+2*time.Millisecond {
		t.Fatalf("last packet at %v, want ~%v", last, want)
	}
	// No sub-burst larger than the bucket: in any 10 ms window at most
	// rate*10ms + bucket bytes leave.
	for i := range at {
		n := 0
		for j := i; j < len(at) && at[j]-at[i] < 10*time.Millisecond; j++ {
			n++
		}
		if n*pkt > 10_000+8*pkt+pkt {
			t.Fatalf("burst of %d packets in 10 ms from %v", n, at[i])
		}
	}
}

func TestMaxDelayBoundsQueueing(t *testing.T) {
	p := New(Config{Factor: 1.5, MaxDelay: 75 * time.Millisecond})
	p.SetTarget(1_000_000)         // pacing ~187 KB/s: 400 packets would take 2.5 s
	pushFrame(p, t0, 0, 400, true) // 480 KB
	ids, at := drain(t, p, t0)
	if len(ids) != 400 {
		t.Fatalf("dropped: %d", len(ids))
	}
	for i := range ids {
		if ids[i] != i {
			t.Fatal("reordered")
		}
		if at[i] > 75*time.Millisecond+time.Microsecond {
			t.Fatalf("packet %d waited %v", i, at[i])
		}
	}
	// and it is still spread, not dumped at the deadline
	if at[200] < 20*time.Millisecond {
		t.Fatalf("mid packet at %v: not paced", at[200])
	}
}

func TestKeyframeBurstCredit(t *testing.T) {
	mk := func(key bool) time.Duration {
		p := New(Config{Factor: 2, MaxDelay: time.Second, KeyBurst: 20 * time.Millisecond})
		p.SetTarget(4_000_000)
		pushFrame(p, t0, 0, 60, key)
		_, at := drain(t, p, t0)
		return at[59]
	}
	plain, key := mk(false), mk(true)
	// 20 ms at 1 MB/s = 20 KB of credit ~ 16 packets earlier
	if d := plain - key; d < 15*time.Millisecond || d > 25*time.Millisecond {
		t.Fatalf("keyframe credit saved %v (plain %v key %v)", d, plain, key)
	}
}

func TestIdleDoesNotAccumulateBeyondBucket(t *testing.T) {
	p := New(Config{Factor: 2, MaxDelay: time.Second, KeyBurst: -1})
	p.SetTarget(4_000_000)
	p.Pop(t0)
	later := t0.Add(10 * time.Second)
	pushFrame(p, later, 0, 50, false)
	n := 0
	for {
		_, ok, _ := p.Pop(later)
		if !ok {
			break
		}
		n++
	}
	if n > 9 { // bucket = 8 packets (+1 for slack)
		t.Fatalf("%d packets went at once after idle", n)
	}
}

func TestTargetChangeTakesEffect(t *testing.T) {
	p := New(Config{Factor: 2, MaxDelay: time.Second, KeyBurst: -1})
	p.SetTarget(1_000_000)
	pushFrame(p, t0, 0, 40, false)
	_, slow := drain(t, p, t0)
	p2 := New(Config{Factor: 2, MaxDelay: time.Second, KeyBurst: -1})
	p2.SetTarget(8_000_000)
	pushFrame(p2, t0, 0, 40, false)
	_, fast := drain(t, p2, t0)
	if slow[39] < 4*fast[39] {
		t.Fatalf("1 Mbit/s %v vs 8 Mbit/s %v", slow[39], fast[39])
	}
}

func TestRunnerFIFO(t *testing.T) {
	var mu sync.Mutex
	var got []int
	done := make(chan struct{})
	r := NewRunner(Config{}, func(v any) {
		mu.Lock()
		got = append(got, v.(int))
		if len(got) == 300 {
			close(done)
		}
		mu.Unlock()
	})
	defer r.Close()
	r.SetTarget(50_000_000)
	for i := 0; i < 300; i++ {
		r.Enqueue(pkt, i == 0, i)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner stalled")
	}
	mu.Lock()
	defer mu.Unlock()
	for i, v := range got {
		if v != i {
			t.Fatalf("reordered at %d", i)
		}
	}
	if s := r.Stats(); s.Sent != 300 || s.MaxDelay > time.Second {
		t.Fatalf("stats %+v", s)
	}
}
