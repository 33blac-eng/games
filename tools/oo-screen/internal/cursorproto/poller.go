package cursorproto

// Pointer-position polling independent of the capture loop.
//
// DXGI reports the pointer only when AcquireNextFrame returns, i.e. at the
// pace of the capture/encode loop: while a big frame encodes, the cursor
// stands still on the viewer. The agent therefore polls the OS pointer
// (GetCursorInfo) on its own goroutine at PollInterval and feeds positions
// straight into the Publisher, whose Coalescer keeps the wire at <= one
// message per CoalesceInterval and only on change. Shapes still come from
// DXGI via the frame loop (ObserveShape).
//
// The poller only "owns" positions while its geometry is trustworthy; when it
// is not (no reading, output rect unknown, or the desktop rect does not match
// the captured frame, e.g. a rotated output) Step reports false and the frame
// loop keeps sending DXGI positions as before.

import "time"

// PollInterval — ~120 Hz; with the 8 ms coalescing the wire sees at most one
// position per tick.
const PollInterval = 8 * time.Millisecond

// Reading is one OS pointer sample: the hotspot in virtual-desktop physical
// pixels and whether the cursor is showing at all.
type Reading struct {
	Showing bool
	X, Y    int
}

// Geometry places the captured output on the virtual desktop.
type Geometry struct {
	Left, Top, W, H int // output DesktopCoordinates
	FrameW, FrameH  int // size of the captured frames
}

// Poller turns readings into Publisher positions, only on change.
type Poller struct {
	Pub *Publisher

	have bool
	last polled
}

type polled struct {
	vis        bool
	x, y, w, h int
}

// Step processes one tick. It returns whether the poller owns the pointer
// position (the frame loop must then only forward shapes).
func (p *Poller) Step(r Reading, rok bool, g Geometry, gok bool) bool {
	if !rok || !gok || g.W <= 0 || g.H <= 0 || g.W != g.FrameW || g.H != g.FrameH {
		p.have = false
		p.Pub.Flush()
		return false
	}
	x, y := r.X-g.Left, r.Y-g.Top
	cur := polled{
		vis: r.Showing && x >= 0 && y >= 0 && x < g.W && y < g.H,
		x:   x, y: y, w: g.FrameW, h: g.FrameH,
	}
	if !cur.vis {
		cur.x, cur.y = 0, 0 // moves on another monitor are not news
	}
	if p.have && cur == p.last {
		p.Pub.Flush() // a coalesced change may still be pending
		return true
	}
	p.have, p.last = true, cur
	p.Pub.ObservePos(cur.vis, cur.x, cur.y, cur.w, cur.h)
	return true
}
