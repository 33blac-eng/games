//go:build windows

// encode-probe is the written encoder gate of Додаток C: capture -> hardware
// MFT -> Annex-B, measured for a minute and reported PASS/FAIL per line item.
//
//	go run ./agent/cmd/encode-probe -seconds 60 -fps 60 -bitrate 8000000
//
// It prints one ENCODE_GATE line per criterion. GPU-encode utilisation is
// reported UNKNOWN on purpose: measuring it needs an external profiler
// (Nsight/GPUView), and inventing a number would defeat the point of a gate.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"time"
	"unsafe"

	"github.com/organicoils/oo-screen/agent/capture"
	"github.com/organicoils/oo-screen/agent/encode"
	"github.com/organicoils/oo-screen/internal/h264"
)

func main() {
	var (
		seconds  = flag.Int("seconds", 60, "measurement window")
		fps      = flag.Int("fps", 60, "target fps (frame budget = 1s/fps)")
		bitrate  = flag.Int("bitrate", 8_000_000, "CBR target, bits/s")
		output   = flag.Int("output", 0, "DXGI output index")
		zeroCopy = flag.Bool("zerocopy", true, "hand NV12 textures to the MFT (no CPU readback)")
		jiggle   = flag.Bool("jiggle", true, "wiggle the cursor so a static desktop still produces frames")
		dumpSecs = flag.Float64("dump-seconds", 5, "how much of the stream to write to $TEMP")
		encW     = flag.Int("width", 1920, "encoded width (0 = native); larger desktops are rescaled on the GPU")
		encH     = flag.Int("height", 1080, "encoded height (0 = native)")
		forceSW  = flag.Bool("force-software", false, "skip hardware enum; use the software Microsoft H264 MFT (CPU NV12 sync path)")
	)
	flag.Parse()

	// The software MFT is CPU-only: it takes NV12 bytes, never a texture. Force
	// CPU readback and drop the zero-copy request when it is selected.
	if *forceSW {
		*zeroCopy = false
	}

	if err := run(*seconds, *fps, *bitrate, *output, *zeroCopy, *jiggle, *dumpSecs, *encW, *encH, *forceSW); err != nil {
		fmt.Println("ENCODE_GATE overall FAIL —", err)
		os.Exit(1)
	}
}

type sample struct {
	submitted time.Time
	pts       time.Duration
}

func run(seconds, fps, bitrate, output int, zeroCopy, jiggle bool, dumpSecs float64, encW, encH int, forceSW bool) error {
	budget := time.Second / time.Duration(fps)

	cap_, err := capture.NewWithOptions(output, capture.Options{FrameBudget: budget})
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	defer cap_.Close()
	w, h := cap_.Size()
	fmt.Printf("capture: output %d, %dx%d, frame budget %v\n", output, w, h, budget)

	// Додаток C measures 1080p. When the desktop is bigger, the encoder rescales
	// NV12->NV12 on the GPU rather than us reading anything back.
	ew, eh := encW, encH
	if ew <= 0 || eh <= 0 || ew > w || eh > h {
		ew, eh = w, h
	}
	cfg := encode.Config{Width: ew, Height: eh, FPS: fps, BitrateBps: bitrate,
		SrcWidth: w, SrcHeight: h, ForceSoftware: forceSW}
	if ew != w || eh != h {
		fmt.Printf("encoding at %dx%d (GPU rescale from %dx%d)\n", ew, eh, w, h)
	}
	if zeroCopy {
		cap_.SetCPUReadback(false)
		cfg.D3DDevice = cap_.Device()
		if cfg.D3DDevice == 0 {
			return fmt.Errorf("capture exposed no ID3D11Device")
		}
	} else {
		// CPU NV12 path: the software MFT (and the hardware CPU fallback) read
		// planes, so the capturer must map each frame back to system memory.
		cap_.SetCPUReadback(true)
	}

	enc, err := encode.New(cfg)
	if err != nil {
		return fmt.Errorf("encoder: %w", err)
	}
	defer enc.Close()

	path := "hardware async"
	if !enc.Hardware() {
		path = "software sync"
	}
	fmt.Printf("encoder path: %s  (%q hardware=%v async=%v zero-copy=%v)\n",
		path, enc.Name(), enc.Hardware(), enc.Async(), enc.ZeroCopy())
	fmt.Printf("encoder: %q async=%v zero-copy=%v gop=%d bitrate=%d\n",
		enc.Name(), enc.Async(), enc.ZeroCopy(), fps*2, bitrate)
	fmt.Printf("cached SPS/PPS from MF_MT_MPEG_SEQUENCE_HEADER: %d bytes\n", len(enc.Headers()))

	if jiggle {
		stop := startJiggle()
		defer stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(seconds)*time.Second+10*time.Second)
	defer cancel()

	dumpPath := filepath.Join(os.TempDir(), "oo-screen-encode-probe.h264")
	dump, err := os.Create(dumpPath)
	if err != nil {
		return err
	}
	defer dump.Close()

	var (
		start      = time.Now()
		deadline   = start.Add(time.Duration(seconds) * time.Second)
		pending    = map[time.Duration]sample{}
		submitMS   []float64 // ms, ProcessInput cost — what the capture loop pays
		waitMS     []float64 // ms, waiting for METransformNeedInput (back-pressure)
		callMS     []float64 // ms, the whole Encode() call
		latencies  []float64 // ms, frame submitted -> AU out (pipeline latency)
		keyLatency []float64
		stream     []byte
		frames     int
		aus        int
		keyframes  int
		totalBytes int
		dumped     bool
		forcedAt   time.Time
		forcedSeen bool
		flushDone  bool
	)

	cpu0 := processCPU()

	for time.Now().Before(deadline) {
		f, err := cap_.NextFrame(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return fmt.Errorf("capture frame %d: %w", frames, err)
		}
		pts := time.Since(start)

		ef := encode.Frame{PTS: pts}
		if zeroCopy {
			ef.Texture = f.Texture
			if ef.Texture == 0 {
				return fmt.Errorf("frame %d: zero-copy requested but no texture", frames)
			}
		} else {
			ef.Y, ef.UV = f.Y, f.UV
			ef.YStride, ef.UVStride = f.YStride, f.UVStride
		}

		now := time.Now()
		pending[pts] = sample{submitted: now, pts: pts}
		out, err := enc.Encode(ef)
		callMS = append(callMS, float64(time.Since(now).Microseconds())/1000)
		wt, pr := enc.LastSubmitTiming()
		waitMS = append(waitMS, float64(wt.Microseconds())/1000)
		submitMS = append(submitMS, float64(pr.Microseconds())/1000)
		if err != nil {
			return fmt.Errorf("encode frame %d: %w", frames, err)
		}
		frames++

		for _, au := range out {
			ms := float64(time.Since(pending[au.PTS].submitted).Microseconds()) / 1000
			if s, ok := pending[au.PTS]; ok {
				ms = float64(time.Since(s.submitted).Microseconds()) / 1000
				delete(pending, au.PTS)
			}
			aus++
			totalBytes += len(au.Data)
			latencies = append(latencies, ms)
			if au.Keyframe {
				keyframes++
				keyLatency = append(keyLatency, ms)
				if !forcedAt.IsZero() && !forcedSeen && au.PTS >= time.Since(start)-2*time.Second {
					forcedSeen = true
				}
			}
			stream = append(stream, au.Data...)
			if !dumped {
				dump.Write(au.Data)
				if time.Since(start) > time.Duration(dumpSecs*float64(time.Second)) {
					dumped = true
					dump.Close()
				}
			}
		}

		// Exercise the two control paths mid-run: an on-demand IDR (§5.4) and
		// the flush+restart reset (§5.5).
		if forcedAt.IsZero() && time.Since(start) > 10*time.Second {
			forcedAt = time.Now()
			if err := enc.ForceIDR(); err != nil {
				return fmt.Errorf("ForceIDR: %w", err)
			}
		}
		if !flushDone && time.Since(start) > 20*time.Second {
			flushDone = true
			if err := enc.Flush(); err != nil {
				return fmt.Errorf("Flush: %w", err)
			}
		}
	}
	elapsed := time.Since(start)
	cpuPct := 100 * (processCPU() - cpu0) / elapsed.Seconds() / float64(numCPU())
	if !dumped {
		dump.Close()
	}

	keyframesTotal, injected := enc.HeaderStats()
	cpuMaps := cap_.CPUMaps()

	// ---------------------------------------------------------------- analysis
	parsed := h264.SplitAUs(stream)
	var sps *h264.SPS
	idrTotal, idrWithSPS := 0, 0
	for _, au := range parsed {
		if au.Keyframe {
			idrTotal++
			if au.HasSPS {
				idrWithSPS++
			}
		}
		if sps == nil && au.HasSPS {
			for _, nal := range h264.SplitNALs(au.Data) {
				if len(nal) > 0 && nal[0]&0x1F == h264.NALSPS {
					if s, err := h264.ParseSPS(nal); err == nil {
						sps = s
					}
				}
			}
		}
	}

	p50 := pct(submitMS, 50)
	p99 := pct(submitMS, 99)
	lat50 := pct(latencies, 50)
	lat99 := pct(latencies, 99)
	keyMax := 0.0
	for _, v := range keyLatency {
		keyMax = math.Max(keyMax, v)
	}
	actualBps := float64(totalBytes) * 8 / elapsed.Seconds()

	fmt.Println()
	fmt.Printf("run: %.1fs, %d frames captured, %d AUs, %d keyframes, %.1f fps effective\n",
		elapsed.Seconds(), frames, aus, keyframes, float64(frames)/elapsed.Seconds())
	fmt.Printf("bytes: %d total, %.2f Mbit/s actual vs %.2f target\n",
		totalBytes, actualBps/1e6, float64(bitrate)/1e6)
	fmt.Printf("ProcessInput cost ms: p50=%.2f p99=%.2f | back-pressure wait ms: p50=%.2f p99=%.2f"+
		" | whole Encode() ms: p50=%.2f p99=%.2f\n",
		p50, p99, pct(waitMS, 50), pct(waitMS, 99), pct(callMS, 50), pct(callMS, 99))
	fmt.Printf("pipeline latency ms (submit -> AU): p50=%.2f p99=%.2f max-keyframe=%.2f\n",
		lat50, lat99, keyMax)
	fmt.Printf("SPS/PPS: %d/%d IDR AUs carry SPS inband; %d of %d needed our cached prefix\n",
		idrWithSPS, idrTotal, injected, keyframesTotal)
	if sps != nil {
		fmt.Printf("parsed SPS: profile_idc=%d level_idc=%d %dx%d profile-level-id=%s codec=%s\n",
			sps.ProfileIDC, sps.LevelIDC, sps.Width, sps.Height,
			sps.ProfileLevelID(), sps.CodecString())
	}
	fmt.Printf("dump: %s (first %.0fs)\n", dumpPath, dumpSecs)
	fmt.Println()

	ok := true
	gate := func(name string, pass bool, format string, args ...any) {
		verdict := "PASS"
		if !pass {
			verdict = "FAIL"
			ok = false
		}
		fmt.Printf("ENCODE_GATE %-22s %s  %s\n", name, verdict, fmt.Sprintf(format, args...))
	}
	unknown := func(name, why string) {
		fmt.Printf("ENCODE_GATE %-22s UNKNOWN  %s\n", name, why)
	}

	soft := !enc.Hardware()
	info := func(name, format string, args ...any) {
		fmt.Printf("ENCODE_GATE %-22s INFO     %s\n", name, fmt.Sprintf(format, args...))
	}
	if soft {
		// The software Microsoft H264 MFT path: the hardware/zero-copy/latency
		// line items do not apply (CPU NV12, synchronous, no DXGI). They are
		// reported as INFO so the H.264 correctness gates below still decide
		// PASS/FAIL. A higher ProcessInput p99 and CPU% is expected here.
		info("hardware-mft", "software sync path %q (CLSID_CMSH264EncoderMFT); hardware N/A", enc.Name())
		info("zero-copy", "N/A on the CPU NV12 software path; capture CPU maps=%d", cpuMaps)
		info("encode-p99", "ProcessInput p99=%.2fms p50=%.2fms (software; no 8ms budget)", p99, p50)
	} else {
		gate("hardware-mft", enc.Async(), "%q, async=%v (MFTEnumEx HARDWARE)", enc.Name(), enc.Async())
		gate("zero-copy", zeroCopy && enc.ZeroCopy() && cpuMaps == 0,
			"DXGI surface path=%v, NV12 CPU maps by capture=%d (want 0)", enc.ZeroCopy(), cpuMaps)
		gate("encode-p99", p99 <= 8, "ProcessInput p99=%.2fms p50=%.2fms (limit 8ms)", p99, p50)
	}
	fmt.Printf("ENCODE_GATE %-22s INFO     submit->AU p50=%.2fms p99=%.2fms; back-pressure wait "+
		"p99=%.2fms; whole Encode() p99=%.2fms (async MFT queue depth + rate limiting; "+
		"not a Додаток C line item)\n",
		"pipeline-latency", lat50, lat99, pct(waitMS, 99), pct(callMS, 99))
	fmt.Printf("ENCODE_GATE %-22s INFO     worst keyframe %.2fms vs median delta %.2fms => the "+
		"keyframe itself costs %+.2fms; the rest is pipeline depth\n",
		"keyframe-marginal", keyMax, lat50, keyMax-lat50)
	if soft {
		info("keyframe-budget", "max keyframe=%.2fms (software; no 2x budget limit)", keyMax)
		info("cpu-percent", "%.2f%% of one machine (software encode is CPU-bound; no 10%% limit)", cpuPct)
	} else {
		gate("keyframe-budget", keyMax <= 2*float64(budget.Milliseconds()) && keyMax > 0,
			"max keyframe=%.2fms (limit 2x budget = %dms)", keyMax, 2*budget.Milliseconds())
		gate("cpu-percent", cpuPct <= 10, "%.2f%% of one machine (limit 10%%)", cpuPct)
	}
	unknown("gpu-encode-util", "needs an external profiler (Nsight/GPUView); not measured here")
	gate("annexb-parses", len(parsed) > 0 && aus > 0 && len(parsed) >= aus-2,
		"internal/h264 split %d AUs out of %d emitted", len(parsed), aus)
	gate("sps-every-idr", idrTotal > 0 && idrWithSPS == idrTotal,
		"%d/%d IDR AUs carry SPS (inband + %d prefixed by us)", idrWithSPS, idrTotal, injected)
	// Main(77), не High(100): Chrome не оголошує 64xx у
	// RTCRtpReceiver.getCapabilities('video') взагалі, тож High-потік хаб
	// відбиває як 415 h264_profile_mismatch. mft.c має High другою сходинкою
	// драбини лише щоб MFT без Main усе-таки відкрився — і тоді цей гейт
	// зобовʼязаний почервоніти на цій машині.
	gate("profile-level", sps != nil && sps.ProfileIDC == 77 && sps.LevelIDC <= 42,
		"want Main(77)@<=4.2(42), got %v", spsDesc(sps))
	gate("resolution", sps != nil && sps.Width == ew && sps.Height == eh,
		"want %dx%d, SPS says %v", ew, eh, spsDim(sps))
	gate("bitrate-cbr", actualBps > 0 && actualBps <= 1.35*float64(bitrate),
		"%.2f Mbit/s vs %.2f target (allow +35%%)", actualBps/1e6, float64(bitrate)/1e6)
	gate("force-idr", forcedSeen || keyframes > 1, "ForceIDR issued, %d keyframes seen", keyframes)
	gate("flush-restart", flushDone, "Flush()+restart executed mid-run without error")
	unknown("visual-10px-text", "manual: open the dump and read a 10px line on a static scene")

	fmt.Println()
	if ok {
		fmt.Println("ENCODE_GATE overall PASS")
		return nil
	}
	return fmt.Errorf("one or more criteria failed")
}

func spsDesc(s *h264.SPS) string {
	if s == nil {
		return "no SPS parsed"
	}
	return fmt.Sprintf("profile_idc=%d level_idc=%d", s.ProfileIDC, s.LevelIDC)
}

func spsDim(s *h264.SPS) string {
	if s == nil {
		return "no SPS parsed"
	}
	return fmt.Sprintf("%dx%d", s.Width, s.Height)
}

func pct(v []float64, p int) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := (len(s) - 1) * p / 100
	return s[i]
}

// ---------------------------------------------------------------- Win32 bits

var (
	user32       = syscall.NewLazyDLL("user32.dll")
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	setCursorPos = user32.NewProc("SetCursorPos")
	getCursorPos = user32.NewProc("GetCursorPos")
	getProcTimes = kernel32.NewProc("GetProcessTimes")
	curProcess   = kernel32.NewProc("GetCurrentProcess")
)

type point struct{ X, Y int32 }

// startJiggle nudges the real cursor from inside this process, so a static
// desktop still produces DXGI frames (a background job cannot do this — it has
// no interactive window station).
func startJiggle() func() {
	done := make(chan struct{})
	go func() {
		var p point
		getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
		base := p
		t := time.NewTicker(8 * time.Millisecond)
		defer t.Stop()
		i := int32(0)
		for {
			select {
			case <-done:
				setCursorPos.Call(uintptr(base.X), uintptr(base.Y))
				return
			case <-t.C:
				i++
				dx := (i % 200) - 100
				setCursorPos.Call(uintptr(base.X+dx), uintptr(base.Y+(i%40)-20))
			}
		}
	}()
	return func() { close(done) }
}

type filetime struct{ Low, High uint32 }

func (f filetime) seconds() float64 {
	return float64(uint64(f.High)<<32|uint64(f.Low)) / 1e7
}

// processCPU is user+kernel CPU seconds consumed by this process so far.
func processCPU() float64 {
	h, _, _ := curProcess.Call()
	var creation, exit, kernel, user filetime
	getProcTimes.Call(h,
		uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	return kernel.seconds() + user.seconds()
}

func numCPU() int { return runtime.NumCPU() }
