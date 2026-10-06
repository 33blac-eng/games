// Перевіряє те, що можна перевірити на машині з ОДНИМ монітором: вибір індексу,
// кламп на старті, поведінку при неіснуючому індексі, зсув епохи й правило
// геометрії софт-шляху. Живе перемикання (SwitchOutput) потребує другого
// монітора — інструкція в звіті/README.
package main

import (
	"math"
	"testing"
)

func TestResolveOutput(t *testing.T) {
	cases := []struct {
		name    string
		idx     int
		count   int
		wantErr bool
	}{
		{"перший з одного", 0, 1, false},
		{"другий з двох", 1, 2, false},
		// Негативний контроль: якби resolveOutput пускав усе (або клампив, як
		// на старті), рівно ці рядки стали б червоними.
		{"неіснуючий: другий з одного", 1, 1, true},
		{"неіснуючий: далеко за межею", 7, 2, true},
		{"відʼємний", -1, 2, true},
		{"жодного виходу", 0, 0, true},
		{"енумерація впала", 0, -1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := resolveOutput(c.idx, c.count)
			if (err != nil) != c.wantErr {
				t.Fatalf("resolveOutput(%d, %d) = %v, хотіли помилку=%v", c.idx, c.count, err, c.wantErr)
			}
		})
	}
}

func TestClampStartOutput(t *testing.T) {
	// На старті правило ПРОТИЛЕЖНЕ до resolveOutput: краще показати основний
	// монітор, ніж не піднятись зовсім (відʼєднали другий — ПК зник би з пульта).
	cases := []struct{ idx, count, want int }{
		{0, 1, 0},
		{1, 2, 1},
		{1, 1, 0},  // другого монітора вже нема -> основний
		{5, 2, 0},  // застарілий індекс із планувальника
		{-1, 2, 0}, // сміття в прапорці
		{3, 0, 0},  // енумерація не вдалась
	}
	for _, c := range cases {
		if got := clampStartOutput(c.idx, c.count); got != c.want {
			t.Fatalf("clampStartOutput(%d, %d) = %d, хотіли %d", c.idx, c.count, got, c.want)
		}
	}
}

// TestEpochShifts — головний негативний контроль усієї задачі. До неї епоха
// була КОНСТАНТОЮ (`configEpoch1 = 1`), і цей тест на тому коді не міг би
// навіть скомпілюватись; на «зсув є, але не працює» (bumpEpoch = no-op) він
// падає на першому ж порівнянні.
func TestEpochShifts(t *testing.T) {
	saved := configEpoch.Load()
	t.Cleanup(func() { configEpoch.Store(saved) })

	configEpoch.Store(1)
	if got := currentEpoch(); got != 1 {
		t.Fatalf("стартова епоха = %d, хотіли 1", got)
	}
	prev := currentEpoch()
	for i := 0; i < 3; i++ {
		got := bumpEpoch()
		if got == prev {
			t.Fatalf("епоха не зрушила: %d -> %d (глядач не побачить зміни геометрії)", prev, got)
		}
		if got != currentEpoch() {
			t.Fatalf("bumpEpoch повернув %d, а в потік піде %d", got, currentEpoch())
		}
		prev = got
	}
}

// Поле envelope.ConfigEpoch — 16-бітне, тож епоха мусить загортатись на 1, а не
// на 0: нуль на боці глядача означає «не задано».
func TestEpochWrapsPastUint16(t *testing.T) {
	saved := configEpoch.Load()
	t.Cleanup(func() { configEpoch.Store(saved) })

	configEpoch.Store(0xFFFF)
	if got := bumpEpoch(); got != 1 {
		t.Fatalf("після 0xFFFF епоха = %d, хотіли 1", got)
	}
}

func TestOutputRequestZeroIsAValidIndex(t *testing.T) {
	var r outputRequest
	if _, ok := r.take(); ok {
		t.Fatal("порожній запит віддав індекс")
	}
	// Негативний контроль сентинела: зі звичайним нулем-сентинелом саме цей
	// випадок (перемикання на основний монітор) губився б мовчки.
	r.set(0)
	idx, ok := r.take()
	if !ok || idx != 0 {
		t.Fatalf("take() = (%d, %v), хотіли (0, true)", idx, ok)
	}
	if _, ok := r.take(); ok {
		t.Fatal("запит віддався двічі")
	}
	r.set(1)
	r.set(3) // остання перемагає: у черзі має сенс лише вона
	if idx, ok := r.take(); !ok || idx != 3 {
		t.Fatalf("take() = (%d, %v), хотіли (3, true)", idx, ok)
	}
}

func TestEncodeGeometry(t *testing.T) {
	const reqW, reqH = 1920, 1080
	cases := []struct {
		name         string
		srcW, srcH   int
		software     bool
		wantW, wantH int
	}{
		// Апаратний шлях масштабує NV12->NV12 у VideoProcessor — бере запитане.
		{"hw, менший монітор", 1280, 720, false, reqW, reqH},
		{"hw, більший монітор", 2560, 1440, false, reqW, reqH},
		// 🚨 Софт-MFT не масштабує: менший монітор без цього правила = помилка
		// на КОЖНОМУ кадрі (encode.checkPlanes), більший = мовчазний кроп.
		{"sw, менший монітор", 1280, 720, true, 1280, 720},
		{"sw, більший монітор", 2560, 1440, true, 2560, 1440},
		{"sw, збіг", reqW, reqH, true, reqW, reqH},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := encodeGeometry(reqW, reqH, c.srcW, c.srcH, c.software)
			if w != c.wantW || h != c.wantH {
				t.Fatalf("encodeGeometry(%d,%d, %d,%d, sw=%v) = %dx%d, хотіли %dx%d",
					reqW, reqH, c.srcW, c.srcH, c.software, w, h, c.wantW, c.wantH)
			}
		})
	}
}

// Рідна роздільність за замовчуванням — те, через що агент програвав
// MeshCentral-у: зашиті 1920x1080 на моніторі 2560x1440.
// A-41 — недосказана геометрія мусить рвати старт, а не зникати.
//
// 🚨 Що саме тут міряється: НЕ «requestedSize повертає рідну» (це він робив і
// до фіксу, мовчки), а те, що агент ВЗАГАЛІ не стартує з половиною запиту.
// Негативний контроль: прибрати перевірку однієї осі з validateSize — і два
// підтести з чотирьох червоніють.
func TestValidateSize(t *testing.T) {
	cases := []struct {
		name       string
		reqW, reqH int
		wantErr    bool
	}{
		{name: "обидва нулі — рідна роздільність, це нормальний старт", reqW: 0, reqH: 0},
		{name: "обидві осі задані — свідомий запит оператора", reqW: 1280, reqH: 720},
		{name: "-width без -height — недосказано, старт рвемо", reqW: 1280, wantErr: true},
		{name: "-height без -width — недосказано, старт рвемо", reqH: 720, wantErr: true},
		{name: "відʼємна вісь — сміття, старт рвемо", reqW: -1, reqH: 720, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSize(tc.reqW, tc.reqH)
			if tc.wantErr && err == nil {
				t.Fatalf("validateSize(%d, %d) = nil, очікували помилку старту", tc.reqW, tc.reqH)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateSize(%d, %d) = %v, очікували nil", tc.reqW, tc.reqH, err)
			}
		})
	}
}

func TestRequestedSize(t *testing.T) {
	cases := []struct {
		name         string
		reqW, reqH   int
		srcW, srcH   int
		wantW, wantH int
	}{
		// (а) прапорців нема (нулі) — беремо розмір виводу, яким би він не був.
		{"без прапорців, 1440p", 0, 0, 2560, 1440, 2560, 1440},
		{"без прапорців, 1080p", 0, 0, 1920, 1080, 1920, 1080},
		{"без прапорців, 1920x1200", 0, 0, 1920, 1200, 1920, 1200},
		// (б) явні прапорці перекривають — шлях для слабкого каналу.
		{"явні менші за монітор", 1280, 720, 2560, 1440, 1280, 720},
		{"явні більші за монітор", 2560, 1440, 1920, 1080, 2560, 1440},
		{"явні, рівно 1080p", 1920, 1080, 2560, 1440, 1920, 1080},
		// Половина розміру розміром не є: одна вісь без другої розтягнула б кадр.
		{"лише ширина", 1280, 0, 2560, 1440, 2560, 1440},
		{"лише висота", 0, 720, 2560, 1440, 2560, 1440},
		{"відʼємні", -1, -1, 2560, 1440, 2560, 1440},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := requestedSize(c.reqW, c.reqH, c.srcW, c.srcH)
			if w != c.wantW || h != c.wantH {
				t.Fatalf("requestedSize(%d,%d, %d,%d) = %dx%d, хотіли %dx%d",
					c.reqW, c.reqH, c.srcW, c.srcH, w, h, c.wantW, c.wantH)
			}
		})
	}
}

// (в) Бітрейт масштабується з пікселями. Без цього рідна роздільність коштувала
// б різкості: 1440p при старих 8 Мбіт/с — менше біт на піксель, ніж було на
// 1080p, тобто мильніша картинка, лише з іншого боку.
func TestDefaultBitrate(t *testing.T) {
	t.Run("1080p лишається 8 Мбіт/с", func(t *testing.T) {
		if got := defaultBitrate(0, 1920, 1080); got != 8_000_000 {
			t.Fatalf("defaultBitrate(0, 1920,1080) = %d, хотіли 8000000", got)
		}
	})

	t.Run("1440p дає 14.2 Мбіт/с", func(t *testing.T) {
		// 8_000_000 * (2560*1440) / (1920*1080) = 8_000_000 * 1.777...
		const want = 14_222_222
		if got := defaultBitrate(0, 2560, 1440); got != want {
			t.Fatalf("defaultBitrate(0, 2560,1440) = %d, хотіли %d", got, want)
		}
	})

	t.Run("щільність біт-на-піксель зберігається", func(t *testing.T) {
		// Головна властивість, а не окреме число: скільки б пікселів не було,
		// біт на піксель має лишитись тим самим, що на 1080p (±1 біт/с на
		// цілочисельне ділення).
		base := float64(8_000_000) / float64(1920*1080)
		// A-41: вище стелі (4K) щільність свідомо НЕ тримається — див. окремий кейс.
		for _, g := range [][2]int{{1280, 720}, {1920, 1080}, {1920, 1200}, {2560, 1440}} {
			got := defaultBitrate(0, g[0], g[1])
			density := float64(got) / float64(g[0]*g[1])
			if d := density - base; d > 1e-6 || d < -1e-6 {
				t.Fatalf("%dx%d: %d біт/с = %.6f біт/піксель, база %.6f", g[0], g[1], got, density, base)
			}
		}
	})

	t.Run("4K упирається в стелю автобітрейту (A-41)", func(t *testing.T) {
		if got := defaultBitrate(0, 3840, 2160); got != maxAutoBitrateBps {
			t.Fatalf("defaultBitrate(0, 3840,2160) = %d, хотіли стелю %d", got, maxAutoBitrateBps)
		}
	})

	t.Run("P0: стеля не ріже нижче 4K і дає 4K ~30 Мбіт/с", func(t *testing.T) {
		if maxAutoBitrateBps < 28_000_000 {
			t.Fatalf("стеля %d: 4K знову обрізано", maxAutoBitrateBps)
		}
		// 3440x1440 (ultrawide, 4.95 Мпікс) — ще за щільністю, без стелі.
		want := 8_000_000 * 3440 * 1440 / (1920 * 1080)
		if got := defaultBitrate(0, 3440, 1440); got != want {
			t.Fatalf("defaultBitrate(0, 3440,1440) = %d, хотіли %d", got, want)
		}
	})

	t.Run("явний -bitrate перекриває", func(t *testing.T) {
		if got := defaultBitrate(3_000_000, 2560, 1440); got != 3_000_000 {
			t.Fatalf("defaultBitrate(3000000, 2560,1440) = %d, хотіли 3000000", got)
		}
	})

	t.Run("невідома геометрія — базове значення", func(t *testing.T) {
		if got := defaultBitrate(0, 0, 0); got != 8_000_000 {
			t.Fatalf("defaultBitrate(0, 0,0) = %d, хотіли 8000000", got)
		}
	})
}

// Відступ після відмови енкодера. Без нього «кодуй у рідній» перетворює машину
// з MFT, обмеженим 1920x1088, на ПК, що зник із пульта.
func TestFallbackSize(t *testing.T) {
	cases := []struct {
		name         string
		auto         bool
		w, h         int
		wantW, wantH int
		wantOK       bool
	}{
		{"авто, рідні 1440p", true, 2560, 1440, 1920, 1080, true},
		// Q-05 (research/QUALITY-AUDIT.md): було 1920x1080 — сплющення 16:10
		// по вертикалі ×0.9. Тепер пропорції збережено.
		{"авто, рідні 1920x1200", true, 1920, 1200, 1728, 1080, true},
		{"авто, рідні 4K", true, 3840, 2160, 1920, 1080, true},
		{"авто, рідні 2560x1600", true, 2560, 1600, 1728, 1080, true},
		{"авто, ultrawide 3440x1440", true, 3440, 1440, 1920, 804, true},
		{"авто, портрет 1080x1920", true, 1080, 1920, 608, 1080, true},
		{"авто, 5:4 1280x1024 влазить", true, 1280, 1024, 1920, 1080, true},
		// Влазить у бокс, але MFT відмовив — як і раніше, рівно 1920x1080.
		{"авто, рідні 1680x1050", true, 1680, 1050, 1920, 1080, true},
		// Другого кола немає: відмова вже на 1920x1080 — не про роздільність.
		{"авто, вже 1920x1080", true, 1920, 1080, 1920, 1080, false},
		// Явний запит оператора не підмінюємо мовчки.
		{"явні прапорці", false, 2560, 1440, 2560, 1440, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h, ok := fallbackSize(c.auto, c.w, c.h)
			if w != c.wantW || h != c.wantH || ok != c.wantOK {
				t.Fatalf("fallbackSize(%v, %d,%d) = %dx%d,%v; хотіли %dx%d,%v",
					c.auto, c.w, c.h, w, h, ok, c.wantW, c.wantH, c.wantOK)
			}
		})
	}
}

// Q-05: відступ для будь-якої геометрії зберігає пропорції (похибка — лише
// округлення до парного), не виходить за бокс 1920x1080 і не має більше
// макроблоків, ніж сам 1920x1080 (тобто для MFT не важчий за перевірений).
// Негативний контроль: стара реалізація (рівно 1920x1080) валить перевірку
// пропорцій уже на 1920x1200.
func TestFallbackSizeKeepsAspect(t *testing.T) {
	mbs := func(w, h int) int { return ((w + 15) / 16) * ((h + 15) / 16) }
	limit := mbs(fallbackW, fallbackH)
	for w := 1090; w <= 7680; w += 26 {
		for h := 482; h <= 4320; h += 34 {
			if w <= fallbackW && h <= fallbackH {
				continue // влазить — інша гілка (рівно 1920x1080)
			}
			fw, fh, ok := fallbackSize(true, w, h)
			if !ok {
				t.Fatalf("%dx%d: немає відступу", w, h)
			}
			if fw%2 != 0 || fh%2 != 0 || fw > fallbackW || fh > fallbackH || fw < 2 || fh < 2 {
				t.Fatalf("%dx%d -> %dx%d: не парне або поза боксом", w, h, fw, fh)
			}
			if m := mbs(fw, fh); m > limit {
				t.Fatalf("%dx%d -> %dx%d: %d MB > %d", w, h, fw, fh, m, limit)
			}
			// Одна сторона впирається в бокс, друга — точна пропорція з
			// похибкою округлення до парного (≤ 1 піксель + float).
			if fw != fallbackW && fh != fallbackH {
				t.Fatalf("%dx%d -> %dx%d: жодна сторона не впирається в бокс", w, h, fw, fh)
			}
			if fw == fallbackW {
				if d := math.Abs(float64(fh) - float64(h)*float64(fw)/float64(w)); d > 1.0001 {
					t.Fatalf("%dx%d -> %dx%d: висота зсунута на %.2f px", w, h, fw, fh, d)
				}
			} else if d := math.Abs(float64(fw) - float64(w)*float64(fh)/float64(h)); d > 1.0001 {
				t.Fatalf("%dx%d -> %dx%d: ширина зсунута на %.2f px", w, h, fw, fh, d)
			}
		}
	}
}

// Політика «софт заради рідної роздільності» — межа, а не смак.
// Число 12 узяте із заміру: софтверний енкодер на 2560x1440 з'їв 3.31 ядра
// проти 0.26 в апаратного. На 4 ядрах це 83% машини, тобто людина за таким
// ПК працювати не зможе — і сама сесія, яку ми показуємо, гальмуватиме.
func TestSoftwareNativeAffordable(t *testing.T) {
	cases := []struct {
		name  string
		cores int
		want  bool
	}{
		{"офісні 4 ядра — 3.31 ядра це 83%, не можна", 4, false},
		{"8 ядер — 41%, усе ще забагато", 8, false},
		{"рівно на порозі", softwareNativeMinCores, true},
		{"робоча станція 24 ядра — 14%, можна", 24, true},
		{"нуль ядер не буває, але не має пускати", 0, false},
	}
	for _, c := range cases {
		if got := softwareNativeAffordable(c.cores); got != c.want {
			t.Errorf("%s: softwareNativeAffordable(%d) = %v, хотіли %v",
				c.name, c.cores, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Бюджет CPU софтверного енкодера.
//
// 🔴 Гілка, яку це покриває, — та, що не виконувалась НІКОЛИ: encode.New
// повернув УСПІХ, але енкодер вийшов софтверним (машина без апаратного H.264
// MFT — mft.c перемикається мовчки, без помилки). Заміри парку 30.08: ПК
// Computer, 4 логічні ядра, рідні 1920x1200.

const (
	pxComputer = 1920 * 1200 // ПК Computer: рідна геометрія
	pxOrganic  = 1920 * 1080 // ПК Organic: рідна геометрія
	px1440     = 2560 * 1440 // геометрія самого заміру
)

func TestSoftwareFPSCapWeakMachineIsThrottled(t *testing.T) {
	const cores, fps = 4, 30
	capped := softwareFPSCap(cores, pxComputer, fps)
	if capped >= fps {
		t.Fatalf("4 ядра на 1920x1200 софтом = %.2f ядра — стеля мала опуститись, а лишилась %d к/с",
			softwareCoreCost(pxComputer, fps), capped)
	}
	// Стеля мусить справді вкладатись у бюджет, а не просто бути меншою.
	if !softwareAffordable(cores, pxComputer, capped) {
		t.Fatalf("стеля %d к/с усе одно коштує %.2f ядра на %d ядрах",
			capped, softwareCoreCost(pxComputer, capped), cores)
	}
	if capped < softwareFPSFloor {
		t.Fatalf("стеля %d к/с нижча за підлогу %d — це вже слайд-шоу", capped, softwareFPSFloor)
	}
}

// Дзеркальний: ядер вистачає — лишаємо як є.
func TestSoftwareFPSCapStrongMachineUntouched(t *testing.T) {
	const fps = 30
	for _, cores := range []int{12, 16, 24} {
		if got := softwareFPSCap(cores, px1440, fps); got != fps {
			t.Fatalf("%d ядер: стеля %d к/с замість %d — притиснули машину, яка тягне", cores, got, fps)
		}
	}
}

// Рідна вже 1080p і 8 ядер (ПК Organic) — не чіпати: софт там прийнятний, і
// відступати нема куди.
func TestSoftwareFPSCapOrganicUntouched(t *testing.T) {
	const cores, fps = 8, 30
	if got := softwareFPSCap(cores, pxOrganic, fps); got != fps {
		t.Fatalf("Organic (8 ядер, 1920x1080): стеля %d к/с замість %d — притиснули машину, яка тягне (%.2f ядра на %d ядрах)",
			got, fps, softwareCoreCost(pxOrganic, fps), cores)
	}
}

// Дві ручки — один бюджет. softwareNativeMinCores каже «12 ядер тягнуть замір»,
// і softwareFPSCap ЗОБОВ'ЯЗАНА казати те саме, інакше вони роз'їдуться.
func TestSoftwareBudgetAgreesWithCoreThreshold(t *testing.T) {
	const fps = softwareMeasuredFPS
	if !softwareNativeAffordable(softwareNativeMinCores) {
		t.Fatal("поріг сам себе не проходить")
	}
	if got := softwareFPSCap(softwareNativeMinCores, softwareMeasuredPixels, fps); got != fps {
		t.Fatalf("на порозі %d ядер бюджет мав зійтись рівно, а стеля впала до %d к/с",
			softwareNativeMinCores, got)
	}
	if got := softwareFPSCap(softwareNativeMinCores-1, softwareMeasuredPixels, fps); got >= fps {
		t.Fatalf("на %d ядрах (нижче порога) стеля лишилась %d к/с — ручки роз'їхались",
			softwareNativeMinCores-1, got)
	}
}

// Проміжок між кадрами — те саме правило в тому вигляді, як його читає цикл.
func TestSoftwareFrameGap(t *testing.T) {
	if gap := softwareFrameGap(24, px1440, 30); gap != 0 {
		t.Fatalf("машина тягне, а цикл дістав проміжок %v замість нуля", gap)
	}
	gap := softwareFrameGap(4, pxComputer, 30)
	if gap <= 0 {
		t.Fatal("слабка машина, а проміжку немає — притискання не дійде до циклу")
	}
	// Проміжок мусить лишатись сильно меншим за сторож браузера (3000 мс) і за
	// keepalive (1000 мс), інакше притискання з'їдало б і keepalive-кадр.
	if gap >= keepaliveAfter {
		t.Fatalf("проміжок %v дорівнює keepalive %v або більший — притискання почне їсти keepalive", gap, keepaliveAfter)
	}
}

// Сміттєвий вхід не має міняти нічого: невідома геометрія чи кількість ядер —
// привід не чіпати частоту, а не привід опустити її на підлогу.
func TestSoftwareFPSCapIgnoresGarbage(t *testing.T) {
	for _, c := range []struct{ cores, px, fps int }{
		{0, pxComputer, 30}, {-1, pxComputer, 30}, {4, 0, 30}, {4, pxComputer, 0},
	} {
		if got := softwareFPSCap(c.cores, c.px, c.fps); got != c.fps {
			t.Fatalf("cores=%d px=%d fps=%d: стеля %d замість %d", c.cores, c.px, c.fps, got, c.fps)
		}
	}
}
