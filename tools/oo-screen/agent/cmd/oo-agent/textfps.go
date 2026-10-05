package main

import (
	"time"

	"github.com/organicoils/oo-screen/internal/textmode"
)

// Gap #2 consumer: in text mode (typing/reading, internal/textmode) the
// screen changes in tiny dirty rects, and 30–60 encoded frames/s buy nothing
// visible while costing CPU, packets and bytes. textModeGap is the minimum
// spacing between ENCODED content frames while text mode lasts; it stacks with
// the software-encoder cap (both gates must pass, so the effective rate is
// the min of the two). Leaving text mode drops it to 0 on the very frame that
// flipped the detector, so motion is never throttled.
//
// Zero means "no cap": text mode off, -text-fps=0, or a cap that is not below
// the session FPS anyway.
func textModeGap(textFPS, fps int, text bool) time.Duration {
	if !text || textFPS <= 0 || (fps > 0 && textFPS >= fps) {
		return 0
	}
	return time.Second / time.Duration(textFPS)
}

// textFlushWait: a content frame swallowed by the text gate must still reach
// the viewer (the last keystroke of a burst would otherwise wait for refine or
// the 1 s keepalive). When one is pending, the capture wait is shortened to
// the moment the gap opens. Returns the (possibly shortened) wait and whether
// it was shortened.
func textFlushWait(pending bool, gap, sinceAdmitted, wait time.Duration) (time.Duration, bool) {
	if !pending || gap <= 0 {
		return wait, false
	}
	rem := gap - sinceAdmitted
	if rem < time.Millisecond {
		rem = time.Millisecond
	}
	if rem >= wait {
		return wait, false
	}
	return rem, true
}

// textFlushDue: the pending frame may be sent now (gap elapsed, there is a
// frame to send and a viewer to send it to).
func textFlushDue(pending, paused, haveLast bool, gap, sinceAdmitted time.Duration) bool {
	return pending && !paused && haveLast && sinceAdmitted >= gap
}

// textCapApplies: the cap is for small changes only. The detector's
// hysteresis (enter < 5 % area, leave > 20 %) keeps text mode on through a
// mid-size change — e.g. a 640x360 video (11 % of 1080p) started right after
// typing — and would halve its frame rate. So a frame whose OWN changed area
// is at least the enter threshold is never held, text mode or not (measured:
// bench/quality/RESULTS-workloads.md, "Текстовий режим").
func textCapApplies(text bool, changed float64) bool {
	return text && changed < textmode.DefaultEnterArea
}
