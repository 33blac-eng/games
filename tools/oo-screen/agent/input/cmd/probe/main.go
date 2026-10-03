//go:build windows

// Command probe checks that input injection works on this machine.
//
// By default it only prints the mapping and touches nothing. With -move it
// nudges the pointer a few pixels and puts it straight back, which is the one
// thing that proves SendInput really reaches the desktop. It never presses a
// key or a mouse button: somebody may well be working on the machine being
// probed, and a stray click there is not recoverable.
package main

import (
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/organicoils/oo-screen/agent/input"
)

func main() {
	move := flag.Bool("move", false, "nudge the pointer a few pixels and put it back")
	delta := flag.Int("delta", 5, "nudge distance in pixels")
	flag.Parse()

	injector, err := input.New()
	if err != nil {
		fmt.Printf("OPEN_ERROR: %v\n", err)
		os.Exit(2)
	}

	screen, err := input.VirtualScreen()
	if err != nil {
		fmt.Printf("SCREEN_ERROR: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("virtual_screen left=%d top=%d width=%d height=%d\n",
		screen.Left, screen.Top, screen.Width, screen.Height)

	if !*move {
		fmt.Println("dry run: pass -move to nudge the pointer")
		return
	}

	origin, err := cursorPos()
	if err != nil {
		fmt.Printf("CURSOR_ERROR: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("cursor_before x=%d y=%d\n", origin.x, origin.y)

	target := point{x: clamp(origin.x+int32(*delta), screen.Left, screen.Left+screen.Width-1), y: origin.y}
	if err := moveTo(injector, screen, target); err != nil {
		fmt.Printf("MOVE_ERROR: %v\n", err)
		os.Exit(4)
	}
	landed, err := cursorPos()
	if err != nil {
		fmt.Printf("CURSOR_ERROR: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("requested x=%d y=%d landed x=%d y=%d offset_x=%d offset_y=%d\n",
		target.x, target.y, landed.x, landed.y, landed.x-target.x, landed.y-target.y)

	if err := moveTo(injector, screen, origin); err != nil {
		fmt.Printf("RESTORE_ERROR: %v\n", err)
		os.Exit(4)
	}
	restored, err := cursorPos()
	if err != nil {
		fmt.Printf("CURSOR_ERROR: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("cursor_after x=%d y=%d restored=%t\n", restored.x, restored.y, restored == origin)

	if landed != target || restored != origin {
		os.Exit(5)
	}
}

// moveTo aims at the centre of the pixel's cell, so the floor the package
// applies to the normalized value lands back on exactly that pixel.
func moveTo(injector *input.Injector, screen input.Bounds, p point) error {
	x := (float64(p.x-screen.Left) + 0.5) / float64(screen.Width)
	y := (float64(p.y-screen.Top) + 0.5) / float64(screen.Height)
	if err := injector.Inject(input.Event{V: input.Version, Kind: input.KindMouseMove, X: &x, Y: &y}); err != nil {
		return err
	}
	// The input queue is asynchronous; give it a moment before reading back.
	time.Sleep(50 * time.Millisecond)
	return nil
}

func clamp(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type point struct{ x, y int32 }

var procGetCursorPos = syscall.NewLazyDLL("user32.dll").NewProc("GetCursorPos")

func cursorPos() (point, error) {
	var p point
	ok, _, err := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	if ok == 0 {
		return point{}, fmt.Errorf("GetCursorPos: %v", err)
	}
	return p, nil
}
