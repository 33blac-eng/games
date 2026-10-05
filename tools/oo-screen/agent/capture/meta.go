package capture

import "github.com/organicoils/oo-screen/internal/textmode"

// ChangedFraction is the dirty-rect area of f as a fraction of the output
// (0..1). Without valid DXGI metadata the whole frame counts as changed.
func ChangedFraction(f *NV12Frame) float64 {
	if f == nil {
		return 1
	}
	if f.NoChange {
		return 0
	}
	if !f.RectsValid {
		return 1
	}
	return textmode.Fraction(f.DirtyArea, f.Width, f.Height)
}
