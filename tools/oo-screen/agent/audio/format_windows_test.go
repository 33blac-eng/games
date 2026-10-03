//go:build windows

package audio

import (
	"encoding/binary"
	"testing"
)

func TestConvertMixFormatPCM16(t *testing.T) {
	mix := waveFormat(1, 2, 44100, 16, 0)
	converted, format, err := convertMixFormat(mix)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(converted[4:8]); got != 48000 {
		t.Fatalf("sample rate = %d, want 48000", got)
	}
	if got := binary.LittleEndian.Uint32(converted[8:12]); got != 192000 {
		t.Fatalf("average bytes/sec = %d, want 192000", got)
	}
	want := Format{
		SampleRate: 48000, Channels: 2, SampleFormat: SampleFormatPCM,
		BitsPerSample: 16, ValidBitsPerSample: 16, BytesPerFrame: 4,
	}
	if format != want {
		t.Fatalf("format = %+v, want %+v", format, want)
	}
}

func TestConvertMixFormatExtensibleFloat(t *testing.T) {
	mix := waveFormat(0xfffe, 6, 44100, 32, 22)
	binary.LittleEndian.PutUint16(mix[18:20], 32)
	binary.LittleEndian.PutUint32(mix[20:24], 0x3f)
	// KSDATAFORMAT_SUBTYPE_IEEE_FLOAT.
	binary.LittleEndian.PutUint32(mix[24:28], 3)
	binary.LittleEndian.PutUint16(mix[28:30], 0)
	binary.LittleEndian.PutUint16(mix[30:32], 0x10)
	copy(mix[32:40], []byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})

	converted, format, err := convertMixFormat(mix)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(converted[4:8]); got != 48000 {
		t.Fatalf("sample rate = %d, want 48000", got)
	}
	if got := binary.LittleEndian.Uint32(converted[8:12]); got != 1152000 {
		t.Fatalf("average bytes/sec = %d, want 1152000", got)
	}
	want := Format{
		SampleRate: 48000, Channels: 6, SampleFormat: SampleFormatFloat,
		BitsPerSample: 32, ValidBitsPerSample: 32, ChannelMask: 0x3f,
		BytesPerFrame: 24,
	}
	if format != want {
		t.Fatalf("format = %+v, want %+v", format, want)
	}
}

func TestConvertMixFormatRejectsUnsupportedType(t *testing.T) {
	mix := waveFormat(6, 1, 8000, 8, 0) // WAVE_FORMAT_ALAW
	if _, _, err := convertMixFormat(mix); err == nil {
		t.Fatal("convertMixFormat() error = nil, want unsupported sample type")
	}
}

func waveFormat(tag, channels uint16, rate uint32, bits, extension uint16) []byte {
	mix := make([]byte, 18+int(extension))
	blockAlign := channels * (bits / 8)
	binary.LittleEndian.PutUint16(mix[0:2], tag)
	binary.LittleEndian.PutUint16(mix[2:4], channels)
	binary.LittleEndian.PutUint32(mix[4:8], rate)
	binary.LittleEndian.PutUint32(mix[8:12], rate*uint32(blockAlign))
	binary.LittleEndian.PutUint16(mix[12:14], blockAlign)
	binary.LittleEndian.PutUint16(mix[14:16], bits)
	binary.LittleEndian.PutUint16(mix[16:18], extension)
	return mix
}
