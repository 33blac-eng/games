//go:build !windows

package audio

import "fmt"

func newNativeSource() (nativeSource, error) {
	return nil, fmt.Errorf("%w: requires Windows", ErrNotAvailable)
}
