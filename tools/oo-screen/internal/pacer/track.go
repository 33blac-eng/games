package pacer

import (
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Track is a TrackLocalStaticSample whose RTP leaves through a Runner: pion
// still packetizes (same MTU, sequencing and timestamps as before), but each
// packet is queued and released by the leaky bucket instead of hitting the
// socket the moment WriteSample is called. The interceptor chain (NACK
// responder, sender reports) sits below the paced writer, so it sees packets
// at their real send time.
type Track struct {
	*webrtc.TrackLocalStaticSample
	r *Runner

	// set by WriteSample, read by the writer on the same goroutine
	// (TrackLocalStaticSample.WriteSample writes synchronously)
	keyStart bool
}

type queued struct {
	w       webrtc.TrackLocalWriter
	hdr     rtp.Header
	payload []byte
	raw     []byte // Write(b) path
}

// NewTrack wraps inner. Until SetTarget it sends without pacing.
func NewTrack(inner *webrtc.TrackLocalStaticSample, cfg Config) *Track {
	t := &Track{TrackLocalStaticSample: inner}
	t.r = NewRunner(cfg, func(v any) {
		q := v.(*queued)
		if q.raw != nil {
			_, _ = q.w.Write(q.raw)
			return
		}
		_, _ = q.w.WriteRTP(&q.hdr, q.payload)
	})
	return t
}

// SetTarget forwards the encoder target bitrate (bits/s).
func (t *Track) SetTarget(bps uint64) { t.r.SetTarget(bps) }

// Stats of the pacer.
func (t *Track) Stats() Stats { return t.r.Stats() }

// Close stops the pacing goroutine.
func (t *Track) Close() { t.r.Close() }

// WriteSample packetizes s; key marks a keyframe (its first packet gets the
// keyframe burst credit). Not safe for concurrent calls (same as the caller
// of a sample track already assumes: one frame loop).
func (t *Track) WriteSample(s media.Sample, key bool) error {
	t.keyStart = key
	err := t.TrackLocalStaticSample.WriteSample(s)
	t.keyStart = false
	return err
}

// Bind hands pion a context whose WriteStream goes through the pacer.
func (t *Track) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	return t.TrackLocalStaticSample.Bind(pacedCtx{ctx, &pacedWriter{t: t, w: ctx.WriteStream()}})
}

type pacedCtx struct {
	webrtc.TrackLocalContext
	w *pacedWriter
}

func (c pacedCtx) WriteStream() webrtc.TrackLocalWriter { return c.w }

type pacedWriter struct {
	t *Track
	w webrtc.TrackLocalWriter
}

func (w *pacedWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	q := &queued{w: w.w, hdr: h.Clone(), payload: append([]byte(nil), payload...)}
	n := h.MarshalSize() + len(payload)
	w.t.r.Enqueue(n, w.t.keyStart, q)
	w.t.keyStart = false
	return n, nil
}

func (w *pacedWriter) Write(b []byte) (int, error) {
	w.t.r.Enqueue(len(b), false, &queued{w: w.w, raw: append([]byte(nil), b...)})
	return len(b), nil
}
