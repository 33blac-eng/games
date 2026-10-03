//go:build windows

package input

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

func checkAvailable() error {
	for _, proc := range []*syscall.LazyProc{procSendInput, procGetSystemMetrics} {
		if err := proc.Find(); err != nil {
			return fmt.Errorf("%w: %v", ErrNotAvailable, err)
		}
	}
	if _, err := VirtualScreen(); err != nil {
		return err
	}
	return nil
}

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// VirtualScreen reports the bounding rectangle of every monitor. Left and Top
// are negative whenever a monitor sits left of or above the primary one, which
// is why they are read at all instead of assuming an origin of 0,0.
func VirtualScreen() (Bounds, error) {
	b := Bounds{
		Left:   systemMetric(smXVirtualScreen),
		Top:    systemMetric(smYVirtualScreen),
		Width:  systemMetric(smCXVirtualScreen),
		Height: systemMetric(smCYVirtualScreen),
	}
	if !b.valid() {
		return Bounds{}, fmt.Errorf("%w: virtual screen is %dx%d", ErrNotAvailable, b.Width, b.Height)
	}
	return b, nil
}

// GetSystemMetrics returns a C int, so the result is truncated to 32 bits to
// keep the sign of a negative origin.
func systemMetric(index int) int32 {
	value, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int32(value)
}

// inject re-reads the virtual screen for every event instead of caching it.
// Monitors get plugged in, resolutions and scaling change, and a cache would
// need invalidation we would get wrong; GetSystemMetrics is a read of data the
// session already keeps, and 60 mouse events per second is not a rate at which
// that matters.
func (in *Injector) inject(ev Event) error {
	virtual, err := VirtualScreen()
	if err != nil {
		return err
	}
	inputs := eventInputs(ev, in.surfaceOr(virtual), virtual)
	if len(inputs) == 0 {
		return nil
	}
	return sendInputs(inputs)
}

func sendInputs(inputs []rawInput) error {
	sent, _, callErr := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if int(sent) != len(inputs) {
		return fmt.Errorf("%w: SendInput queued %d of %d events: %v", ErrBlocked, sent, len(inputs), callErr)
	}
	return nil
}
