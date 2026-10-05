package main

import (
	"os"
	"strconv"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/webrtc/v4"
)

// NACK-генератор хаба (B6, продовження). Генератор працює лише на ВХІДНИХ
// медіапотоках, а єдиний вхідний медіапотік хаба — від агента: глядачі медіа
// не шлють. Отже ці параметри стосуються рівно ноги агент->хаб.
//
// Дефолт pion опитує дірки раз на 100 мс: ретрансмісія з агента приходить
// через 100 мс + RTT, кадр, що на неї чекає, стоїть > 200 мс — і з вікном
// SRTP 1024 при 5 % втрат лишалось 7–9 с/хв фризу. libwebrtc шле NACK
// одразу на дірку і повторює раз на RTT; 20 мс опиту — близько до того.
//
// Захист від NACK-шторму при частому опиті:
//   - nackMaxPerPkt: той самий seq просимо не більше N разів (pion лічить
//     спроби на seq). 10 x 20 мс = 200 мс спроб — далі кадр усе одно
//     рятує PLI (nack.go), а не сотий NACK;
//   - nackSkipLast: останні N seq не вважаються діркою — пакет, що просто
//     переставився в мережі, приїде сам за мілісекунду, і NACK на нього
//     був би даремним дублем.
//
// Env (порожньо = дефолт): OO_SCREEN_NACK_INTERVAL (Go duration),
// OO_SCREEN_NACK_MAX_PER_PKT, OO_SCREEN_NACK_SKIP_LAST.
var (
	nackInterval  = envDuration("OO_SCREEN_NACK_INTERVAL", 20*time.Millisecond)
	nackMaxPerPkt = envUint16("OO_SCREEN_NACK_MAX_PER_PKT", 10)
	nackSkipLast  = envUint16("OO_SCREEN_NACK_SKIP_LAST", 2)
)

func envUint16(k string, def uint16) uint16 {
	if v, err := strconv.ParseUint(os.Getenv(k), 10, 16); err == nil {
		return uint16(v)
	}
	return def
}

func nackGeneratorOptions() []nack.GeneratorOption {
	return []nack.GeneratorOption{
		nack.GeneratorInterval(nackInterval),
		nack.GeneratorMaxNacksPerPacket(nackMaxPerPkt),
		nack.GeneratorSkipLastN(nackSkipLast),
	}
}

// registerHubInterceptors — рівно RegisterDefaultInterceptors pion, але БЕЗ
// stats-interceptor (B7). Хаб ніде не кличе pc.GetStats() (RTT — з RR, див.
// rtt.go), а stats-interceptor на КОЖЕН пакет кожної ноги бере мʼютекс і
// time.Now() в обидва боки: на 1×16 це ~16 тис. зайвих записів за секунду
// просто в нікуди. NACK (з генератором вище), RTCP-звіти, simulcast-заголовки
// і TWCC — як були.
//
// OO_SCREEN_PION_STATS=1 повертає дефолтний набір (якщо колись знадобиться
// GetStats для діагностики) — з тим самим NACK-генератором.
func registerHubInterceptors(m *webrtc.MediaEngine, i *interceptor.Registry) error {
	if os.Getenv("OO_SCREEN_PION_STATS") == "1" {
		if err := webrtc.RegisterDefaultInterceptorsWithOptions(m, i,
			webrtc.WithNackGeneratorOptions(nackGeneratorOptions()...)); err != nil {
			return err
		}
		// Тут FEC зовні відносно TWCC: з узгодженим transport-cc розширенням
		// захищені байти не збіглися б із мережевими — діагностичний режим.
		addFECInterceptor(i)
		return nil
	}
	if err := webrtc.ConfigureNackWithOptions(m, i, nackGeneratorOptions()); err != nil {
		return err
	}
	if err := webrtc.ConfigureRTCPReports(i); err != nil {
		return err
	}
	// FEC (fec.go, OO_SCREEN_FEC): ЗОВНІ від NACK responder-а (той кешує вже
	// перенумеровані RED-пакети) і ВСЕРЕДИНІ від розширень заголовка (захищені
	// байти = мережеві). Порядок Add визначає вкладеність: останній — зовнішній.
	addFECInterceptor(i)
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		return err
	}
	return webrtc.ConfigureTWCCSender(m, i)
}
