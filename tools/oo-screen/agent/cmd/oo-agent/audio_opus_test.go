package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/agent/audio"
	"github.com/organicoils/oo-screen/internal/opusenc"
)

// TestAudioDefaultPublishesOpusStereo — F1: без OO_SCREEN_AUDIO_CODEC агент
// оголошує opus/48000/2 зі stereo=1, і PCMU в offer-і немає.
func TestAudioDefaultPublishesOpusStereo(t *testing.T) {
	withAudioFlag(t, true)
	withAudioCodec(t, opusenc.CodecOpus)
	sdp, atrk := agentOffer(t)
	if atrk == nil {
		t.Fatal("немає доріжки")
	}
	up := strings.ToUpper(sdp)
	if !strings.Contains(up, "OPUS/48000/2") || !strings.Contains(sdp, "stereo=1") {
		t.Fatalf("у offer немає opus/48000/2 stereo=1:\n%s", sdp)
	}
	if strings.Contains(up, "PCMU/8000") {
		t.Fatalf("PCMU в offer при opus:\n%s", sdp)
	}
}

func TestDefaultCodecIsOpus(t *testing.T) {
	c, err := opusenc.Parse("")
	if err != nil || c != opusenc.CodecOpus {
		t.Fatalf("типовий кодек %v %v", c, err)
	}
}

// TestOpusEncoderStereoClock — 1 с WASAPI 48к стерео у пакетах по 10 мс =
// рівно 50 кадрів по 20 мс, годинник 48000, і канали не змішані.
func TestOpusEncoderStereoClock(t *testing.T) {
	fe, err := newFrameEncoder(opusenc.CodecOpus)
	if err != nil {
		t.Fatal(err)
	}
	var frames [][]byte
	for p := 0; p < 100; p++ {
		base := p * 480
		data, f := float48kStereo(480,
			func(i int) float64 { return 0.3 * math.Sin(2*math.Pi*9000*float64(base+i)/48000) },
			func(i int) float64 { return 0.3 * math.Sin(2*math.Pi*500*float64(base+i)/48000) })
		frames = append(frames, fe.encode(data, f)...)
	}
	if len(frames) != 50 || fe.clock() != 48000 || fe.rate() != 48000 {
		t.Fatalf("кадрів %d, годинник %d", len(frames), fe.clock())
	}
	dec, _ := opusenc.NewDecoder()
	out := make([]float32, 5760*2)
	var lHi, lLo, rHi, rLo float64
	for i, fr := range frames {
		if d := opusenc.CodecOpus.FrameDur(fr); d != 20*time.Millisecond {
			t.Fatalf("dur %v", d)
		}
		n, err := dec.Decode(fr, out)
		if err != nil {
			t.Fatal(err)
		}
		if i < 10 {
			continue
		}
		var l, r []float64
		for k := 0; k < n; k++ {
			l = append(l, float64(out[2*k]))
			r = append(r, float64(out[2*k+1]))
		}
		lHi += tonePower(l, 9000)
		lLo += tonePower(l, 500)
		rHi += tonePower(r, 9000)
		rLo += tonePower(r, 500)
	}
	if lHi < 100*lLo || rLo < 100*rHi {
		t.Fatalf("канали змішались: L 9k/500=%.1f R 500/9k=%.1f", lHi/lLo, rLo/rHi)
	}
}

func tonePower(x []float64, f float64) float64 {
	w := 2 * math.Pi * f / 48000
	c := 2 * math.Cos(w)
	var s1, s2 float64
	for _, v := range x {
		s1, s2 = v+c*s1-s2, s1
	}
	return s1*s1 + s2*s2 - c*s1*s2
}

func TestOpusSilenceKeepsClock(t *testing.T) {
	fe, _ := newFrameEncoder(opusenc.CodecOpus)
	if fr := fe.silence(4800); len(fr) != 5 || fe.clock() != 4800 {
		t.Fatalf("100мс тиші: %d кадрів, годинник %d", len(fr), fe.clock())
	}
	if g := audioGap(100*time.Millisecond, fe.clock(), audioSyncTolerance, fe.rate()); g != 0 {
		t.Fatalf("gap %d", g)
	}
	if g := audioGap(time.Second, 0, audioSyncTolerance, opusenc.Rate); g != 48000 {
		t.Fatalf("gap 1с = %d", g)
	}
}

func TestOpusLayout(t *testing.T) {
	f := audio.Format{SampleRate: 44100, Channels: 2, SampleFormat: audio.SampleFormatFloat, BitsPerSample: 32, BytesPerFrame: 8}
	if _, _, ok := audioLayout(f, opusenc.Rate); ok {
		t.Fatal("44.1 кГц прийнято")
	}
	f.SampleRate = 96000
	if factor, _, ok := audioLayout(f, opusenc.Rate); !ok || factor != 2 {
		t.Fatal("96 кГц")
	}
	// Моно 16 біт: дублюється в обидва канали, 20 мс -> рівно один кадр.
	fe, _ := newFrameEncoder(opusenc.CodecOpus)
	mono := audio.Format{SampleRate: 48000, Channels: 1, SampleFormat: audio.SampleFormatPCM, BitsPerSample: 16, ValidBitsPerSample: 16, BytesPerFrame: 2}
	if fr := fe.encode(make([]byte, 960*2), mono); len(fr) != 1 {
		t.Fatalf("моно 20мс -> %d кадрів", len(fr))
	}
}

// BenchmarkOpusAgentPath — повний шлях агента на 10 мс пакеті WASAPI
// (розбір float32 + кодування); %core — частка одного ядра від реального часу.
func BenchmarkOpusAgentPath(b *testing.B) {
	fe, _ := newFrameEncoder(opusenc.CodecOpus)
	data, f := float48kStereo(480,
		func(i int) float64 { return 0.3 * math.Sin(float64(i)/7) },
		func(i int) float64 { return 0.3 * math.Sin(float64(i)/3) })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fe.encode(data, f)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/10e6*100, "%core")
}

// TestRejectedOpusAudioDoesNotBreakVideo — хаб без OO_SCREEN_AUDIO відповідає
// "m=audio 0 ... 0". Без detachAudioTrack pion валить SetRemoteDescription
// (а з ним і відео) — з PCMU це маскував збіг PT 0. Тест повторює рівно
// послідовність dialWebRTC.
func TestRejectedOpusAudioDoesNotBreakVideo(t *testing.T) {
	withAudioFlag(t, true)
	withAudioCodec(t, opusenc.CodecOpus)

	api, err := newWebRTCAPI(h264Fmtp())
	if err != nil {
		t.Fatal(err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	h264 := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpLine}
	video, _ := webrtc.NewTrackLocalStaticSample(h264, "video", "oo-screen-agent")
	if _, err := pc.AddTrack(video); err != nil {
		t.Fatal(err)
	}
	atrk, err := addAudioTrack(pc)
	if err != nil || atrk == nil {
		t.Fatalf("addAudioTrack: %v", err)
	}
	offer, _ := pc.CreateOffer(nil)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}

	// «Хаб без прапорця»: лише H.264, жодного аудіокодека.
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: h264, PayloadType: 102}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	hub, _ := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	defer hub.Close()
	if _, err := hub.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	if err := hub.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	ans, err := hub.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !audioRejected(ans.SDP) {
		t.Fatalf("очікували відхилений m=audio:\n%s", ans.SDP)
	}
	if err := detachAudioTrack(pc, atrk); err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(ans); err != nil {
		t.Fatalf("після detach SetRemoteDescription усе одно падає: %v", err)
	}
	if audioRejected("m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n") {
		t.Fatal("живий m=audio сприйнято як відхилений")
	}
}

// BenchmarkPCMUAgentPath — те саме для запасного μ-law, для порівняння.
func BenchmarkPCMUAgentPath(b *testing.B) {
	fe, _ := newFrameEncoder(opusenc.CodecPCMU)
	data, f := float48kStereo(480,
		func(i int) float64 { return 0.3 * math.Sin(float64(i)/7) },
		func(i int) float64 { return 0.3 * math.Sin(float64(i)/3) })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fe.encode(data, f)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/10e6*100, "%core")
}
