//go:build !windows

package input

import "fmt"

func checkAvailable() error {
	return fmt.Errorf("%w: SendInput requires Windows", ErrNotAvailable)
}

// VirtualScreen has no meaning off Windows; it exists so callers compile
// everywhere and disable input on the platforms that cannot do it.
func VirtualScreen() (Bounds, error) {
	return Bounds{}, fmt.Errorf("%w: requires Windows", ErrNotAvailable)
}

func (in *Injector) inject(Event) error {
	return fmt.Errorf("%w: requires Windows", ErrNotAvailable)
}
