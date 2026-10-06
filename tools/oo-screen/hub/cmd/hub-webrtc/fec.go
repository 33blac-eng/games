package main

// FEC на нозі хаб->глядач (P1 / N2): ULPFEC у RED, як у libwebrtc, тож
// Chrome декодує його без field trial-ів (FlexFEC-03 Chrome приймає лише під
// WebRTC-FlexFEC-03 trial — тому не він). Код — internal/ulpfec.
//
// 🔴 OO_SCREEN_FEC=1, ТИПОВО ВИМКНЕНО. Без прапорця MediaEngine і ланцюг
// interceptor-ів бітово ті самі, що й до цього файлу.
//
// Накладні адаптивні: частка FEC на кадр — з оцінки втрат за унікальними
// NACK-ами глядача (RR FractionLost не годиться: ретрансмісія, що встигла,
// рахується отриманою). На чистій лінії FEC-пакетів нема (OO_SCREEN_FEC_MIN_LOSS=0).
//
// Env: OO_SCREEN_FEC_TARGET (ймовірність, що кадр FEC не врятує, дефолт 0.01),
// OO_SCREEN_FEC_MAX_RATE (стеля FEC/медіа, 0.5), OO_SCREEN_FEC_MIN_LOSS (0),
// OO_SCREEN_FEC_LAYOUT=2d (2D-парність, коли 1D не дотягує; типово 1D),
// OO_SCREEN_FEC_MAX_RATE_2D (стеля для 2D-сітки, 0.7).
//
// Відомі межі: (1) контролер бітрейту не знає про накладні — на вузькому
// каналі FEC додає навантаження (план TZ: «вимкнено при обмеженні каналу» —
// ще НЕ зроблено); (2) відновлені FEC пакети не потрапляють у TWCC (його на
// глядачевій нозі й так не узгоджено).

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/organicoils/oo-screen/internal/ulpfec"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

const (
	fecRedPT    = 116
	fecUlpfecPT = 117
)

var (
	fecEnabled = os.Getenv("OO_SCREEN_FEC") == "1"
	fecLegs    sync.Map // media SSRC -> ulpfec.Params
)

func registerFECCodecs(m *webrtc.MediaEngine, offerSDP string) error {
	if !fecEnabled {
		return nil
	}
	// PT беремо з offer глядача. Статичні 116/117 збігались із PT, під якими
	// Chrome пропонує rtx/H.264 (117 = H.264 4d001f): pion при неточному
	// збігу профілю ноди (4d40xx) з профілем Chrome викидав H.264 з answer —
	// глядач діставав 415 no_h264 (прод 05.10.2026, TestFECAnswerKeepsH264ForRealChromeOffer).
	redPT, fecPT := fecPTsFromOffer(offerSDP)
	for _, c := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/red", ClockRate: 90000}, PayloadType: redPT},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeUlpFEC, ClockRate: 90000}, PayloadType: fecPT},
	} {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	return nil
}

// fecPTsFromOffer — PT red/ulpfec відеосекції offer-а; якщо їх нема (нога
// агента, старий клієнт) — запасні fecRedPT/fecUlpfecPT.
func fecPTsFromOffer(sdp string) (red, fec webrtc.PayloadType) {
	red, fec = fecRedPT, fecUlpfecPT
	inVideo := false
	for _, raw := range strings.Split(sdp, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "m=") {
			inVideo = strings.HasPrefix(line, "m=video")
			continue
		}
		if !inVideo || !strings.HasPrefix(line, "a=rtpmap:") {
			continue
		}
		pt, codec, ok := strings.Cut(strings.TrimPrefix(line, "a=rtpmap:"), " ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(pt)
		if err != nil || n < 0 || n > 127 {
			continue
		}
		switch strings.ToLower(codec) {
		case "red/90000":
			red = webrtc.PayloadType(n)
		case "ulpfec/90000":
			fec = webrtc.PayloadType(n)
		}
	}
	return red, fec
}

// fecH264Fmtp — fmtp H.264, який хаб оголошує нозі під FEC. pion бере кодеки
// з ТОЧНИМ збігом fmtp, а частковий (профіль ноди 4d40xx проти 4d001f Chrome)
// лише коли точних нема. Без FEC точних нема — працює частковий; з FEC
// red/ulpfec збігаються точно і H.264 зникає з answer (415 no_h264, прод
// 05.10.2026). Тому під FEC беремо РЯДОК САМОГО offer-а (packetization-mode=1,
// h264ProfileCompatible з профілем ноди): збіг точний, потік той самий —
// глядач з тими ж/меншими constraint-прапорцями його декодує (див.
// h264ProfileCompatible), рівень покриває level-asymmetry-allowed.
// Без FEC, без offer-а чи без сумісного запису — h264FmtpFor(profile), як було.
func fecH264Fmtp(profile, offerSDP string) string {
	if !fecEnabled || offerSDP == "" {
		return h264FmtpFor(profile)
	}
	want := effectiveProfile(profile)
	rtp, fmtps, inVideo := map[string]bool{}, map[string]string{}, false
	var order []string
	for _, raw := range strings.Split(offerSDP, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "m="):
			inVideo = strings.HasPrefix(line, "m=video")
		case !inVideo:
		case strings.HasPrefix(line, "a=rtpmap:"):
			if pt, c, ok := strings.Cut(strings.TrimPrefix(line, "a=rtpmap:"), " "); ok && strings.HasPrefix(strings.ToUpper(c), "H264/") {
				rtp[pt] = true
				order = append(order, pt)
			}
		case strings.HasPrefix(line, "a=fmtp:"):
			if pt, f, ok := strings.Cut(strings.TrimPrefix(line, "a=fmtp:"), " "); ok {
				fmtps[pt] = f
			}
		}
	}
	for _, pt := range order {
		f := fmtps[pt]
		if fmtpParam(f, "packetization-mode") == "1" && h264ProfileCompatible(fmtpParam(f, "profile-level-id"), want) {
			return f
		}
	}
	return h264FmtpFor(profile)
}

func addFECInterceptor(i *interceptor.Registry) {
	if !fecEnabled {
		return
	}
	i.Add(&ulpfec.Factory{Cfg: ulpfec.Config{
		Lookup: func(ssrc uint32) (ulpfec.Params, bool) {
			v, ok := fecLegs.Load(ssrc)
			if !ok {
				return ulpfec.Params{}, false
			}
			return v.(ulpfec.Params), true
		},
		Forget:  func(ssrc uint32) { fecLegs.Delete(ssrc) },
		Target:  envFloat("OO_SCREEN_FEC_TARGET", 0),
		MaxRate: envFloat("OO_SCREEN_FEC_MAX_RATE", 0),
		MinLoss: envFloat("OO_SCREEN_FEC_MIN_LOSS", 0),
		// N2: стеля для 2D-сітки (дефолт 0.7), окремо від 1D MaxRate.
		MaxRate2D: envFloat("OO_SCREEN_FEC_MAX_RATE_2D", 0),
		// N2: на RTT 200 мс 1D-інтерлівінг упирається в MaxRate раніше, ніж
		// дає target (2 втрати в підгрупі = NACK = кадр чекає RTT).
		Layout2D: os.Getenv("OO_SCREEN_FEC_LAYOUT") == "2d",
	}})
}

// fecParamsFrom — PT red/ulpfec серед узгоджених кодеків; ok=false, якщо
// глядач хоч одного з них не запропонував (тоді нога йде без FEC/RED).
func fecParamsFrom(codecs []webrtc.RTPCodecParameters) (ulpfec.Params, bool) {
	var p ulpfec.Params
	var red, fec bool
	for _, c := range codecs {
		switch strings.ToLower(c.MimeType) {
		case "video/red":
			p.RedPT, red = uint8(c.PayloadType), true
		case strings.ToLower(webrtc.MimeTypeUlpFEC):
			p.FecPT, fec = uint8(c.PayloadType), true
		}
	}
	return p, red && fec
}

// bindViewerFEC — після SetRemoteDescription глядача: запамʼятати PT для
// SSRC відеодоріжки, щоб інтерсептор почав RED/FEC. Запис прибирає
// UnbindLocalStream інтерсептора (Forget).
func bindViewerFEC(pc *webrtc.PeerConnection) {
	if !fecEnabled {
		return
	}
	for _, s := range pc.GetSenders() {
		if s.Track() == nil || s.Track().Kind() != webrtc.RTPCodecTypeVideo {
			continue
		}
		prm := s.GetParameters()
		p, ok := fecParamsFrom(prm.Codecs)
		if !ok || len(prm.Encodings) == 0 {
			continue
		}
		p.HighestOut = new(atomic.Uint32)
		p.OutCount = new(atomic.Uint64)
		fecLegs.Store(uint32(prm.Encodings[0].SSRC), p)
	}
}

// fecHighestSeq — найновіший вихідний seq ноги з FEC (після вставок FEC він
// більший за vl.lastSeq). ok=false — FEC на цій нозі нема або ще не почався.
func fecHighestSeq(mediaSSRC uint32) (uint16, bool) {
	if !fecEnabled {
		return 0, false
	}
	v, ok := fecLegs.Load(mediaSSRC)
	if !ok {
		return 0, false
	}
	h := v.(ulpfec.Params).HighestOut
	if h == nil {
		return 0, false
	}
	x := h.Load()
	return uint16(x), x != 0
}

// legOutSent — скільки пакетів нога реально віддала у ВИХІДНОМУ просторі seq:
// з FEC це медіа + вставлені FEC (саме їх тримає буфер NACK responder-а і
// саме в цьому просторі глядач шле NACK), без FEC — vl.sent.
func legOutSent(vl *viewerLeg, mediaSSRC uint32) uint64 {
	if fecEnabled {
		if v, ok := fecLegs.Load(mediaSSRC); ok {
			if c := v.(ulpfec.Params).OutCount; c != nil {
				if n := c.Load(); n != 0 {
					return n
				}
			}
		}
	}
	return atomic.LoadUint64(&vl.sent)
}
