package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/organicoils/oo-screen/internal/opusenc"
)

// TestOpusToneReachesViewer — F1: типовий кодек хаба Opus; глядач отримує
// opus/48000/2, і в доріжку йдуть справжні 20-мс Opus-пакети,
// які декодуються в стерео.
func TestOpusToneReachesViewer(t *testing.T) {
	withAudioFlag(t, true)
	withHubAudioCodec(t, opusenc.CodecOpus)

	dialAgentLegWith(t, agentNodeIDEnv, false)
	vpc, sdp, tracks := dialViewerLeg(t, true)
	// stereo=1 в answer — дзеркало offer-а глядача (pion повторює його fmtp),
	// тому його вмикає плеєр (opusStereoSdp у JS), а не хаб.
	if !strings.Contains(strings.ToUpper(sdp), "OPUS/48000/2") {
		t.Fatalf("у answer немає opus/48000/2:\n%s", sdp)
	}
	audio := awaitAudioTrack(t, vpc, tracks)
	if !strings.EqualFold(audio.Codec().MimeType, "audio/opus") || audio.Codec().Channels != 2 {
		t.Fatalf("кодек доріжки %+v", audio.Codec())
	}
	if err := audio.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	dec, _ := opusenc.NewDecoder()
	pcm := make([]float32, 5760*2)
	var prevTS uint32
	for i := 0; i < 10; i++ {
		pkt, _, err := audio.ReadRTP()
		if err != nil {
			t.Fatalf("пакет %d: %v", i, err)
		}
		if d := opusenc.PacketDuration(pkt.Payload); d != 20*time.Millisecond {
			t.Fatalf("пакет %d: тривалість %v", i, d)
		}
		if i > 0 && pkt.Timestamp-prevTS != opusenc.FrameSamples {
			t.Fatalf("крок RTP-годинника %d, want %d", pkt.Timestamp-prevTS, opusenc.FrameSamples)
		}
		prevTS = pkt.Timestamp
		if n, err := dec.Decode(pkt.Payload, pcm); err != nil || n != opusenc.FrameSamples {
			t.Fatalf("decode %d: n=%d err=%v", i, n, err)
		}
	}
}

// TestOpusAgentAudioForwardedVerbatim — пакети агента доходять до глядача
// байт-у-байт (хаб не транскодує).
func TestOpusAgentAudioForwardedVerbatim(t *testing.T) {
	withAudioFlag(t, true)
	withHubAudioCodec(t, opusenc.CodecOpus)

	_, atrk, _ := dialAgentLegWith(t, agentNodeIDEnv, true)
	if atrk == nil {
		t.Fatal("немає аудіо-доріжки агента")
	}
	// Мітка: CELT FB 20 мс (TOC 0xF8) + постійне тіло — тон такого не дає.
	marked := append([]byte{0xF8}, bytes.Repeat([]byte{0x5A}, 40)...)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = atrk.WriteSample(media.Sample{Data: marked, Duration: 20 * time.Millisecond})
			}
		}
	}()

	vpc, _, tracks := dialViewerLeg(t, true)
	audio := awaitAudioTrack(t, vpc, tracks)
	if err := audio.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	streak := 0
	for i := 0; i < 400 && streak < 3; i++ {
		pkt, _, err := audio.ReadRTP()
		if err != nil {
			t.Fatalf("пакет %d: %v", i, err)
		}
		if bytes.Equal(pkt.Payload, marked) {
			streak++
		} else {
			streak = 0
		}
	}
	if streak < 3 {
		t.Fatal("пакети агента не дійшли до глядача")
	}
}

// TestOpusMKVDecodes — запис (OO_SCREEN_RECORD) з Opus: A_OPUS + OpusHead,
// і ffmpeg реально декодує доріжку. Без корпусу H.264 (на відміну від
// TestRecordWritesClosedMKVWithBothTracks, який без нього пропускається).
func TestOpusMKVDecodes(t *testing.T) {
	withHubAudioCodec(t, opusenc.CodecOpus)
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg немає — зовнішньої перевірки НЕ БУЛО")
	}
	path := filepath.Join(t.TempDir(), "a.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	m := newMKVWriter(f)
	if err := m.writeHeader(640, 480, avcC([]byte{0x67, 0x64, 0x00, 0x2A}, []byte{0x68, 0xEE}), true); err != nil {
		t.Fatal(err)
	}
	var tone audioTone
	for i := 0; i < 100; i++ { // 2 с
		m.block(mkvAudioTrack, int64(i*20), true, audioBlock(tone.next()))
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(ff, "-v", "error", "-i", path, "-map", "0:a", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	// 2 с × 48000 × 2 канали × 2 байти, мінус pre-skip.
	if n := len(out); n < 2*48000*2*2*9/10 {
		t.Fatalf("ffmpeg видав %d байт PCM, замало для 2 с стерео", n)
	}
	probe, _ := exec.Command(strings.Replace(ff, "ffmpeg", "ffprobe", 1), "-v", "error", "-select_streams", "a",
		"-show_entries", "stream=codec_name,sample_rate,channels", "-of", "default=noprint_wrappers=1", path).Output()
	got := string(probe)
	t.Logf("ffprobe:\n%s", got)
	for _, want := range []string{"codec_name=opus", "sample_rate=48000", "channels=2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ffprobe не побачив %q:\n%s", want, got)
		}
	}
}

// TestOpusToneFramesVary — мітка з тесту вище має сенс лише якщо тон не дає
// однакових пакетів поспіль.
func TestOpusToneFramesVary(t *testing.T) {
	withHubAudioCodec(t, opusenc.CodecOpus)
	var tone audioTone
	prev := tone.next()
	for i := 0; i < 10; i++ {
		f := tone.next()
		if opusenc.PacketDuration(f) != 20*time.Millisecond {
			t.Fatalf("кадр %d: %v", i, opusenc.PacketDuration(f))
		}
		if bytes.Equal(f, prev) {
			t.Fatalf("два однакові пакети тону поспіль (%d)", i)
		}
		prev = f
	}
}
