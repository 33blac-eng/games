// Package pacer spreads the RTP packets of one encoded frame over time instead
// of handing them to the socket in one burst.
//
// Why: the agent writes a whole access unit at once; pion packetizes it and a
// large IDR leaves as 100+ back-to-back packets. Any bottleneck with a short
// queue (a capped hub->viewer link, a home router) drops the tail of that
// burst, the frame is lost and the viewer freezes until NACK/PLI repairs it.
//
// Model: a leaky bucket at Factor x the current target bitrate (the
// bitrate_target the hub sends). The bucket holds Burst worth of bytes, so
// small frames still leave immediately; the first packet of a keyframe adds a
// one-time KeyBurst credit. The queue is FIFO (never reorders) and unbounded
// (drops nothing). No packet waits longer than MaxDelay: while a backlog is
// queued the drain rate is raised to whatever empties it before the head's
// deadline, and a head that hits the deadline goes regardless of tokens.
//
// Pacer itself is pure (time is passed in), so it is tested with a fake clock;
// Runner drives it with real timers and Track plugs it under a pion track.
package pacer

import (
	"time"
)

// Config tunes the pacer. Zero fields take the defaults below.
type Config struct {
	// Factor x target bitrate = pacing rate. 1.5..2.5 is the useful range:
	// below that a CBR encoder's normal frame-size swings start to queue.
	Factor float64
	// MaxDelay caps how long any packet may sit in the queue.
	MaxDelay time.Duration
	// Burst is the bucket depth, as time at the pacing rate.
	Burst time.Duration
	// MinBurstBytes is the floor of the bucket depth (low bitrates).
	MinBurstBytes int
	// KeyBurst is the one-time extra credit, as time at the pacing rate,
	// granted when a keyframe starts.
	KeyBurst time.Duration
}

// Defaults.
const (
	DefaultFactor        = 2.0
	DefaultMaxDelay      = 75 * time.Millisecond
	DefaultBurst         = 5 * time.Millisecond
	DefaultMinBurstBytes = 8 * 1200
	DefaultKeyBurst      = 10 * time.Millisecond
	// sendSlack — a packet whose wait is shorter than this goes now: a timer
	// for a few µs costs more than the burst it would avoid.
	sendSlack = 250 * time.Microsecond
)

func (c Config) withDefaults() Config {
	if c.Factor <= 0 {
		c.Factor = DefaultFactor
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = DefaultMaxDelay
	}
	if c.Burst <= 0 {
		c.Burst = DefaultBurst
	}
	if c.MinBurstBytes <= 0 {
		c.MinBurstBytes = DefaultMinBurstBytes
	}
	if c.KeyBurst < 0 {
		c.KeyBurst = 0
	} else if c.KeyBurst == 0 {
		c.KeyBurst = DefaultKeyBurst
	}
	return c
}

type item struct {
	size int
	enq  time.Time
	v    any
}

// Pacer is the pure leaky-bucket queue. Not safe for concurrent use.
type Pacer struct {
	cfg    Config
	rate   float64 // pacing rate, bytes/s; 0 = passthrough (no target yet)
	tokens float64
	last   time.Time
	q      []item
	head   int
	queued int // bytes in q[head:]
}

// New returns a pacer in passthrough mode until SetTarget.
func New(cfg Config) *Pacer { return &Pacer{cfg: cfg.withDefaults()} }

// SetTarget sets the encoder target bitrate (bits/s). 0 = passthrough.
func (p *Pacer) SetTarget(bps uint64) {
	p.rate = float64(bps) / 8 * p.cfg.Factor
	if b := p.burstBytes(); p.tokens > b {
		p.tokens = b
	}
}

// Rate is the current pacing rate in bits/s (0 = passthrough).
func (p *Pacer) Rate() uint64 { return uint64(p.rate * 8) }

// Len is the number of queued packets.
func (p *Pacer) Len() int { return len(p.q) - p.head }

// QueuedBytes is the number of queued bytes.
func (p *Pacer) QueuedBytes() int { return p.queued }

func (p *Pacer) burstBytes() float64 {
	b := p.rate * p.cfg.Burst.Seconds()
	if m := float64(p.cfg.MinBurstBytes); b < m {
		b = m
	}
	return b
}

// Push queues one packet of size bytes. keyStart marks the first packet of
// a keyframe: it adds the KeyBurst credit.
func (p *Pacer) Push(now time.Time, size int, keyStart bool, v any) {
	p.refill(now)
	if keyStart && p.rate > 0 {
		p.tokens += p.rate * p.cfg.KeyBurst.Seconds()
	}
	p.q = append(p.q, item{size: size, enq: now, v: v})
	p.queued += size
}

// refill credits the bucket for the time since the last call. While a
// backlog is queued the drain rate is at least what empties it before the
// head packet's deadline.
func (p *Pacer) refill(now time.Time) {
	if p.last.IsZero() {
		p.last = now
		p.tokens = p.burstBytes()
		return
	}
	dt := now.Sub(p.last).Seconds()
	p.last = now
	if dt <= 0 {
		return
	}
	add := p.effRate(now) * dt
	b := p.burstBytes()
	// Credit above the bucket depth (KeyBurst) is kept until spent but is
	// never topped up by refill.
	if p.tokens+add > b {
		if p.tokens < b {
			p.tokens = b
		}
		return
	}
	p.tokens += add
}

func (p *Pacer) effRate(now time.Time) float64 {
	r := p.rate
	if p.Len() == 0 {
		return r
	}
	left := p.cfg.MaxDelay - now.Sub(p.q[p.head].enq)
	if left <= 0 {
		return r
	}
	if need := float64(p.queued) / left.Seconds(); need > r {
		r = need
	}
	return r
}

// Pop returns the next packet that may be sent at now. If none may, ok is
// false and wait is how long until one may (-1 when the queue is empty).
func (p *Pacer) Pop(now time.Time) (v any, ok bool, wait time.Duration) {
	p.refill(now)
	if p.Len() == 0 {
		return nil, false, -1
	}
	h := p.q[p.head]
	age := now.Sub(h.enq)
	need := float64(h.size) - p.tokens
	switch {
	case p.rate <= 0:
		// passthrough
	case age >= p.cfg.MaxDelay:
		if p.tokens < float64(h.size) {
			p.tokens = float64(h.size) // forgive the debt: lateness is enough
		}
	case need > 0:
		w := time.Duration(need / p.effRate(now) * float64(time.Second))
		if left := p.cfg.MaxDelay - age; w > left {
			w = left
		}
		if w >= sendSlack {
			return nil, false, w
		}
		p.tokens = float64(h.size)
	}
	if p.rate > 0 {
		p.tokens -= float64(h.size)
	}
	p.q[p.head] = item{}
	p.head++
	p.queued -= h.size
	if p.head == len(p.q) {
		p.q, p.head = p.q[:0], 0
	} else if p.head > 1024 && p.head*2 > len(p.q) {
		n := copy(p.q, p.q[p.head:])
		p.q, p.head = p.q[:n], 0
	}
	return h.v, true, 0
}

// Delay is how long the head has waited (0 when empty).
func (p *Pacer) Delay(now time.Time) time.Duration {
	if p.Len() == 0 {
		return 0
	}
	return now.Sub(p.q[p.head].enq)
}
