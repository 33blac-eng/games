package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/organicoils/oo-screen/internal/control"
)

// A-28 — реконект зобовʼязаний ставити гейт на паузу і повертати його рівно
// туди, де він був, не затираючи слова нового хаба.
//
// Негативний контроль (те, що робить тест здатним почервоніти): до фіксу
// beginReconnectGate не існувало зовсім, і перший підтест «під час дозвону
// стоїть пауза» червонів би на порожньому місці.
func TestReconnectGate(t *testing.T) {
	cases := []struct {
		name string
		// стан гейта ДО реконекту
		pausedBefore bool
		// що (і чи) сказав хаб за час дозвону: nil = мовчав
		hubSays *bool
		want    bool
	}{
		{name: "хаб мовчить, до реконекту йшов потік — повертаємось у потік",
			pausedBefore: false, hubSays: nil, want: false},
		{name: "хаб мовчить, до реконекту була пауза — лишаємось на паузі",
			pausedBefore: true, hubSays: nil, want: true},
		{name: "хаб сказав pause — не затираємо його свіжим resume",
			pausedBefore: false, hubSays: boolPtr(false), want: true},
		{name: "хаб сказав resume — не залипаємо в нашій паузі",
			pausedBefore: true, hubSays: boolPtr(true), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var paused, seen atomic.Bool
			paused.Store(tc.pausedBefore)
			// Прапорець «хаб уже говорив» лишаємо зведеним від МИНУЛОЇ сесії:
			// beginReconnectGate мусить його скинути сам, інакше слово мертвого
			// хаба заблокувало б відновлення назавжди.
			seen.Store(true)

			g := beginReconnectGate(&paused, &seen)
			if !paused.Load() {
				t.Fatalf("під час дозвону гейт мусить стояти на паузі (капчер віддано, звук мовчить)")
			}
			if tc.hubSays != nil {
				// Рівно те, що робить onGate у main.go.
				seen.Store(true)
				if *tc.hubSays {
					paused.CompareAndSwap(true, false)
				} else {
					paused.CompareAndSwap(false, true)
				}
			}
			g.restore()
			if got := paused.Load(); got != tc.want {
				t.Fatalf("після реконекту paused=%v, очікували %v", got, tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// Пункт 127.3 — A-06: коли енкодер треба перебудовувати.
//
// Тест на рівні stream із живим капчером тут неможливий (потрібен D3D-девайс і
// DXGI-дублікація), тож перевіряємо саме те правило, заради якого A-06 і
// заведено: тотожність капчера — це (девайс, ПОКОЛІННЯ), а не сама адреса.
func TestEncoderStale(t *testing.T) {
	cases := []struct {
		name        string
		dev, encDev uintptr
		gen, encGen uint64
		want        bool
	}{
		{"той самий капчер", 0x1000, 0x1000, 1, 1, false},
		{"інший девайс", 0x2000, 0x1000, 1, 1, true},
		// Регресія, заради якої все це: капчер перебудували, D3D-девайс ліг на
		// ТУ САМУ адресу. Порівняння лише за вказівником сказало б «свій».
		{"та сама адреса, нове покоління", 0x1000, 0x1000, 2, 1, true},
		{"капчер віддав дублікацію (dev=0)", 0, 0x1000, 2, 1, true},
	}
	for _, c := range cases {
		if got := encoderStale(c.dev, c.gen, c.encDev, c.encGen); got != c.want {
			t.Errorf("%s: encoderStale(%#x,%d,%#x,%d)=%v, чекали %v",
				c.name, c.dev, c.gen, c.encDev, c.encGen, got, c.want)
		}
	}
}

// spsPLID — мінімальний Annex-B SPS із заданими profile_idc/constraints/level.
// Далі трьох байтів після NAL-заголовка ParseSPS для ProfileLevelID() не треба,
// але хвіст мусить лишатись валідним RBSP — беремо реальний 1920x1080 SPS і
// підміняємо саме ці три байти.
func spsPLID(t *testing.T, profileIDC, constraints, levelIDC byte) []byte {
	t.Helper()
	// SPS від Microsoft H264 Encoder MFT, 1920x1080 (Annex-B, зі стартовим кодом).
	nal := []byte{
		0x00, 0x00, 0x00, 0x01,
		0x67, 0x64, 0x00, 0x2A, 0xAC, 0xD9, 0x40, 0x78,
		0x02, 0x27, 0xE5, 0x84, 0x00, 0x00, 0x03, 0x00,
		0x04, 0x00, 0x00, 0x03, 0x00, 0xF0, 0x3C, 0x60,
		0xC6, 0x58,
	}
	nal[5], nal[6], nal[7] = profileIDC, constraints, levelIDC
	return nal
}

// A-26: fmtp мусить нести профіль І рівень ЖИВОГО енкодера, а не константи.
func TestH264FmtpFollowsEncoderSPS(t *testing.T) {
	old, _ := encPLID.Load().(string)
	t.Cleanup(func() { encPLID.Store(old) })

	// Main 5.1 — те, що MFT пінить на 2560x1440 після переходу на Main.
	plid, err := encoderPLID(spsPLID(t, 77, 0, 51))
	if err != nil {
		t.Fatalf("encoderPLID: %v", err)
	}
	encPLID.Store(plid)
	if got, want := h264Fmtp(), "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d0033"; got != want {
		t.Fatalf("h264Fmtp()=%q, чекали %q", got, want)
	}

	// Порожній SPS = енкодера ще нема: лишається константа, а не сміття в SDP.
	encPLID.Store("")
	if got := h264Fmtp(); got != h264FmtpLine {
		t.Fatalf("без SPS h264Fmtp()=%q, чекали фолбек %q", got, h264FmtpLine)
	}
	if _, err := encoderPLID(nil); err == nil {
		t.Fatal("encoderPLID(nil) мусить повертати помилку, а не порожній рядок мовчки")
	}
}

// 🔴 Гейт проти розходження SDP <-> енкодер.
//
// Профіль живе у двох місцях, які компілятор між собою не звʼязує: перша
// сходинка профільної драбини в mft.c і фолбек-рядок h264FmtpLine, яким агент
// оголошується ДО відкриття MFT. Один раз вони вже розійшлися (mft.c кодував
// High, SDP теж казав High — і обидва були неправильні для браузера), тож тут
// читаємо саме джерело, а не памʼять про нього.
func TestFmtpFallbackMatchesMFTProfile(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "encode", "mft.c"))
	if err != nil {
		t.Fatalf("mft.c: %v", err)
	}
	m := regexp.MustCompile(`profiles\[\] = \{ eAVEncH264VProfile_(\w+)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("не знайшов профільну драбину в mft.c — гейт осліп, полагодь регулярку")
	}
	// profile_idc за ITU-T H.264 Annex A; MF_MT_MPEG2_PROFILE несе ті самі числа.
	idc := map[string]byte{"Baseline": 66, "ConstrainedBaseline": 66, "Main": 77, "High": 100}
	want, ok := idc[string(m[1])]
	if !ok {
		t.Fatalf("невідомий профіль %q у mft.c — додай його в таблицю", m[1])
	}
	if int(want) != encProfileMain {
		t.Fatalf("mft.c кодує %s (profile_idc=%d), а encProfileMain=%d — лог про відкат на High бреше",
			m[1], want, encProfileMain)
	}
	// Браузери оголошують лише 42xx/4dxx/f4xx (виміряно в Chrome через
	// RTCRtpReceiver.getCapabilities). High(0x64) серед них немає.
	if want != 66 && want != 77 {
		t.Fatalf("mft.c кодує %s (0x%02x) — приймач такого не оголошує, хаб відповість 415", m[1], want)
	}
	gotPLID := fmtpParam(h264FmtpLine, "profile-level-id")
	if len(gotPLID) != 6 {
		t.Fatalf("h264FmtpLine=%q: profile-level-id не 6 hex-цифр", h264FmtpLine)
	}
	raw, err := hex.DecodeString(gotPLID)
	if err != nil {
		t.Fatalf("profile-level-id %q: %v", gotPLID, err)
	}
	if raw[0] != want {
		t.Fatalf("mft.c кодує %s (profile_idc=%d), а фолбек h264FmtpLine оголошує 0x%02x — SDP розійшовся з енкодером",
			m[1], want, raw[0])
	}
}

// fmtpParam — значення одного параметра fmtp-рядка ("" якщо немає).
func fmtpParam(line, key string) string {
	for _, kv := range strings.Split(line, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && k == key {
			return v
		}
	}
	return ""
}

func mustCtl(t *testing.T, m control.Msg) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %v: %v", m, err)
	}
	return b
}
