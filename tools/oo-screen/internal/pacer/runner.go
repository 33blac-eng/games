package pacer

import (
	"sync"
	"time"
)

// Stats are counters since the runner started.
type Stats struct {
	Sent        uint64
	MaxDelay    time.Duration // worst queueing delay seen
	MaxQueueLen int
}

// Runner drives a Pacer on its own goroutine: Enqueue never blocks, send is
// called in FIFO order from the runner goroutine only.
type Runner struct {
	mu     sync.Mutex
	p      *Pacer
	send   func(any)
	wake   chan struct{}
	done   chan struct{}
	closed bool
	st     Stats
	now    func() time.Time
}

// NewRunner starts the pacing goroutine. Close stops it.
func NewRunner(cfg Config, send func(any)) *Runner {
	r := &Runner{p: New(cfg), send: send, wake: make(chan struct{}, 1), done: make(chan struct{}), now: time.Now}
	go r.loop()
	return r
}

// SetTarget forwards the encoder target bitrate (bits/s).
func (r *Runner) SetTarget(bps uint64) {
	r.mu.Lock()
	r.p.SetTarget(bps)
	r.mu.Unlock()
	r.kick()
}

// Enqueue queues v (size bytes on the wire).
func (r *Runner) Enqueue(size int, keyStart bool, v any) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.p.Push(r.now(), size, keyStart, v)
	if n := r.p.Len(); n > r.st.MaxQueueLen {
		r.st.MaxQueueLen = n
	}
	r.mu.Unlock()
	r.kick()
}

// Stats returns a snapshot of the counters.
func (r *Runner) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}

// Close stops the goroutine; whatever is still queued is discarded (the
// connection it was meant for is going away).
func (r *Runner) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	close(r.done)
}

func (r *Runner) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) loop() {
	t := time.NewTimer(time.Hour)
	t.Stop()
	for {
		r.mu.Lock()
		now := r.now()
		d := r.p.Delay(now)
		v, ok, wait := r.p.Pop(now)
		if ok {
			if d > r.st.MaxDelay {
				r.st.MaxDelay = d
			}
			r.st.Sent++
		}
		r.mu.Unlock()
		if ok {
			r.send(v)
			continue
		}
		if wait >= 0 {
			t.Reset(wait)
		}
		select {
		case <-r.done:
			t.Stop()
			return
		case <-r.wake:
			t.Stop()
		case <-t.C:
		}
	}
}
