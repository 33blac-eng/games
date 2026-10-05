package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/organicoils/oo-screen/agent/audio"
)

func main() {
	capturer, err := audio.New()
	if err != nil {
		fmt.Printf("OPEN_ERROR: %v\n", err)
		os.Exit(2)
	}
	defer capturer.Close()

	format := capturer.Format()
	fmt.Printf("opened sample_rate=%d channels=%d sample_format=%s bits=%d valid_bits=%d bytes_per_frame=%d channel_mask=0x%X\n",
		format.SampleRate, format.Channels, format.SampleFormat,
		format.BitsPerSample, format.ValidBitsPerSample,
		format.BytesPerFrame, format.ChannelMask)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	deadline := time.Now().Add(3 * time.Second)
	for packet := 1; time.Now().Before(deadline); packet++ {
		frame, err := capturer.NextFrame(ctx)
		if err != nil {
			fmt.Printf("READ_ERROR: %v\n", err)
			os.Exit(3)
		}
		fmt.Printf("buffer=%02d sample_rate=%d channels=%d format=%s/%dbit frames=%d bytes=%d rms=%.8f timestamp=%s\n",
			packet, frame.Format.SampleRate, frame.Format.Channels,
			frame.Format.SampleFormat, frame.Format.BitsPerSample,
			frame.Frames, len(frame.Data), frame.RMS,
			frame.Timestamp.Format("15:04:05.000000"))
		time.Sleep(250 * time.Millisecond)
	}
}
