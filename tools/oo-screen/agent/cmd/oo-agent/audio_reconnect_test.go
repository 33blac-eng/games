//go:build windows

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/agent/audio"
)

// fakeCapturer — джерело, що віддає рівно один валідний 20-мс пакет 48 кГц
// стерео float32 на кожен NextFrame, з монотонною міткою.
type fakeCapturer struct {
	mu       sync.Mutex
	n        int
	closedCh chan struct{} // якщо задано — Close сигналить сюди (без блокування)
}

func (c *fakeCapturer) Format() audio.Format {
	return audio.Format{SampleRate: 48000, Channels: 2, SampleFormat: audio.SampleFormatFloat,
		BitsPerSample: 32, ValidBitsPerSample: 32, BytesPerFrame: 8}
}

func (c *fakeCapturer) NextFrame(ctx context.Context) (*audio.Frame, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	// 960 семплів = 20 мс на 48 кГц, 2 канали по 4 байти.
	return &audio.Frame{
		Data:      make([]byte, 960*8),
		Format:    c.Format(),
		Timestamp: time.Unix(0, 0).Add(time.Duration(n) * 20 * time.Millisecond),
	}, nil
}

func (c *fakeCapturer) Close() error {
	if c.closedCh != nil {
		select {
		case c.closedCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// TestRunAudioSurvivesTransportSwap — інваріант, який досі жив ЛИШЕ в коментарі
// (main.go: «після реконекту звук піде в НОВУ доріжку без жодного власного
// механізму перепідключення»).
//
// Реконект з погляду runAudio виглядає РІВНО так: send у мертву доріжку віддає
// помилку. Якщо після цього горутина мовчки помирає — звук після кожного
// реконекту зникає до перезапуску агента, і в лозі про це немає ні рядка.
//
// 🔴 НЕГАТИВНИЙ КОНТРОЛЬ: замінити в audio.go тіло runAudio-циклу на
// `audioCapture(...); return` (тобто прибрати `for`) — тест ЧЕРВОНІЄ на
// «звук не повернувся в нову доріжку».
func TestRunAudioSurvivesTransportSwap(t *testing.T) {
	cap := &fakeCapturer{closedCh: make(chan struct{}, 1)}
	orig := audioOpen
	audioOpen = func() (audioCapturer, error) { return cap, nil }
	t.Cleanup(func() { audioOpen = orig })

	var (
		mu       sync.Mutex
		dead     = true // «стара доріжка мертва» — як під час дозвону
		afterNew int    // кадри, що доїхали ПІСЛЯ підміни транспорту
	)
	swapped := make(chan struct{})
	done := make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var paused atomic.Bool
	go func() {
		defer close(done)
		runAudio(ctx, &paused, func([]byte, time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			if dead {
				return errors.New("track closed") // реконект: транспорт помінявся під нами
			}
			afterNew++
			if afterNew == 5 {
				select {
				case <-swapped:
				default:
					close(swapped)
				}
			}
			return nil
		})
	}()

	// Дочекатись, поки горутина вперлась у мертву доріжку й закрила джерело,
	// потім «підняти» нову. Подія, а не сон: під навантаженням `go test ./...`
	// фіксовані 200 мс не встигали (R3-G6 ⚪2).
	select {
	case <-cap.closedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("джерело не закрилось після помилки send — audioCapture не вийшов")
	}
	mu.Lock()
	dead = false
	mu.Unlock()

	select {
	case <-swapped:
	case <-ctx.Done():
		mu.Lock()
		n := afterNew
		mu.Unlock()
		t.Fatalf("звук не повернувся в нову доріжку після реконекту: кадрів після підміни %d", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runAudio не завершився по ctx")
	}
}

// badFormatCapturer — пристрій із mix format, який агент не обслуговує.
type badFormatCapturer struct{ fakeCapturer }

func (c *badFormatCapturer) Format() audio.Format {
	return audio.Format{SampleRate: 48000, Channels: 2, SampleFormat: audio.SampleFormat(99), BitsPerSample: 32, BytesPerFrame: 8}
}

func (c *badFormatCapturer) NextFrame(ctx context.Context) (*audio.Frame, error) {
	f, err := c.fakeCapturer.NextFrame(ctx)
	if f != nil {
		f.Format = c.Format()
	}
	return f, err
}

// Непідтримуваний формат не має крутити open/close гарячим циклом: одна
// спроба, далі пауза audioReopenAfter. Прибери перевірку audioLayout після
// audioOpen у runAudio — відкриттів за 300 мс будуть сотні.
func TestRunAudioBadFormatBacksOff(t *testing.T) {
	var opens atomic.Int32
	orig := audioOpen
	audioOpen = func() (audioCapturer, error) { opens.Add(1); return &badFormatCapturer{}, nil }
	t.Cleanup(func() { audioOpen = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var paused atomic.Bool
	runAudio(ctx, &paused, func([]byte, time.Duration) error { return nil })
	if n := opens.Load(); n > 2 {
		t.Fatalf("за 300 мс джерело відкривалось %d разів — гарячий цикл на непідтримуваному форматі", n)
	}
}
