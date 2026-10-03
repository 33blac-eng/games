package main

import (
	"os"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// registerHubInterceptors — рівно RegisterDefaultInterceptors pion, але БЕЗ
// stats-interceptor (B7). Хаб ніде не кличе pc.GetStats() (RTT — з RR, див.
// rtt.go), а stats-interceptor на КОЖЕН пакет кожної ноги бере мʼютекс і
// time.Now() в обидва боки: на 1×16 це ~16 тис. зайвих записів за секунду
// просто в нікуди. NACK, RTCP-звіти, simulcast-заголовки і TWCC — як були.
//
// OO_SCREEN_PION_STATS=1 повертає дефолтний набір (якщо колись знадобиться
// GetStats для діагностики).
func registerHubInterceptors(m *webrtc.MediaEngine, i *interceptor.Registry) error {
	if os.Getenv("OO_SCREEN_PION_STATS") == "1" {
		return webrtc.RegisterDefaultInterceptors(m, i)
	}
	if err := webrtc.ConfigureNack(m, i); err != nil {
		return err
	}
	if err := webrtc.ConfigureRTCPReports(i); err != nil {
		return err
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		return err
	}
	return webrtc.ConfigureTWCCSender(m, i)
}
