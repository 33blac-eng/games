//go:build !windows

package consent

import (
	"context"
	"log"
)

// NativeUI на не-Windows: діалогу нема, тож запит = відмова (безпечний бік).
// Policy unattended працює (індикатор — рядок у журналі).
type NativeUI struct{}

func (NativeUI) Ask(context.Context, string) bool {
	log.Printf("consent: no interactive UI on this platform — denying")
	return false
}
func (NativeUI) ShowIndicator(func()) { log.Printf("consent: [indicator] viewer connected") }
func (NativeUI) HideIndicator()       {}
