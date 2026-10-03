package main

import (
	"testing"
	"time"
)

// curDelay — єдина нетривіальна логіка, додана в реле, і саме вона тихо керує
// всім виміром: якщо вона збрехала, прогін виміряє не ту мережу, а помилки не
// буде видно НІДЕ. Тому один прогінний тест на всі гілки вікна й рампи.
func TestCurDelay(t *testing.T) {
	t0 := time.Now()
	r := &relay{delay: 300 * time.Millisecond, delayRamp: 20 * time.Second, delayFrom: t0.Add(10 * time.Second)}

	for _, c := range []struct {
		name string
		at   time.Duration
		want time.Duration
	}{
		{"до вікна — чисто", 5 * time.Second, 0},
		{"старт рампи", 10 * time.Second, 0},
		{"половина рампи", 20 * time.Second, 150 * time.Millisecond},
		{"кінець рампи", 30 * time.Second, 300 * time.Millisecond},
		{"плато після рампи", 60 * time.Second, 300 * time.Millisecond},
	} {
		if got := r.curDelay(t0.Add(c.at)); got != c.want {
			t.Errorf("%s: curDelay(+%v) = %v, хотіли %v", c.name, c.at, got, c.want)
		}
	}

	// Вікно закривається — черга розсмокталась.
	r.delayUntil = r.delayFrom.Add(40 * time.Second)
	if got := r.curDelay(t0.Add(60 * time.Second)); got != 0 {
		t.Errorf("після delayUntil: curDelay = %v, хотіли 0", got)
	}

	// Сходинка (ramp == 0) — повна затримка одразу.
	step := &relay{delay: 100 * time.Millisecond, delayFrom: t0}
	if got := step.curDelay(t0.Add(time.Second)); got != 100*time.Millisecond {
		t.Errorf("сходинка: curDelay = %v, хотіли 100ms", got)
	}

	// Коливання не має ні виносити затримку за ±амплітуду, ні йти в мінус:
	// негативна затримка означала б відправку пакета в минуле.
	jit := &relay{delay: 10 * time.Millisecond, delayJitter: 40 * time.Millisecond,
		jitterPeriod: 4 * time.Second, delayFrom: t0}
	var lo, hi time.Duration = time.Hour, 0
	for i := 0; i < 400; i++ {
		d := jit.curDelay(t0.Add(time.Duration(i) * 10 * time.Millisecond))
		if d < 0 {
			t.Fatalf("відʼємна затримка %v", d)
		}
		if d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	if hi < 45*time.Millisecond || hi > 50*time.Millisecond {
		t.Errorf("пік коливання %v — мав бути ~50ms (10 + 40)", hi)
	}
	if lo != 0 {
		t.Errorf("дно коливання %v — мало клампитись у 0 (10 − 40 < 0)", lo)
	}
}
