package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

// stepFn — сигнатура чистого кроку контролера. Через неї ОДИН І ТОЙ САМИЙ набір
// перевірок ганяється двічі: на справжньому step() і на свідомо зламаних
// варіантах (негативний контроль). Гейт, який не вміє почервоніти, не гейт.
type stepFn func(bitrateCtl, float64, time.Duration, time.Time) (bitrateCtl, bool)

// checkAll проганяє весь набір і повертає список провалів (порожній = зелено).
func checkAll(step stepFn) []string {
	var fails []string
	add := func(format string, args ...any) { fails = append(fails, fmt.Sprintf(format, args...)) }

	cases := []struct {
		name    string
		in      bitrateCtl
		loss    float64
		now     time.Time
		wantTgt uint64
		wantSnd bool
	}{
		// --- втрати (єдиний сигнал контролера) ---
		{"cut-on-loss", bitrateCtl{target: 8_000_000, startBps: 8_000_000},
			0.03, t0, 5_600_000, true},
		{"hold-in-gray-zone", bitrateCtl{target: 8_000_000, startBps: 8_000_000},
			0.01, t0, 8_000_000, false},
		{"cut-clamped-to-floor", bitrateCtl{target: 600_000, startBps: 8_000_000},
			0.5, t0, 500_000, true},
		{"floor-is-sticky", bitrateCtl{target: 500_000, startBps: 8_000_000},
			0.5, t0, 500_000, false},

		// --- підйом: лише після 5с чистих ---
		{"no-up-before-streak", bitrateCtl{target: 4_000_000, startBps: 8_000_000, goodSince: t0},
			0, t0.Add(4 * time.Second), 4_000_000, false},
		{"up-after-streak", bitrateCtl{target: 4_000_000, startBps: 8_000_000, goodSince: t0},
			0, t0.Add(5 * time.Second), 4_200_000, true},
		{"up-clamped-to-session-start", bitrateCtl{target: 8_000_000, startBps: 8_000_000, goodSince: t0},
			0, t0.Add(5 * time.Second), 8_000_000, false},

		// --- асиметричний дебаунс: вниз 2с, вгору 10с ---
		{"down-debounced-under-2s", bitrateCtl{target: 8_000_000, startBps: 8_000_000, lastSent: t0},
			0.03, t0.Add(1 * time.Second), 8_000_000, false},
		{"down-after-2s", bitrateCtl{target: 8_000_000, startBps: 8_000_000, lastSent: t0},
			0.03, t0.Add(2 * time.Second), 5_600_000, true},
		{"up-debounced-under-10s", bitrateCtl{target: 4_000_000, startBps: 8_000_000, goodSince: t0, lastSent: t0},
			0, t0.Add(5 * time.Second), 4_000_000, false},
		{"up-after-10s", bitrateCtl{target: 4_000_000, startBps: 8_000_000, goodSince: t0, lastSent: t0},
			0, t0.Add(10 * time.Second), 4_200_000, true},
	}
	for _, c := range cases {
		// rttExcess = 0: цей блок доводить, що поведінка ПО ВТРАТАХ не змінилась
		// від появи другого сигналу. Тренд RTT перевіряється нижче, окремо.
		got, send := step(c.in, c.loss, 0, c.now)
		if got.target != c.wantTgt || send != c.wantSnd {
			add("%s: маємо target=%d send=%v, чекали target=%d send=%v",
				c.name, got.target, send, c.wantTgt, c.wantSnd)
		}
	}

	// --- послідовність 1: чистий канал 30с, RR раз на секунду ---
	// Доводить, що підйом іде НЕ частіше ніж раз на upDebounce (10с).
	c := bitrateCtl{target: 4_000_000, startBps: 8_000_000}
	var ups []time.Duration
	for i := 1; i <= 30; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		next, send := step(c, 0, 0, now)
		c = next
		if send {
			ups = append(ups, now.Sub(t0))
		}
	}
	if len(ups) < 2 {
		add("seq-clean: підйомів %d за 30с чистих — контролер вгору не йде взагалі", len(ups))
	}
	for i := 1; i < len(ups); i++ {
		if gap := ups[i] - ups[i-1]; gap < upDebounce {
			add("seq-clean: підйом частіше за upDebounce: %v після %v (моменти %v)", gap, ups[i-1], ups)
		}
	}

	// --- послідовність 2: сіра зона посеред серії скидає відлік 5с ---
	c = bitrateCtl{target: 4_000_000, startBps: 8_000_000}
	var first time.Duration
	n := 0
	for i := 1; i <= 15; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		loss := 0.0
		if i == 4 {
			loss = 0.01 // сіра зона: не ріже, але серію "чисто" обриває
		}
		next, send := step(c, loss, 0, now)
		c = next
		if send {
			if n == 0 {
				first = now.Sub(t0)
			}
			n++
		}
	}
	if n != 1 || first != 10*time.Second {
		add("seq-reset: підйомів %d, перший на %v; чекали рівно 1 на 10s (серія рахується від 5-ї секунди)", n, first)
	}
	return append(fails, checkRTT(step)...)
}

// --- ГЕЙТИ RTT: bufferbloat БЕЗ втрат ---
//
// Дірка, яку закриваємо: після виходу jitter-а з рішення контролер по втратах
// сліпий до глибокого буфера — затримка росте, FractionLost нуль. УСІ сценарії
// нижче — на НУЛЬОВИХ втратах, інакше доводили б адаптацію по втратах; частина
// з них узята просто з логів живих прогонів із керованою затримкою.
//
// runRTT проганяє серію RR (раз на секунду, loss=0) із заданим приростом RTT на
// кожному кроці й повертає найнижчу та найвищу ціль за прогін.
func runRTT(step stepFn, start uint64, excess []time.Duration) (lo, hi uint64) {
	lo, hi, _ = runRTTStat(step, start, excess)
	return lo, hi
}

// runRTTStat — те саме, але ще й рахує ЗРІЗИ (кроки вниз). Кількість зрізів —
// головна нова величина: після появи плеча по рівню важливо не «поїхала ціль чи
// ні», а СКІЛЬКИ разів вона поїхала (один зріз на епізод проти з'їзду в підлогу).
func runRTTStat(step stepFn, start uint64, excess []time.Duration) (lo, hi uint64, downs int) {
	c := bitrateCtl{target: start, startBps: 8_000_000}
	lo, hi = start, start
	for i, e := range excess {
		prev := c.target
		c, _ = step(c, 0, e, t0.Add(time.Duration(i+1)*time.Second))
		if c.target < prev {
			downs++
		}
		if c.target < lo {
			lo = c.target
		}
		if c.target > hi {
			hi = c.target
		}
	}
	return lo, hi, downs
}

// runRTTEnd — те саме, але віддає КІНЦЕВУ ціль. Гейти блокування підйому
// питають не «чи рухалась ціль», а «де вона зупинилась»: і «стоїть, поки стоїть
// черга», і «відновилась, коли черга зникла» — це саме про кінцеву точку.
func runRTTEnd(step stepFn, start uint64, excess []time.Duration) uint64 {
	c := bitrateCtl{target: start, startBps: 8_000_000}
	for i, e := range excess {
		c, _ = step(c, 0, e, t0.Add(time.Duration(i+1)*time.Second))
	}
	return c.target
}

// wobble — синусоїда приросту: mid ± amp із періодом period за семпл-секунду.
// Модель звичайного Wi-Fi, на якій заміряно хибне спрацювання старого порога.
func wobble(n int, mid, amp time.Duration, period float64) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		v := float64(mid) + float64(amp)*math.Sin(2*math.Pi*float64(i)/period)
		if v < 0 {
			v = 0
		}
		out[i] = time.Duration(v)
	}
	return out
}

// ramp — приріст, що росте на step за семпл; flat — сталий; fall — спадає.
func ramp(n int, step time.Duration) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = time.Duration(i) * step
	}
	return out
}

func flat(n int, v time.Duration) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func fall(n int, from, step time.Duration) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		if d := from - time.Duration(i)*step; d > 0 {
			out[i] = d
		}
	}
	return out
}

func checkRTT(step stepFn) []string {
	var fails []string
	add := func(format string, args ...any) { fails = append(fails, fmt.Sprintf(format, args...)) }

	// 1. RTT РОСТЕ, втрат нема -> ціль мусить поїхати вниз. Це і є bufferbloat.
	if lo, _ := runRTT(step, 8_000_000, ramp(20, 80*time.Millisecond)); lo >= 8_000_000 {
		add("rtt-rise: приріст RTT росте на 80мс за семпл при НУЛЬОВИХ втратах, а ціль лишилась %d — bufferbloat досі невидимий", lo)
	}

	// 2. Приріст стабільно ВИСОКИЙ (300мс над власною базою ноги) — це вже не
	// відстань, це черга, тож один зріз заслужений. Але РІВНО ОДИН: далі
	// спрацьовує засувка rttLevel, і безперервного тиску вниз бути не має.
	lo, _, downs := runRTTStat(step, 4_000_000, flat(60, 300*time.Millisecond))
	if downs != 1 {
		add("rtt-level-latch: сталий приріст 300мс дав %d зріз(ів), чекали рівно 1 (ціль сповзла до %d) — засувка не тримає, глядач їде в підлогу", downs, lo)
	}

	// 2b. ЗАМІРЯНА ХИБА (RESULTS-thresholds.md): зріз разовий, а дозвіл на підйом
	// висів лише на втратах, тож ціль повзла назад до стелі, ХОЧ ЧЕРГА СТОЯЛА —
	// ramp5 дав 6.8 -> 8 Мбіт/с за 42 с при прирості 294 мс. Тепер ціль мусить
	// зупинитись рівно на одному зрізі (8.0 * 0.85 = 6.8 Мбіт/с) і там СТОЯТИ.
	if held := runRTTEnd(step, 8_000_000, flat(60, 300*time.Millisecond)); held != 6_800_000 {
		add("rtt-flat-hold: приріст 300мс стоїть цілу хвилину, а ціль зупинилась на %d замість 6800000 — черга ціль більше не стримує", held)
	}

	// 2c. ЗВОРОТНИЙ БІК того самого блокування, і саме він тут головна пастка:
	// щойно черга розсмокталась, ціль мусить ВІДНОВИТИСЬ до стелі. Без цієї
	// перевірки "не піднімати при високому прирості" тихо перетворюється на
	// "не піднімати ніколи" — рівно та хиба, за яку з рішення прибрали jitter.
	back := runRTTEnd(step, 8_000_000, append(flat(60, 300*time.Millisecond), flat(100, 0)...))
	if back != 8_000_000 {
		add("rtt-flat-recover: черга зникла 100с тому, а ціль лишилась на %d, не на стелі 8000000 — блокування підйому залипло", back)
	}

	// 2a. Приріст сталий, але НИЖЧЕ порога шуму — далекий глядач або звичайне
	// коливання каналу. Жодного зрізу бути не має.
	if _, hiFar, downsFar := runRTTStat(step, 4_000_000, flat(60, 100*time.Millisecond)); downsFar != 0 || hiFar <= 4_000_000 {
		add("rtt-distant: сталий приріст 100мс (нижче порога шуму) дав %d зрізів, стеля %d — далекого глядача карають за відстань", downsFar, hiFar)
	}

	// 3. RTT ПАДАЄ -> підйом дозволено. Горизонт тут 120с, а не 60: відколи
	// приріст блокує підйом, перші ~26с цієї рампи ціль стоїть законно (приріст
	// ще вище за rttUpClear), і на 60с вона просто не встигала б відіграти зріз.
	// Твердження гейта від цього не змінилось — спадання підйому не заважає.
	if _, hi := runRTT(step, 4_000_000, fall(120, 400*time.Millisecond, 10*time.Millisecond)); hi <= 4_000_000 {
		add("rtt-fall: RTT спадає, втрат нема, а ціль стоїть на %d — підйому немає", hi)
	}

	// 4. ЗАМІРЯНА ХИБА, яку закриваємо: коливання 100мс ±40мс з періодом 4с на
	// абсолютно чистому каналі клало 8 Мбіт/с на підлогу 500 кбіт/с при
	// lossPct 0.00. Приріст над minRTT там гуляє в межах 0-80мс, тобто нижче
	// порога шуму — жодного зрізу бути не має.
	if _, _, downs := runRTTStat(step, 8_000_000, wobble(60, 40*time.Millisecond, 40*time.Millisecond, 4)); downs != 0 {
		add("rtt-wobble: звичайне коливання Wi-Fi (±40мс, період 4с) дало %d зріз(ів) при нульових втратах — межа «шум/затор» знову проходить посеред здорового каналу", downs)
	}

	// 5. ЗАМІРЯНА СЛІПА ЗОНА: повільна рампа 5мс/с до ~300мс і НУЛЬ втрат не
	// давала ЖОДНОГО зрізу — тренд її не бачить у принципі, бо жодне вікно не
	// набирає rttRiseStep. Ловить її саме плече по рівню.
	if _, _, downs := runRTTStat(step, 8_000_000, ramp(70, 5*time.Millisecond)); downs == 0 {
		add("rtt-slow-ramp: рампа 5мс/с до 350мс при нульових втратах не дала жодного зрізу — глибокий буфер досі невидимий")
	}

	// 6. Глибина кроку: затримка ріже мʼякше за втрати. Заміряно, що з 0.7 ціль
	// летіла в підлогу за ~12с — набагато глибше, ніж треба черзі.
	byRTT, _ := step(bitrateCtl{target: 8_000_000, startBps: 8_000_000}, 0, 300*time.Millisecond, t0)
	if byRTT.target != 6_800_000 {
		add("rtt-step-depth: зріз по затримці дав %d, чекали 6800000 (крок 0.85) — затримку ріжуть як втрати", byRTT.target)
	}
	return fails
}

func TestBitrateCtlStep(t *testing.T) {
	for _, f := range checkAll(bitrateCtl.step) {
		t.Error(f)
	}
}

// --- НЕГАТИВНИЙ КОНТРОЛЬ ---
// Доводимо, що набір перевірок уміє почервоніти: ті самі перевірки на свідомо
// зламаних кроках МУСЯТЬ впасти. Якщо не падають — гейт нічого не ловить.

// brokenDeaf — контролер, глухий до RR: ціль ніколи не міняється.
func brokenDeaf(c bitrateCtl, _ float64, _ time.Duration, _ time.Time) (bitrateCtl, bool) {
	return c, false
}

// brokenNoUpDebounce — справжня логіка, але БЕЗ 10-секундного гальма на підйом.
// Має провалити саме перевірку частоти підйомів.
func brokenNoUpDebounce(c bitrateCtl, loss float64, rttExcess time.Duration, now time.Time) (bitrateCtl, bool) {
	saved := c.lastSent
	c.lastSent = time.Time{}
	next, send := c.step(loss, rttExcess, now)
	if !send {
		next.lastSent = saved
	}
	return next, send
}

// brokenNoLevelArm — справжня логіка, але БЕЗ плеча по рівню: приріст
// підрізається так, щоб rttLevelHigh не досягався ніколи, а тренд лишався як є.
// Має провалити саме гейт повільної рампи — тієї, до якої тренд сліпий.
func brokenNoLevelArm(c bitrateCtl, loss float64, rttExcess time.Duration, now time.Time) (bitrateCtl, bool) {
	if rttExcess >= rttLevelHigh {
		rttExcess = rttLevelHigh - time.Millisecond
	}
	return c.step(loss, rttExcess, now)
}

// brokenOversensitive — модель ЗАНИЗЬКОГО порога «шум/затор»: будь-який
// ненульовий приріст читається як затор. Має провалити саме гейти коливання й
// далекого глядача — ті самі два випадки, на яких хиба спостерігалась живцем.
func brokenOversensitive(c bitrateCtl, loss float64, rttExcess time.Duration, now time.Time) (bitrateCtl, bool) {
	if rttExcess > 0 {
		rttExcess = rttLevelHigh
	}
	return c.step(loss, rttExcess, now)
}

// hasFail — чи є серед провалів той, чия назва містить key.
func hasFail(fails []string, key string) bool {
	for _, f := range fails {
		if strings.Contains(f, key) {
			return true
		}
	}
	return false
}

func TestBitrateCtlStepNegativeControl(t *testing.T) {
	if fails := checkAll(brokenDeaf); len(fails) == 0 {
		t.Fatal("негативний контроль: глухий контролер пройшов перевірки — перевірки нічого не ловлять")
	}
	// Плече по рівню вимкнене -> повільна рампа знову невидима.
	if fails := checkAll(brokenNoLevelArm); !hasFail(fails, "rtt-slow-ramp") {
		t.Fatalf("негативний контроль: без плеча по рівню гейт повільної рампи лишився зеленим; провали: %v", fails)
	}
	// Поріг занизький -> здоровий канал знову їде вниз.
	fails := checkAll(brokenOversensitive)
	if !hasFail(fails, "rtt-wobble") || !hasFail(fails, "rtt-distant") {
		t.Fatalf("негативний контроль: занизький поріг пройшов гейти коливання/далекого глядача; провали: %v", fails)
	}
	fails = checkAll(brokenNoUpDebounce)
	if len(fails) == 0 {
		t.Fatal("негативний контроль: підйом без 10с-дебаунсу пройшов перевірки")
	}
	found := false
	for _, f := range fails {
		if strings.Contains(f, "частіше за upDebounce") || strings.Contains(f, "seq-reset") {
			found = true
		}
	}
	if !found {
		t.Fatalf("негативний контроль: зламали саме гальмо підйому, а провали інші: %v", fails)
	}
}

// --- ГЕЙТ: НЕРУХОМИЙ ЕКРАН ---
//
// Той самий потік RR, на якому живий прогін зловив хибне спрацювання: втрат
// НУЛЬ ("lossPct":0.00 у кожному рядку), а jitter стоїть високо весь час, бо
// міжкадровий інтервал стрибнув 33 мс -> 1 с (keepalive нерухомого екрана).
// Це не затор, а інша частота кадрів — тож ціль не має ані їхати вниз, ані
// залипати внизу.

// stillScreenJitter — виміряно живцем в агенті (коментар до sampleDuration у
// agent/cmd/oo-agent/main.go): 10002 такти@90k, тобто вище колишнього порога
// 4500, і воно НЕ спадає, поки екран нерухомий. Це стале зміщення, а не сплеск.
const stillScreenJitter = 10002

// rrStep — сигнатура "крок по одному RR" разом із jitter: нинішній контролер
// його ігнорує, прибране правило — ні. Через неї той самий потік ганяється
// крізь обидва.
type rrStep func(bitrateCtl, float64, uint32, time.Time) (bitrateCtl, bool)

// nowStep — нинішнє правило: jitter приходить у телеметрію, а в рішення не йде.
func nowStep(c bitrateCtl, lossFrac float64, _ uint32, now time.Time) (bitrateCtl, bool) {
	return c.step(lossFrac, 0, now)
}

// legacyStep — ПРИБРАНЕ правило, виражене через нинішні входи: витриманий
// jitter вище 4500 діяв рівно як втрати вище порога (ріже ціль), а вище 1800 —
// як сіра зона (тримає й збиває серію "чисто", тобто блокує підйом). Витримки
// (колишній jitterHold) відтворювати не треба: на нерухомому екрані jitter
// високий БЕЗПЕРЕРВНО, тож будь-яка витримка минала. Живе лише в тесті й лише
// як негативний контроль.
func legacyStep(c bitrateCtl, lossFrac float64, jitterTicks uint32, now time.Time) (bitrateCtl, bool) {
	switch {
	case jitterTicks > 4500 && lossFrac <= lossHighFrac:
		lossFrac = lossHighFrac + 0.01
	case jitterTicks > 1800 && lossFrac <= lossLowFrac:
		lossFrac = (lossLowFrac + lossHighFrac) / 2
	}
	return c.step(lossFrac, 0, now)
}

// runStill проганяє secs звітів нерухомого екрана (RR раз на секунду) від
// стартової цілі й повертає найнижчу та найвищу ціль за весь прогін.
func runStill(start uint64, secs int, step rrStep) (lo, hi uint64) {
	c := bitrateCtl{target: start, startBps: 8_000_000}
	lo, hi = start, start
	for i := 1; i <= secs; i++ {
		c, _ = step(c, 0, stillScreenJitter, t0.Add(time.Duration(i)*time.Second))
		if c.target < lo {
			lo = c.target
		}
		if c.target > hi {
			hi = c.target
		}
	}
	return lo, hi
}

func TestStillScreenDoesNotRatchet(t *testing.T) {
	if lo, _ := runStill(8_000_000, 60, nowStep); lo != 8_000_000 {
		t.Errorf("нерухомий екран збив ціль до %d при НУЛЬОВИХ втратах — ратчет живий", lo)
	}
	// Друга половина доказу: ціль ВІДНОВЛЮЄТЬСЯ. Якби jitter прибрали лише з
	// гілки зниження, він і далі блокував би підйом — ціль залипла б унизу
	// назавжди, а це гірше за вихідний стан.
	if _, hi := runStill(5_600_000, 120, nowStep); hi != 8_000_000 {
		t.Errorf("після ратчета ціль доповзла лише до %d, а не до стелі 8000000", hi)
	}
}

// НЕГАТИВНИЙ КОНТРОЛЬ: той самий потік крізь ПРИБРАНЕ правило мусить і зрізати
// ціль, і не дати їй повернутись. Не впаде — значить потік не відтворює баг, і
// зелений тест вище нічого не доводить.
func TestStillScreenNegativeControl(t *testing.T) {
	if lo, _ := runStill(8_000_000, 60, legacyStep); lo >= 8_000_000 {
		t.Fatalf("негативний контроль: старе правило не зрізало ціль (мінімум %d) — потік не відтворює хибне спрацювання", lo)
	}
	if _, hi := runStill(5_600_000, 120, legacyStep); hi > 5_600_000 {
		t.Fatalf("негативний контроль: старе правило пустило ціль угору до %d — блокування підйому не відтворено", hi)
	}
}

// TestRealLossStillDrivesDown — доведене живим прогоном (8 -> 0.66 Мбіт/с і
// назад ×1.05 раз на 10 с) мусить лишитись: прибирали jitter, а не втрати.
func TestRealLossStillDrivesDown(t *testing.T) {
	c := bitrateCtl{target: 8_000_000, startBps: 8_000_000}
	for i := 1; i <= 60; i++ {
		c, _ = c.step(0.30, 0, t0.Add(time.Duration(i)*time.Second))
	}
	if c.target != minBitrateBps {
		t.Fatalf("30%% втрат 60 с: ціль %d, чекали підлогу %d", c.target, minBitrateBps)
	}
	base := t0.Add(60 * time.Second)
	ups := 0
	for i := 1; i <= 120; i++ {
		var send bool
		c, send = c.step(0, 0, base.Add(time.Duration(i)*time.Second))
		if send {
			ups++
		}
	}
	if c.target <= minBitrateBps {
		t.Fatalf("канал очистився, а ціль лишилась на %d — підйому немає", c.target)
	}
	if ups > 12 {
		t.Fatalf("підйомів %d за 120 с — частіше, ніж раз на upDebounce (10 с)", ups)
	}
}

// --- стеля з offer агента (поле "bitrate") ---

// Найкрихкіше місце всієї проводки — json-тег: помилишся в ньому, і hub мовчки
// поїде на фолбеку, ніде не впавши.
func TestOfferReqDecodesBitrate(t *testing.T) {
	var req offerReq
	if err := json.Unmarshal([]byte(`{"sdp":"x","token":"t","node":"n","bitrate":4000000}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Bitrate != 4_000_000 {
		t.Errorf("offerReq.Bitrate = %d, чекали 4000000", req.Bitrate)
	}
	var old offerReq
	if err := json.Unmarshal([]byte(`{"sdp":"x","token":"t","node":"n"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Bitrate != 0 {
		t.Errorf("старий offer без поля: Bitrate = %d, чекали 0", old.Bitrate)
	}
}

// climbPeak садить ноду на підлогу й 20 хвилин годує її ЧИСТИМИ RR, повертаючи
// найвищу ціль, до якої вона доповзла. 1200с при кроці +5% раз на 10с — це
// x1.05^120 від 500 кбіт/с, тобто впертись у стелю встигне будь-яка з
// перевірюваних. agentCtrl нульовий, тож відправка — тихий no-op: перевіряємо
// саме рішення контролера, без мережі.
func climbPeak(ns *nodeSession) uint64 {
	ns.mu.Lock()
	if ns.bitrate.startBps == 0 {
		ns.bitrate = newBitrateCtl(ns.ceilingBps())
	}
	ns.bitrate.target = minBitrateBps
	ns.mu.Unlock()

	peak := uint64(0)
	for i := 1; i <= 1200; i++ {
		onReceiverReport(ns, 0, 0, 0, t0.Add(time.Duration(i)*time.Second))
		ns.mu.Lock()
		if ns.bitrate.target > peak {
			peak = ns.bitrate.target
		}
		ns.mu.Unlock()
	}
	return peak
}

func TestOfferBitrateIsCeiling(t *testing.T) {
	cases := []struct {
		name     string
		offerBps uint64 // 0 = агент поля "bitrate" не прислав (старий агент)
		wantCeil uint64
	}{
		{"offer bitrate=4Mbps -> стеля 4Mbps", 4_000_000, 4_000_000},
		{"поля немає -> фолбек startBitrateBps", 0, startBitrateBps},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := &nodeSession{nodeID: "n"}
			if tc.offerBps > 0 {
				setStartBitrate(ns, tc.offerBps)
			}
			ns.mu.Lock()
			gotCeil := ns.ceilingBps()
			ns.mu.Unlock()
			if gotCeil != tc.wantCeil {
				t.Errorf("ceilingBps = %d, чекали %d", gotCeil, tc.wantCeil)
			}
			// І головне: контролер справді НЕ підіймається вище стелі.
			if peak := climbPeak(ns); peak != tc.wantCeil {
				t.Errorf("пік цілі = %d, чекали рівно %d (нижче — не доповз, вище — стеля не тримає)", peak, tc.wantCeil)
			}
		})
	}
}

// НЕГАТИВНИЙ КОНТРОЛЬ: hub, який ІГНОРУЄ поле "bitrate" з offer (тобто
// setStartBitrate не викликано), мусить провалити ту саму перевірку — інакше
// вона доводила б не підхоплення поля, а збіг із фолбеком.
func TestOfferBitrateIsCeilingNegativeControl(t *testing.T) {
	if startBitrateBps == 4_000_000 {
		t.Skip("OO_SCREEN_START_BITRATE збігається з тестовим offer — контроль безглуздий")
	}
	ns := &nodeSession{nodeID: "n"} // offer нібито ніс 4 Мбіт/с, hub його "загубив"
	if peak := climbPeak(ns); peak == 4_000_000 {
		t.Fatal("негативний контроль: стеля 4 Мбіт/с вийшла БЕЗ підхоплення поля з offer — перевірка нічого не доводить")
	}
}

// --- НЕГАТИВНИЙ КОНТРОЛЬ ДО ГЕЙТІВ RTT ---
//
// Обидва зламані кроки — це два способи зробити RTT неправильно, по одному на
// кожен гейт. Якщо котрийсь пройде свій гейт зеленим, гейт нічого не доводить.

// brokenRTTBlind — контролер, ЯКИМ ВІН БУВ ДО ЦІЄЇ ПРАВКИ: RTT до рішення не
// доходить взагалі. Мусить провалити рівно rtt-rise (bufferbloat невидимий) і
// пройти решту — тобто довести, що rtt-rise ловить саме нову проводку.
func brokenRTTBlind(c bitrateCtl, loss float64, _ time.Duration, now time.Time) (bitrateCtl, bool) {
	return c.step(loss, 0, now)
}

// brokenRTTAbsolute — RTT за РІВНЕМ, без тренду: приріст вище порога ріже завжди.
// Так виглядала б «проста» реалізація, і вона мусить провалити rtt-flat: далекий
// глядач зі сталим високим приростом поїхав би на підлогу ні за що.
func brokenRTTAbsolute(c bitrateCtl, loss float64, excess time.Duration, now time.Time) (bitrateCtl, bool) {
	if excess >= rttExcessMin && loss <= lossHighFrac {
		loss = lossHighFrac + 0.01
	}
	return c.step(loss, 0, now)
}

// brokenUpBlindToQueue — приріст, підрізаний рівно під поріг звільнення підйому.
// Так виглядає контролер, у якого важеля по СТОЯЧІЙ черзі немає взагалі: ані
// зрізу по рівню, ані блокування. Мусить провалити саме rtt-flat-hold — гейт,
// який вимагає, щоб ціль стояла, поки стоїть черга.
func brokenUpBlindToQueue(c bitrateCtl, loss float64, excess time.Duration, now time.Time) (bitrateCtl, bool) {
	if excess >= rttUpClear {
		excess = rttUpClear - time.Millisecond
	}
	return c.step(loss, excess, now)
}

func TestBitrateRTTGatesNegativeControl(t *testing.T) {
	for _, tc := range []struct {
		name string
		step stepFn
		want string // яка саме перевірка мусить почервоніти
	}{
		{"сліпий до RTT (як було до правки)", brokenRTTBlind, "rtt-rise"},
		{"RTT за рівнем, без тренду", brokenRTTAbsolute, "rtt-flat"},
		{"черга ціль не тримає", brokenUpBlindToQueue, "rtt-flat-hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fails := checkRTT(tc.step)
			if len(fails) == 0 {
				t.Fatalf("негативний контроль: зламаний крок пройшов гейти RTT зеленим")
			}
			for _, f := range fails {
				if strings.HasPrefix(f, tc.want) {
					return
				}
			}
			t.Fatalf("негативний контроль: чекали провал %q, а провали інші: %v", tc.want, fails)
		})
	}
}
