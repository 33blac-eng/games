//go:build windows

package main

import (
	"sync"
	"testing"
)

// Основний формат WebRTC-control-каналу — типізований control.Msg. Тест веде
// повідомлення тим самим шляхом, що прод: handleCtlMessage -> onBitrateTarget
// -> clampBitrate (у main цей колбек далі кличе SetBitrate) і
// keyframe_request -> onKeyframeRequest (у main це ForceIDR).
func TestHandleCtlMessage(t *testing.T) {
	const start = 8_000_000

	// recorder ловить те, що дійшло до колбеків, і проганяє ціль крізь той
	// самий clamp, що й main.
	type got struct {
		clamped  []int
		idr      int
		gate     []bool
		rejected int
	}

	run := func(lines ...string) got {
		var g got
		onKeyframe := func() { g.idr++ }
		onGate := func(resume bool) { g.gate = append(g.gate, resume) }
		onBitrate := func(want uint64) {
			bps, ok := clampBitrate(want, start)
			if !ok {
				g.rejected++
				return
			}
			g.clamped = append(g.clamped, bps)
		}
		for _, l := range lines {
			handleCtlMessage([]byte(l), onKeyframe, onGate, onBitrate, nil)
		}
		return g
	}

	t.Run("bitrate_target доходить до clamp", func(t *testing.T) {
		g := run(`{"v":1,"type":"bitrate_target","bitrate_bps":4000000}` + "\n")
		if len(g.clamped) != 1 || g.clamped[0] != 4_000_000 {
			t.Fatalf("clamped = %v; хочемо [4000000]", g.clamped)
		}
	})

	t.Run("без термінатора теж доходить", func(t *testing.T) {
		// DataChannel message-oriented — кадр цілий і без '\n'.
		g := run(`{"v":1,"type":"bitrate_target","bitrate_bps":4000000}`)
		if len(g.clamped) != 1 || g.clamped[0] != 4_000_000 {
			t.Fatalf("clamped = %v; хочемо [4000000]", g.clamped)
		}
	})

	t.Run("ціль вище стелі притискається", func(t *testing.T) {
		g := run(`{"v":1,"type":"bitrate_target","bitrate_bps":50000000}`)
		if len(g.clamped) != 1 || g.clamped[0] != start {
			t.Fatalf("clamped = %v; хочемо [%d]", g.clamped, start)
		}
	})

	t.Run("keyframe_request кличе ForceIDR", func(t *testing.T) {
		g := run(`{"v":1,"type":"keyframe_request","seq":7}` + "\n")
		if g.idr != 1 {
			t.Fatalf("ForceIDR викликано %d разів; хочемо 1", g.idr)
		}
	})

	t.Run("resume/pause лишились робочими", func(t *testing.T) {
		g := run("pause", "resume")
		if len(g.gate) != 2 || g.gate[0] != false || g.gate[1] != true {
			t.Fatalf("gate = %v; хочемо [false true]", g.gate)
		}
	})

	t.Run("сміття не доходить нікуди", func(t *testing.T) {
		g := run(
			`{"v":1,"type":"bitrate_target"`,                          // обірваний JSON
			`{"v":1,"type":"bitrate_target","bitrate_bps":"багато"}`,  // тип поля не той
			`{"v":9,"type":"bitrate_target","bitrate_bps":4000000}`,   // чужа версія протоколу
			`{"v":1,"type":"чого-такого-нема","bitrate_bps":4000000}`, // невідомий тип
			`{}`, `{`, "", "хтозна-що",
		)
		if len(g.clamped) != 0 || g.idr != 0 || len(g.gate) != 0 || g.rejected != 0 {
			t.Fatalf("сміття пройшло: clamped=%v idr=%d gate=%v rejected=%d",
				g.clamped, g.idr, g.gate, g.rejected)
		}
	})

	t.Run("bitrate_target без bitrate_bps не доходить до енкодера", func(t *testing.T) {
		g := run(`{"v":1,"type":"bitrate_target"}`)
		if len(g.clamped) != 0 || g.rejected != 1 {
			t.Fatalf("clamped=%v rejected=%d; хочемо [] і 1 (clamp відкинув нуль)",
				g.clamped, g.rejected)
		}
	})

	t.Run("текстовий псевдонім ще працює", func(t *testing.T) {
		g := run("bitrate 4000000")
		if len(g.clamped) != 1 || g.clamped[0] != 4_000_000 {
			t.Fatalf("clamped = %v; хочемо [4000000]", g.clamped)
		}
	})
}

// Рядок із WebRTC-control-каналу приходить із мережі й лежить в одному потоці
// з "resume"/"pause". Тест тримає межу: до енкодера доходить лише коректне
// "bitrate <число>", решта — мовчки повз.
func TestParseBitrateLine(t *testing.T) {
	cases := []struct {
		line string
		bps  uint64
		ok   bool
	}{
		{"bitrate 4000000", 4_000_000, true},
		{"bitrate 300000", 300_000, true},
		{"bitrate", 0, false},                         // без аргументу
		{"bitrate ", 0, false},                        // пробіл є, числа нема
		{"bitrate abc", 0, false},                     // не число
		{"bitrate -5", 0, false},                      // ParseUint знак не приймає
		{"bitrate 4000000 ", 0, false},                // хвостовий пробіл — не наш формат
		{"bitrate  4000000", 0, false},                // подвійний пробіл
		{"bitrate4000000", 0, false},                  // без роздільника
		{"BITRATE 4000000", 0, false},                 // регістр має значення
		{"resume", 0, false},                          // сусіди по каналу не чіпаються
		{"pause", 0, false},                           //
		{"", 0, false},                                //
		{"bitrate 99999999999999999999999", 0, false}, // не влазить в uint64
	}

	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			bps, ok := parseBitrateLine(c.line)
			if ok != c.ok || bps != c.bps {
				t.Fatalf("parseBitrateLine(%q) = %d, %v; хочемо %d, %v",
					c.line, bps, ok, c.bps, c.ok)
			}
		})
	}
}

// Клампу довіряють дані з мережі: hub може прислати нуль, сміття або число,
// що не влазить в int32 енкодера. Тест тримає саме ці межі.
func TestClampBitrate(t *testing.T) {
	const start = 8_000_000

	cases := []struct {
		name  string
		want  uint64
		start int
		bps   int
		ok    bool
	}{
		{"у межах — як просили", 4_000_000, start, 4_000_000, true},
		{"нижче підлоги — підлога", 100_000, start, minBitrateBps, true},
		{"рівно підлога", minBitrateBps, start, minBitrateBps, true},
		{"вище стелі — стеля", 50_000_000, start, start, true},
		{"рівно стеля", start, start, start, true},
		{"величезне число не переповнює int", 1 << 62, start, start, true},
		{"нуль = поля нема, ігноруємо", 0, start, 0, false},
		{"стартовий бітрейт нижчий за підлогу — стеля виграє", 1_000_000, 100_000, 100_000, true},
		{"невалідний старт — ігноруємо", 4_000_000, 0, 0, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bps, ok := clampBitrate(c.want, c.start)
			if ok != c.ok || bps != c.bps {
				t.Fatalf("clampBitrate(%d, %d) = %d, %v; хочемо %d, %v",
					c.want, c.start, bps, ok, c.bps, c.ok)
			}
		})
	}
}

// На паузі енкодер смикати не можна: MFT приймає нову ціль надійно лише поки
// в нього йдуть кадри. Ціль має відкластись і віддатись рівно один раз.
func TestBitrateTargetDeferredWhilePaused(t *testing.T) {
	var b bitrateTarget

	if now := b.set(4_000_000, true); now != 0 {
		t.Fatalf("на паузі set повернув %d — енкодер смикнули б просто зараз", now)
	}
	// Остання ціль перебиває попередню: у черзі має сенс лише вона.
	if now := b.set(2_000_000, true); now != 0 {
		t.Fatalf("на паузі set повернув %d", now)
	}
	if got := b.take(); got != 2_000_000 {
		t.Fatalf("take = %d; хочемо 2000000 (остання відкладена ціль)", got)
	}
	if got := b.take(); got != 0 {
		t.Fatalf("take вдруге = %d; хочемо 0 — ціль застосовується рівно раз", got)
	}

	// Без паузи ціль іде в енкодер одразу й нічого не осідає в черзі.
	if now := b.set(6_000_000, false); now != 6_000_000 {
		t.Fatalf("без паузи set = %d; хочемо 6000000", now)
	}
	if got := b.take(); got != 0 {
		t.Fatalf("після негайного застосування take = %d; хочемо 0", got)
	}

	// set із control-горутини і take з циклу захоплення ходять паралельно —
	// -race має бути чистим.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			b.set(1_000_000, true)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			b.take()
		}
	}()
	wg.Wait()
}
