// Package textmode turns DXGI dirty/move-rect metadata into a "text mode"
// signal (RESEARCH-leaders.md gap #2): when only a small part of the screen
// changes per frame and nothing scrolls or drags, the content is most likely
// typing / reading — the case where sharpness matters more than motion.
//
// Pure Go, no build tags: the capture side feeds it numbers, the policy lives
// here where it can be unit-tested on Linux.
package textmode

// Config tunes the detector. Zero fields take the defaults.
type Config struct {
	// Alpha is the EWMA weight of the newest frame (0..1].
	Alpha float64
	// EnterArea / ExitArea: smoothed changed-area fraction below which we
	// enter text mode, above which we leave it (hysteresis: Enter < Exit).
	EnterArea float64
	ExitArea  float64
	// EnterMotion / ExitMotion: the same for the moved-area fraction (scroll,
	// window drag — DXGI move rects).
	EnterMotion float64
	ExitMotion  float64
	// MinFrames is how many consecutive "quiet" frames are needed to enter.
	MinFrames int
}

// Defaults: a typed character or a blinking caret is far below 1% of a
// 1080p screen; a scrolling page is a large move rect.
const (
	DefaultAlpha       = 0.25
	DefaultEnterArea   = 0.05
	DefaultExitArea    = 0.20
	DefaultEnterMotion = 0.01
	DefaultExitMotion  = 0.05
	DefaultMinFrames   = 8
)

func (c Config) withDefaults() Config {
	if c.Alpha <= 0 || c.Alpha > 1 {
		c.Alpha = DefaultAlpha
	}
	if c.EnterArea <= 0 {
		c.EnterArea = DefaultEnterArea
	}
	if c.ExitArea <= c.EnterArea {
		c.ExitArea = max(DefaultExitArea, c.EnterArea)
	}
	if c.EnterMotion <= 0 {
		c.EnterMotion = DefaultEnterMotion
	}
	if c.ExitMotion <= c.EnterMotion {
		c.ExitMotion = max(DefaultExitMotion, c.EnterMotion)
	}
	if c.MinFrames <= 0 {
		c.MinFrames = DefaultMinFrames
	}
	return c
}

// Fraction returns area/(w*h) clamped to [0,1]. A non-positive frame size
// yields 1 (unknown means "everything changed": never wrongly claim text).
func Fraction(area int64, w, h int) float64 {
	total := int64(w) * int64(h)
	if total <= 0 {
		return 1
	}
	if area <= 0 {
		return 0
	}
	if area >= total {
		return 1
	}
	return float64(area) / float64(total)
}

// Detector is not safe for concurrent use; the agent's frame loop owns it.
type Detector struct {
	cfg    Config
	area   float64
	motion float64
	primed bool
	quiet  int
	text   bool
}

// New returns a detector that starts in non-text mode.
func New(cfg Config) *Detector { return &Detector{cfg: cfg.withDefaults()} }

// Update feeds one ENCODED-worthy frame: changed is the dirty-area fraction,
// moved the move-rect area fraction (both 0..1). No-op frames (nothing
// changed) must not be fed — they carry no information about the content.
// Returns the text-mode state after this frame and whether it just flipped.
func (d *Detector) Update(changed, moved float64) (text, flipped bool) {
	changed, moved = clamp01(changed), clamp01(moved)
	if !d.primed {
		d.area, d.motion, d.primed = changed, moved, true
	} else {
		a := d.cfg.Alpha
		d.area += a * (changed - d.area)
		d.motion += a * (moved - d.motion)
	}
	was := d.text
	if d.text {
		if d.area > d.cfg.ExitArea || d.motion > d.cfg.ExitMotion {
			d.text = false
			d.quiet = 0
		}
	} else {
		if d.area < d.cfg.EnterArea && d.motion < d.cfg.EnterMotion {
			d.quiet++
		} else {
			d.quiet = 0
		}
		if d.quiet >= d.cfg.MinFrames {
			d.text = true
		}
	}
	return d.text, d.text != was
}

// Text reports the current state.
func (d *Detector) Text() bool { return d.text }

// Smoothed returns the current EWMA of changed and moved fractions.
func (d *Detector) Smoothed() (changed, moved float64) { return d.area, d.motion }

// Reset forgets history (e.g. after an output switch).
func (d *Detector) Reset() { *d = Detector{cfg: d.cfg} }

func clamp01(v float64) float64 {
	if v != v || v < 0 { // NaN or negative
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
