// Package codec444 — прототип Q2/F8: вибір 4:4:4-кодека (VP9 profile 1 або
// AV1 profile 1 «High») за можливостями плеєра з фолбеком на H.264 4:2:0.
//
// Чисті функції без I/O. Увімкнення — лише явним прапором (Config.Enabled,
// за замовчуванням false): без нього Negotiate ЗАВЖДИ повертає H.264, тож
// поточний шлях не змінюється. Реальний медіашлях агента/хаба лишається
// H.264; цей пакет дає рішення переговорів і параметри програмного
// енкодера (ffmpeg), перевірені лише на Linux-симуляції (bench/quality/
// codec444_motion.py). Декодування у Chrome — НЕ ПЕРЕВІРЕНО.
package codec444

import (
	"sort"
	"strings"
)

// Codec — обраний кодек відеопотоку.
type Codec string

const (
	H264  Codec = "h264"   // Main 4:2:0 — фолбек, завжди доступний
	VP9P1 Codec = "vp9-p1" // VP9 profile 1, 8 біт 4:4:4
	AV1P1 Codec = "av1-p1" // AV1 profile 1 (High), 8 біт 4:4:4
)

// Config — налаштування прототипу. Нульове значення = вимкнено.
type Config struct {
	Enabled bool
	// Prefer — порядок переваги 4:4:4-кодеків; порожній = {AV1P1, VP9P1}
	// (AV1 у симуляції тримає бітрейт, VP9 realtime перевищує ціль).
	Prefer []Codec
}

// Caps — що плеєр повідомив про себе (див. desktop-oo-codec444.js).
type Caps struct {
	// Codecs — рядки WebCodecs, для яких плеєр отримав supported=true,
	// напр. "vp09.01.10.08.03", "av01.1.08M.08.0.000".
	Codecs []string
}

// Decision — результат переговорів.
type Decision struct {
	Codec  Codec
	Reason string
}

// IsVP9P1 — рядок кодека описує VP9 profile 1 (vp09.01.*).
func IsVP9P1(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "vp09.01.")
}

// IsAV1High — рядок описує AV1 profile 1 (av01.1.*), тобто 4:4:4.
func IsAV1High(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "av01.1.")
}

// Negotiate обирає кодек. Будь-яка невизначеність → H.264.
func Negotiate(cfg Config, caps Caps) Decision {
	if !cfg.Enabled {
		return Decision{H264, "codec444 disabled"}
	}
	has := map[Codec]bool{}
	for _, c := range caps.Codecs {
		switch {
		case IsVP9P1(c):
			has[VP9P1] = true
		case IsAV1High(c):
			has[AV1P1] = true
		}
	}
	prefer := cfg.Prefer
	if len(prefer) == 0 {
		prefer = []Codec{AV1P1, VP9P1}
	}
	for _, c := range prefer {
		if has[c] {
			return Decision{c, "player supports " + string(c)}
		}
	}
	return Decision{H264, "player lacks 4:4:4 decode"}
}

// SDPHas444 повертає 4:4:4-кодеки, оголошені у відеосекції SDP: VP9 з
// fmtp profile-id=1 та AV1 з fmtp profile=1. Відсортовано, без дублів.
// libwebrtc Chrome, наскільки відомо, VP9 profile 1 не оголошує
// (НЕ ПЕРЕВІРЕНО) — тому основний канал можливостей — Caps від WebCodecs.
func SDPHas444(sdp string) []Codec {
	rtpmap, fmtp := map[string]string{}, map[string]string{}
	inVideo := false
	for _, line := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			inVideo = strings.HasPrefix(line, "m=video")
		case !inVideo:
		case strings.HasPrefix(line, "a=rtpmap:"):
			if pt, rest, ok := strings.Cut(strings.TrimPrefix(line, "a=rtpmap:"), " "); ok {
				rtpmap[pt] = strings.ToUpper(rest)
			}
		case strings.HasPrefix(line, "a=fmtp:"):
			if pt, rest, ok := strings.Cut(strings.TrimPrefix(line, "a=fmtp:"), " "); ok {
				fmtp[pt] = rest
			}
		}
	}
	set := map[Codec]bool{}
	for pt, c := range rtpmap {
		switch {
		case strings.HasPrefix(c, "VP9/") && param(fmtp[pt], "profile-id") == "1":
			set[VP9P1] = true
		case strings.HasPrefix(c, "AV1/") && param(fmtp[pt], "profile") == "1":
			set[AV1P1] = true
		}
	}
	out := make([]Codec, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func param(fmtp, key string) string {
	for _, kv := range strings.Split(fmtp, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && strings.EqualFold(k, key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// FFmpegArgs — параметри програмного енкодера прототипу (ті самі, що міряє
// bench/quality/codec444_motion.py). Для H264 — nil: агент лишається на
// штатному енкодері.
func FFmpegArgs(c Codec) []string {
	switch c {
	case VP9P1:
		return []string{"-c:v", "libvpx-vp9", "-profile:v", "1", "-pix_fmt", "yuv444p",
			"-deadline", "realtime", "-cpu-used", "8", "-row-mt", "1", "-lag-in-frames", "0", "-error-resilient", "1"}
	case AV1P1:
		return []string{"-c:v", "libaom-av1", "-profile:v", "1", "-pix_fmt", "yuv444p",
			"-usage", "realtime", "-cpu-used", "8", "-row-mt", "1", "-lag-in-frames", "0", "-tiles", "2x2"}
	}
	return nil
}
