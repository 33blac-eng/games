// Package pcmu — G.711 μ-law (RTP payload type 0, PCMU/8000).
//
// F1: ТЕПЕР ЦЕ ЗАПАСНИЙ КОДЕК (OO_SCREEN_AUDIO_CODEC=pcmu). Типовий — Opus 48
// кГц стерео через internal/opusenc: знайшовся чистий-Go енкодер
// (github.com/thesyncim/gopus), тож аргумент «libopus/cgo» нижче більше не діє.
//
// ЧОМУ САМЕ ЦЕЙ КОДЕК, А НЕ OPUS. Твердження «WebRTC іншого аудіокодека не
// приймає» — неправда, і це перевірено живцем, а не за памʼяттю. Chrome
// 148.0.7778.280 на цій машині, RTCRtpReceiver.getCapabilities('audio'):
//
//	audio/opus/48000/2, audio/red/48000/2, audio/G722/8000/1,
//	audio/PCMU/8000/1, audio/PCMA/8000/1, audio/CN/8000/1, ...
//
// PCMU/PCMA — mandatory-to-implement для WebRTC (RFC 7874 §3), тобто в браузері
// вони є завжди, як і Opus. Різниця для нас у ціні енкодера:
//
//   - Opus: у Windows Media Foundation Opus-енкодера НЕМАЄ (є AAC і лише
//     ДЕКОДЕР Opus). Лишається cgo+libopus — а libopus на цій машині немає в
//     жодному вигляді: ні opus.h/libopus.a в mingw64, ні msys2, ні vcpkg, ні
//     pkg-config. Тобто ціна — або ~250 файлів C у репо (vendored libopus),
//     або зібрати libopus із джерел на КОЖНІЙ машині збірки.
//   - G.711 μ-law: ось цей файл. Нуль залежностей, нуль вимог до збірки,
//     таблиці немає взагалі — формула з G.711.
//
// ponytail: СТЕЛЯ ЦЬОГО ВИБОРУ НАЗИВАЄТЬСЯ ЧЕСНО — 8 кГц моно, 64 кбіт/с,
// смуга 300–3400 Гц (телефон). Мова, сповіщення, голос у дзвінку — розбірливо;
// музика — погано. Апгрейд до Opus не чіпає ні транспорт, ні гейтинг, ні
// годинник: міняється рівно Encode() тут, MimeType у двох місцях і CodecID у
// mkv.go. Проміжний варіант — G.722 (16 кГц за ті самі 64 кбіт/с, браузер його
// теж приймає, див. список вище), але його енкодер — це вже QMF + два ADPCM
// суб-діапазони з таблицями, тобто ~250 рядків DSP, які нічим дешевим не
// перевіриш; μ-law перевіряється точним round-trip'ом (pcmu_test.go).
package pcmu

import "time"

const (
	// bias і clip — константи G.711: 33 (зсув) у форматі 14-біт + 3 біти
	// мантиси = 0x84, і межа кліпування амплітуди.
	bias = 0x84
	clip = 32635

	// Rate — частота дискретизації PCMU. Не «наша» константа, а частина
	// кодека: RTP clock rate для PT 0 теж 8000.
	Rate = 8000

	// FrameSamples — семплів у кадрі 20 мс (ptime=20 — те, що шле і чекає
	// браузер за замовчуванням). Один семпл = один байт, тож це ще й довжина
	// payload у байтах.
	FrameSamples = Rate / 50
)

// Encode стискає лінійний 16-бітний семпл у один байт μ-law.
func Encode(sample int16) byte {
	// int32, бо -32768 не має додатного відповідника в int16: -(-32768)
	// переповнилось би назад у -32768 і дало б знак «+» з максимальною
	// амплітудою — клац на кожному максимально гучному семплі.
	s := int32(sample)
	var sign byte
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > clip {
		s = clip
	}
	s += bias

	exp := byte(7)
	for mask := int32(0x4000); exp > 0 && s&mask == 0; mask >>= 1 {
		exp--
	}
	mantissa := byte(s>>(exp+3)) & 0x0F
	return ^(sign | exp<<4 | mantissa)
}

// Decode розтискає байт μ-law назад у лінійний семпл. Потрібен рівно там, де
// звук треба покласти НЕ в RTP: у MKV-запис сесії (доріжка A_PCM/INT/LIT).
func Decode(b byte) int16 {
	u := ^b
	sign := u & 0x80
	exp := int32((u >> 4) & 0x07)
	mantissa := int32(u & 0x0F)
	v := ((mantissa<<3)+bias)<<exp - bias
	if sign != 0 {
		return int16(-v)
	}
	return int16(v)
}

// Silence — байт μ-law, що декодується в нуль. НЕ 0x00: у μ-law нуль кодується
// як 0xFF, а 0x00 — це найгучніший відʼємний семпл. Заповнити пропуск нулями
// означало б заповнити його тріском на повну гучність.
const Silence = 0xFF

// Duration — скільки звучить payload із n байтів. Один байт = один семпл 8 кГц,
// тож це і є та тривалість, яку чекає media.Sample: саме з неї pion крутить
// RTP-годинник доріжки (Duration.Seconds()*ClockRate).
func Duration(n int) time.Duration {
	return time.Duration(n) * time.Second / Rate
}
