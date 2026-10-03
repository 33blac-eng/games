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
	mu     sync.Mutex
	n      int
	closed int
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

func (c *fakeCapturer) closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeCapturer) Close() error {
	c.mu.Lock()
	c.closed++
	c.mu.Unlock()
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
	cap := &fakeCapturer{}
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

	// Дати горутині впертись у мертву доріжку хоча б раз, потім «підняти» нову.
	time.Sleep(200 * time.Millisecond)
	if cap.closes() == 0 {
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
