//go:build windows

package audio

/*
#cgo CFLAGS: -O2 -Wall -Wextra
#cgo LDFLAGS: -lole32

#include <stdlib.h>
#include "wasapi.h"
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"
)

type wasapiSource struct {
	audio *C.oos_audio
}

func newNativeSource() (nativeSource, error) {
	var source *C.oos_audio
	var errbuf [512]C.char
	status := C.oos_audio_open(&source, &errbuf[0], C.size_t(len(errbuf)))
	if status != C.OOS_AUDIO_OK {
		return nil, nativeError(status, C.GoString(&errbuf[0]))
	}
	return &wasapiSource{audio: source}, nil
}

func (s *wasapiSource) read(timeout time.Duration) (nativeFrame, error) {
	var packet C.oos_audio_packet
	var errbuf [512]C.char
	milliseconds := timeout.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	status := C.oos_audio_read(s.audio, C.uint32_t(milliseconds),
		&packet, &errbuf[0], C.size_t(len(errbuf)))
	switch status {
	case C.OOS_AUDIO_TIMEOUT:
		return nativeFrame{}, errNativeTimeout
	case C.OOS_AUDIO_OK:
		defer C.oos_audio_packet_release(&packet)
		return nativeFrame{
			data:           C.GoBytes(unsafe.Pointer(packet.data), C.int(packet.bytes)),
			frames:         int(packet.frames),
			format:         formatFromC(packet.format),
			qpc100ns:       uint64(packet.qpc_100ns),
			timestampValid: packet.timestamp_valid != 0,
		}, nil
	default:
		return nativeFrame{}, nativeError(status, C.GoString(&errbuf[0]))
	}
}

func (s *wasapiSource) format() Format {
	var format C.oos_audio_format
	C.oos_audio_get_format(s.audio, &format)
	return formatFromC(format)
}

func (s *wasapiSource) clock100ns() uint64 {
	return uint64(C.oos_audio_clock_100ns())
}

func (s *wasapiSource) close() error {
	if s.audio != nil {
		C.oos_audio_close(s.audio)
		s.audio = nil
	}
	return nil
}

func nativeError(status C.int, detail string) error {
	if detail == "" {
		detail = "unknown native error"
	}
	switch status {
	case C.OOS_AUDIO_NO_DEVICE:
		return fmt.Errorf("%w: %s", ErrNoDevice, detail)
	case C.OOS_AUDIO_CLOSED:
		return ErrClosed
	default:
		return fmt.Errorf("%w: %s", ErrNotAvailable, detail)
	}
}

func formatFromC(format C.oos_audio_format) Format {
	return Format{
		SampleRate:         int(format.sample_rate),
		Channels:           int(format.channels),
		SampleFormat:       SampleFormat(format.sample_format),
		BitsPerSample:      int(format.bits_per_sample),
		ValidBitsPerSample: int(format.valid_bits_per_sample),
		ChannelMask:        uint32(format.channel_mask),
		BytesPerFrame:      int(format.bytes_per_frame),
	}
}

// convertMixFormat exercises the same native parser/converter used immediately
// before IAudioClient::Initialize. It is kept unexported; hardware-independent
// tests feed it raw WAVEFORMATEX bytes.
func convertMixFormat(input []byte) ([]byte, Format, error) {
	if len(input) == 0 {
		return nil, Format{}, fmt.Errorf("audio: convert mix format: empty input")
	}
	in := C.CBytes(input)
	defer C.free(in)
	out := make([]byte, 64)
	var outLen C.uint32_t
	var format C.oos_audio_format
	var errbuf [256]C.char
	status := C.oos_audio_convert_format(
		in, C.uint32_t(len(input)), unsafe.Pointer(&out[0]), C.uint32_t(len(out)),
		&outLen, &format, &errbuf[0], C.size_t(len(errbuf)))
	if status != C.OOS_AUDIO_OK {
		return nil, Format{}, fmt.Errorf("audio: convert mix format: %s", C.GoString(&errbuf[0]))
	}
	return out[:int(outLen)], formatFromC(format), nil
}
