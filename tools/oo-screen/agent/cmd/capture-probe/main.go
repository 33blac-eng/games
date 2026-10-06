// capture-probe — proves the DXGI capture + BGRA->NV12 pipeline actually
// produces changing frames on this machine (Ф0 gate, plan §4).
//
//	capture-probe -n 300
//
// Every 60 frames it prints frame size, whether the cursor was composited, the
// SHA-256 of the first 64KB of the Y plane (it must CHANGE between samples —
// that is the proof the data is live and not a frozen buffer), and the running
// p50/p95 of AcquireNextFrame+convert.
//
// Exits 0 with "CAPTURE_OK frames=N" on success.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/organicoils/oo-screen/agent/capture"
)

const sampleEvery = 60

func main() {
	n := flag.Int("n", 300, "number of frames to capture")
	output := flag.Int("output", 0, "output (monitor) index on adapter 0")
	budget := flag.Duration("budget", 33*time.Millisecond, "AcquireNextFrame timeout (frame budget)")
	timeout := flag.Duration("timeout", 60*time.Second, "overall deadline")
	flag.Parse()

	if err := run(*n, *output, *budget, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "CAPTURE_FAIL %v\n", err)
		os.Exit(1)
	}
}

func run(n, output int, budget, timeout time.Duration) error {
	if cnt, err := capture.OutputCount(); err == nil {
		fmt.Printf("outputs=%d using=%d\n", cnt, output)
	}

	c, err := capture.NewWithOptions(output, capture.Options{FrameBudget: budget})
	if err != nil {
		if errors.Is(err, capture.ErrNotAvailable) {
			return fmt.Errorf("desktop duplication unavailable (RDP session / secure desktop / headless adapter): %w", err)
		}
		return err
	}
	defer c.Close()

	w, h := c.Size()
	fmt.Printf("opened output=%d size=%dx%d budget=%s\n", output, w, h, budget)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	durs := make([]time.Duration, 0, n)
	var lastHash string
	distinct := map[string]struct{}{}
	realFrames, mouseOnly := 0, 0
	start := time.Now()

	for i := 1; i <= n; i++ {
		f, err := c.NextFrame(ctx)
		if err != nil {
			return fmt.Errorf("frame %d/%d after %d captured: %w", i, n, len(durs), err)
		}
		durs = append(durs, f.AcquireConvert)
		if f.MouseOnly {
			mouseOnly++
		} else {
			realFrames++
		}

		sum := yHash(f)
		distinct[sum] = struct{}{}

		if i%sampleEvery == 0 || i == n {
			changed := "CHANGED"
			if sum == lastHash {
				changed = "same"
			}
			p50, p95 := pct(durs, 0.50), pct(durs, 0.95)
			fmt.Printf("frame=%-5d size=%dx%d stride=%d cursor=%v/%s@%d,%d composited=%v mouse_only=%v accum=%d y64k_sha256=%s..%s p50=%s p95=%s\n",
				i, f.Width, f.Height, f.YStride,
				f.CursorVisible, f.CursorShape, f.CursorX, f.CursorY,
				f.CursorComposited, f.MouseOnly, f.AccumulatedFrames,
				sum[:16], changed, rnd(p50), rnd(p95))
			lastHash = sum
		}
	}

	elapsed := time.Since(start)
	p50, p95 := pct(durs, 0.50), pct(durs, 0.95)
	fmt.Printf("\nframes=%d real=%d mouse_only=%d distinct_y64k_hashes=%d elapsed=%s effective_fps=%.1f\n",
		len(durs), realFrames, mouseOnly, len(distinct),
		rnd(elapsed), float64(len(durs))/elapsed.Seconds())
	fmt.Printf("acquire+convert p50=%s p95=%s min=%s max=%s\n",
		rnd(p50), rnd(p95), rnd(pct(durs, 0)), rnd(pct(durs, 1)))

	if len(distinct) < 2 {
		return fmt.Errorf("Y plane never changed across %d frames (%d distinct hashes) — capture looks frozen",
			len(durs), len(distinct))
	}
	if realFrames == 0 {
		return errors.New("no real desktop frame — every update was mouse-only")
	}
	fmt.Printf("CAPTURE_OK frames=%d\n", len(durs))
	return nil
}

// yHash hashes the first 64KB of the luma plane. Cheap, and enough to see the
// screen move.
func yHash(f *capture.NV12Frame) string {
	b := f.Y
	if len(b) > 64*1024 {
		b = b[:64*1024]
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(p * float64(len(s)-1))
	return s[i]
}

func rnd(d time.Duration) time.Duration { return d.Round(10 * time.Microsecond) }
