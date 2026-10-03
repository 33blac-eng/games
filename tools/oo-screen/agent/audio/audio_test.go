package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestRMSLevel(t *testing.T) {
	t.Run("PCM16 signal", func(t *testing.T) {
		data := make([]byte, 4)
		negativeFullScale := int16(-32768)
		binary.LittleEndian.PutUint16(data[0:], uint16(negativeFullScale))
		binary.LittleEndian.PutUint16(data[2:], uint16(int16(16384)))
		format := Format{
			SampleRate: 48000, Channels: 1, SampleFormat: SampleFormatPCM,
			BitsPerSample: 16, ValidBitsPerSample: 16, BytesPerFrame: 2,
		}
		want := math.Sqrt((1 + 0.25) / 2)
		if got := rmsLevel(data, format); math.Abs(got-want) > 1e-12 {
			t.Fatalf("rmsLevel() = %.12f, want %.12f", got, want)
		}
	})

	t.Run("float32 signal", func(t *testing.T) {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data[0:], math.Float32bits(0.25))
		binary.LittleEndian.PutUint32(data[4:], math.Float32bits(-0.75))
		format := Format{
			SampleRate: 48000, Channels: 1, SampleFormat: SampleFormatFloat,
			BitsPerSample: 32, ValidBitsPerSample: 32, BytesPerFrame: 4,
		}
		want := math.Sqrt((0.25*0.25 + 0.75*0.75) / 2)
		if got := rmsLevel(data, format); math.Abs(got-want) > 1e-7 {
			t.Fatalf("rmsLevel() = %.9f, want %.9f", got, want)
		}
	})

	t.Run("silence is zero", func(t *testing.T) {
		format := Format{
			SampleRate: 48000, Channels: 2, SampleFormat: SampleFormatFloat,
			BitsPerSample: 32, ValidBitsPerSample: 32, BytesPerFrame: 8,
		}
		if got := rmsLevel(make([]byte, 8*480), format); got != 0 {
			t.Fatalf("silent rmsLevel() = %v, want 0", got)
		}
	})
}

func TestNoDeviceErrorPath(t *testing.T) {
	openFailure := func() (nativeSource, error) {
		return nil, fmt.Errorf(
			"%w: GetDefaultAudioEndpoint failed: HRESULT 0x80070490 (Element not found.)",
			ErrNoDevice,
		)
	}
	_, err := openWith(openFailure)
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("errors.Is(%v, ErrNoDevice) = false", err)
	}
	const want = "audio: open loopback: audio: no default render device: GetDefaultAudioEndpoint failed: HRESULT 0x80070490 (Element not found.)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}
