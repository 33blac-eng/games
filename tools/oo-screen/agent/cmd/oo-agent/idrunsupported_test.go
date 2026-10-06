//go:build windows

package main

import (
	"errors"
	"testing"
)

func TestIDRUnsupported(t *testing.T) {
	if !idrUnsupported(errors.New("encode: force idr: AVEncVideoForceKeyFrame: hr=0x80004001")) {
		t.Fatal("E_NOTIMPL від Win7 MFT мусить вмикати перебудову енкодера")
	}
	if idrUnsupported(errors.New("encode: force idr: hr=0x8000FFFF")) || idrUnsupported(nil) {
		t.Fatal("інші помилки/nil — не привід перебудовувати енкодер")
	}
}
