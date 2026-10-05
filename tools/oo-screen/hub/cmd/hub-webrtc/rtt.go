// RTT з Receiver Report — ДРУГИЙ сигнал контролера бітрейту, рівно під ту дірку,
// яку лишив вихід jitter-а з рішення (177a3e56): затримка росте, втрат немає.
//
// ЧОМУ З RR, А НЕ ЧЕРЕЗ pc.GetStats(). Обидва шляхи дають RTT, але:
//   - дані вже в руках. RR читає той самий sender.Read у viewer-нозі, який уже
//     тягне PLI й FractionLost; LSR/DLSR лежать у тому ж блоці, що й втрати.
//     GetStats() — це окрема горутина з тікером, окреме життя (зупинка при
//     dropViewer), окрема періодичність і окремий шлях у контролер;
//   - семпл на КОЖЕН RR, тобто рівно там і тоді, де вже ухвалюється рішення про
//     ціль: втрати й затримка приходять одним пакетом, одним now, одним локом.
//     Опитування GetStats() жило б у своєму ритмі й давало б розʼїзд між тим,
//     що бачив контролер по втратах, і тим, що по RTT;
//   - ціна — ~15 рядків NTP-арифметики нижче, бо pion тримає свою в internal/.
//
// ponytail: стеля — RTT тут рівно такої частоти, як RTCP (раз на кілька секунд).
// Для тренду цього досить, для швидкої реакції — ні; швидший шлях той самий, що
// й для втрат: TWCC з оцінювачем у агенті.
package main

import (
	"time"

	"github.com/pion/rtcp"
)

// ntpMiddle32 — «зараз» у середніх 32 бітах 64-бітної NTP-мітки (RFC 3550,
// §4): 16 молодших біт секунд + 16 старших біт дробової частини, тобто одиниця
// = 1/65536 с. Це РІВНО той формат, у якому приймач кладе LSR і DLSR у RR, тож
// віднімати можна безпосередньо. У pion така функція є (ToNTP32), але лежить у
// interceptor/internal/ntp — імпортувати не можна, тож свої 6 рядків.
func ntpMiddle32(t time.Time) uint32 {
	const epochOffset = 2_208_988_800 // секунд між 1900-01-01 і 1970-01-01
	ns := uint64(t.Unix()+epochOffset)*uint64(time.Second) + uint64(t.Nanosecond())
	sec := ns / uint64(time.Second)
	frac := ((ns % uint64(time.Second)) << 32) / uint64(time.Second)
	return uint32(sec<<16) | uint32(frac>>16)
}

// rttFromReport виводить RTT ноги з її ж Receiver Report: RTT = зараз − LSR −
// DLSR (RFC 3550, §6.4.1). SR, від якого приймач відлічує DLSR, хаб шле сам —
// його ставить sender-report interceptor з RegisterDefaultInterceptors.
//
// САНІТАРНИЙ КЛАМП. LSR і Delay приходять З МЕРЕЖІ, тобто це вхід від
// віддаленої сторони, і дурне значення не має ламати контролер:
//   - LSR == 0 — приймач ще не бачив жодного SR (так і документовано в
//     rtcp.ReceptionReport), виводити RTT нізвідки;
//   - різниця рахується в uint32 і сама переживає перехід через межу 16-бітних
//     секунд (раз на 18 год). Але якщо LSR+Delay «з майбутнього» (годинник
//     поїхав, поле підроблене), різниця обгорнеться у величезне число — його
//     ловить та сама верхня межа rttSaneMax, окремої перевірки не треба.
//
// ok == false означає «семпла немає»: викликач лишає попередній RTT ноги.
func rttFromReport(rr rtcp.ReceptionReport, now time.Time) (time.Duration, bool) {
	if rr.LastSenderReport == 0 {
		return 0, false
	}
	ticks := ntpMiddle32(now) - rr.LastSenderReport - rr.Delay
	d := time.Duration(ticks) * time.Second / 65536
	if d > rttSaneMax {
		return 0, false
	}
	return d, true
}
