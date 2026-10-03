//go:build windows

// staticprobe — скільки НАСПРАВДІ коштує нерухомий екран.
//
// Питання, заради якого це написано: чи має сенс, щоб мережевий контролер
// хаба керував бітрейтом, поки на екрані нічого не рухається. Проба відтворює
// рівно те, що робить цикл oo-agent на статиці — один і той самий кадр раз на
// keepaliveAfter, PTS за стінним годинником — і показує в байтах, чого коштує
// одна ціль від хаба.
//
// Що вимірюється (1920x1080 з 2560x1440, NVIDIA H.264 Encoder MFT, цей ПК):
//
//	keepalive P-кадр нерухомого екрана       76-530 Б (~1.5 кбіт/с при 1 к/с)
//	IDR при цілі 8 Мбіт/с                    ~54-72 КБ
//	IDR при цілі 500 кбіт/с                  ~8 КБ
//
// Три висновки, які проба тримає відтворюваними:
//
//  1. ціль бітрейту НЕ впливає на keepalive-кадри (сотні байтів у будь-якому
//     разі) — на статиці ручка бітрейту не керує трафіком;
//  2. -idr-only: голий ForceIDR коштує стільки ж, скільки SetBitrate+ForceIDR.
//     Оскільки hub шле keyframe_request ОДРАЗУ за кожним bitrate_target
//     (sendBitrateTarget), відкладання цілі на статиці не економить НІЧОГО;
//  3. -idr-twice: два ForceIDR поспіль дають ОДИН IDR того ж розміру — власний
//     ForceIDR агента і keyframe_request хаба зливаються в один кадр.
//
// -dump складає IDR кожної фази як Annex-B: 8 Мбіт/с проти 500 кбіт/с на тому
// самому нерухомому екрані — 54 КБ проти 8 КБ, PSNR 36.5 дБ проти 25.7 дБ,
// текст у другому нечитний. Тобто на статиці ціль вирішує не смугу, а
// читабельність застиглої картинки.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/organicoils/oo-screen/agent/capture"
	"github.com/organicoils/oo-screen/agent/encode"
)

type opts struct {
	bitrate, low   int
	fps, frames    int
	period         time.Duration
	w, h           int
	software       bool
	idrOnly, twice bool
	dump           string
}

func main() {
	var o opts
	flag.IntVar(&o.bitrate, "bitrate", 8_000_000, "стартовий CBR, біт/с")
	flag.IntVar(&o.low, "low", 500_000, "ціль, яку імітуємо від хаба")
	flag.IntVar(&o.fps, "fps", 60, "номінальний fps енкодера")
	flag.IntVar(&o.frames, "frames", 4, "скільки keepalive у кожній фазі")
	flag.DurationVar(&o.period, "period", time.Second, "keepaliveAfter")
	flag.IntVar(&o.w, "w", 1920, "ширина кодування")
	flag.IntVar(&o.h, "h", 1080, "висота кодування")
	flag.BoolVar(&o.software, "sw", false, "софтверний MFT замість апаратного")
	flag.BoolVar(&o.idrOnly, "idr-only", false, "не міняти бітрейт — лише ForceIDR (ціна самого keyframe_request)")
	flag.BoolVar(&o.twice, "idr-twice", false, "два ForceIDR поспіль (applyBitrate + keyframe_request)")
	flag.StringVar(&o.dump, "dump", "", "куди складати IDR кожної фази (Annex-B)")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Println("STATICPROBE error:", err)
		os.Exit(1)
	}
}

func run(o opts) error {
	c, err := capture.New(0)
	if err != nil {
		return fmt.Errorf("capture.New: %w", err)
	}
	defer c.Close()
	srcW, srcH := c.Size()

	// Той самий порядок, що в агенті (openEncoder + applyReadback): софт-MFT
	// текстур не приймає і не масштабує, тому для нього — CPU-площини й рідна
	// геометрія; апаратному йде D3D-девайс капчера (zero-copy).
	dev, w, h := c.Device(), o.w, o.h
	if o.software {
		dev, w, h = 0, srcW, srcH
	}
	c.SetCPUReadback(o.software)

	enc, err := encode.New(encode.Config{
		Width: w, Height: h, FPS: o.fps, BitrateBps: o.bitrate,
		D3DDevice: dev, SrcWidth: srcW, SrcHeight: srcH, ForceSoftware: o.software,
	})
	if err != nil {
		return fmt.Errorf("encode.New: %w", err)
	}
	defer enc.Close()
	fmt.Printf("STATICPROBE src=%dx%d enc=%dx%d %q hw=%v zerocopy=%v\n",
		srcW, srcH, w, h, enc.Name(), enc.Hardware(), enc.ZeroCopy())

	// Знову як в агенті: спершу DXGI з дедлайном, а на справді нерухомому
	// екрані він не віддасть нічого — тоді GDI (shouldRearm).
	ctx, cancel := context.WithTimeout(context.Background(), o.period)
	frame, ferr := c.NextFrame(ctx)
	cancel()
	via := "DXGI"
	if ferr != nil {
		if frame, err = c.GDIFrame(); err != nil {
			return fmt.Errorf("GDIFrame (після %v): %w", ferr, err)
		}
		via = "GDI"
	}
	fmt.Printf("STATICPROBE first-frame=%s\n", via)

	var pts time.Duration
	phase := func(tag string) {
		for i := 0; i < o.frames; i++ {
			pts += o.period
			f := encode.Frame{Texture: frame.Texture, TextureGen: frame.TextureGen, PTS: pts}
			if o.software {
				f.Texture = 0
				f.Y, f.UV, f.YStride, f.UVStride = frame.Y, frame.UV, frame.YStride, frame.UVStride
			}
			aus, err := enc.Encode(f)
			if err != nil {
				fmt.Printf("STATICPROBE %s encode: %v\n", tag, err)
				continue
			}
			for _, au := range aus {
				fmt.Printf("STATICPROBE %-10s pts=%-6s bytes=%-7d key=%v\n",
					tag, au.PTS.Round(time.Millisecond), len(au.Data), au.Keyframe)
				if o.dump != "" && au.Keyframe {
					name := filepath.Join(o.dump, tag+".h264")
					if werr := os.WriteFile(name, au.Data, 0o644); werr != nil {
						fmt.Printf("STATICPROBE dump %s: %v\n", name, werr)
					} else {
						fmt.Printf("STATICPROBE dumped %s (%d B)\n", name, len(au.Data))
					}
				}
			}
		}
	}

	// applyBitrate агента — це SetBitrate + ForceIDR. -idr-only лишає сам
	// ForceIDR: так видно, що платимо ми за keyframe, а не за зміну цілі.
	step := func(tag string, bps int) error {
		if !o.idrOnly {
			if err := enc.SetBitrate(bps); err != nil {
				return fmt.Errorf("SetBitrate(%d): %w", bps, err)
			}
		}
		for i := 0; i < 1+btoi(o.twice); i++ {
			if err := enc.ForceIDR(); err != nil {
				return fmt.Errorf("ForceIDR: %w", err)
			}
		}
		fmt.Printf("STATICPROBE --- %s: ForceIDR x%d, bitrate=%d ---\n", tag, 1+btoi(o.twice), bps)
		phase(tag)
		return nil
	}

	phase("keepalive")
	if err := step("low", o.low); err != nil {
		return err
	}
	return step("high", o.bitrate)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
