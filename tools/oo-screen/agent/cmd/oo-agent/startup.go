//go:build windows

package main

import (
	"context"
	"log"
	"time"
)

// retryUntil повторює open() з бек-офом, доки не вдасться або не скасують ctx.
//
// A-27: жодного log.Fatalf після розбору прапорців. onlogon-задача стріляє
// один раз; агент, що впав на старті (лок-скрін у момент входу, RDP-сесія,
// драйвер ще не піднявся, хаб на перевикочуванні), зникав із пульта до
// наступного входу людини — тобто на день. Бек-оф той самий, що в реконекті:
// парк, який одночасно піднявся після перезапуску хаба, не має стукати строєм.
func retryUntil[T any](ctx context.Context, what string, open func() (T, error)) (T, error) {
	backoff := reconnectBackoffMin
	for {
		v, err := open()
		if err == nil {
			return v, nil
		}
		wait := jitterBackoff(backoff)
		log.Printf("oo-agent: %s: %v — повтор через %s", what, err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(wait):
		}
		backoff = nextBackoff(backoff)
	}
}
