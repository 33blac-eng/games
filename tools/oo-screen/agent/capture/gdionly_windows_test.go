//go:build windows

package capture

import (
	"context"
	"testing"
	"time"
)

// Windows 7 не має Desktop Duplication; там захоплення — лише GDI (BitBlt ->
// CPU NV12, без D3D-девайса). OO_SCREEN_FORCE_GDI=1 вмикає цей самий конвеєр
// на будь-якій Windows, тож перевіряємо його тут, а не на чужому ПК з Win7.
func TestGDIOnlyPipeline(t *testing.T) {
	t.Setenv("OO_SCREEN_FORCE_GDI", "1")
	c, err := New(0)
	if err != nil {
		t.Fatalf("New(0) у GDI-режимі: %v", err)
	}
	defer c.Close()

	if d := c.Device(); d != 0 {
		t.Fatalf("GDI-режим віддав D3D-девайс %#x: енкодер пішов би апаратним шляхом", d)
	}
	w, h := c.Size()
	if w <= 0 || h <= 0 {
		t.Fatalf("розмір %dx%d", w, h)
	}

	// Перший NextFrame мусить дати кадр (знімка ще не було), навіть на нерухомому екрані.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	f, err := c.NextFrame(ctx)
	if err != nil {
		t.Fatalf("NextFrame: %v", err)
	}
	took := time.Since(start)
	if f.Width != w || f.Height != h || len(f.Y) == 0 || len(f.UV) == 0 {
		t.Fatalf("кадр %dx%d Y=%d UV=%d, чекали %dx%d з обома площинами", f.Width, f.Height, len(f.Y), len(f.UV), w, h)
	}
	// BT.709 limited range: люма в межах 16..235 і робочий стіл — не суцільна заливка.
	minY, maxY := byte(255), byte(0)
	for _, v := range f.Y {
		if v < minY {
			minY = v
		}
		if v > maxY {
			maxY = v
		}
	}
	if minY < 16 || maxY > 235 || maxY == minY {
		t.Fatalf("люма поза limited range або суцільна: min=%d max=%d", minY, maxY)
	}
	t.Logf("GDI-only: %dx%d перший кадр за %v, Y %d..%d", w, h, took.Round(time.Millisecond), minY, maxY)

	g, err := c.GDIFrame()
	if err != nil || len(g.Y) == 0 {
		t.Fatalf("GDIFrame у GDI-режимі: %v", err)
	}
}

// F9 (шар курсора типово ON): на GDI-only (Windows 7) форми DXGI нема, тож
// навіть із дозволеним шаром вказівник МУСИТЬ лишитися в кадрі — інакше
// глядач із шаром не бачить жодного курсора. Без фіксу (dxgi.c
// layer_hides_pointer) CursorComposited=false при видимому вказівнику.
func TestGDIOnlyCursorLayerKeepsPointer(t *testing.T) {
	t.Setenv("OO_SCREEN_FORCE_GDI", "1")
	SetCursorLayer(true)
	defer SetCursorLayer(false)
	c, err := New(0)
	if err != nil {
		t.Fatalf("New(0) у GDI-режимі: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f, err := c.NextFrame(ctx)
	if err != nil {
		t.Fatalf("NextFrame: %v", err)
	}
	if !f.CursorVisible {
		t.Skip("вказівник не видно на цьому виході (сервісна сесія / інший монітор)")
	}
	if !f.CursorComposited {
		t.Fatal("GDI-only + шар курсора: вказівник не вмальовано в кадр, а форми для шару нема")
	}
}
