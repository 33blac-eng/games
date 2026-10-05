//go:build windows

package encode

import (
	"errors"
	"testing"
)

// TestSubmitWedgedIsTyped — R6-G6: перебудова енкодера після A-12 вирішується
// за статусом OOS_ENC_WEDGED, а не за текстом повідомлення з mft.c. Навіть
// перефразоване повідомлення лишається ErrWedged; звичайна відмова — ні.
func TestSubmitWedgedIsTyped(t *testing.T) {
	if err := submitErr(statusWedged, "MFT stopped raising events"); !errors.Is(err, ErrWedged) {
		t.Fatalf("OOS_ENC_WEDGED -> %v, want errors.Is(ErrWedged)", err)
	}
	if err := submitErr(statusWedged+1, "wedged-looking text"); errors.Is(err, ErrWedged) {
		t.Fatalf("інший статус став ErrWedged: %v", err)
	}
}
