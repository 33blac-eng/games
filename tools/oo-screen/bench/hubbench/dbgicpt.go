package main

import (
	"sync/atomic"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// rawLog — діагностичний interceptor (HUBBENCH_DEBUG_PKTS>0): друкує перші
// пакети потоку ДО будь-якої логіки pion над TrackRemote, щоб відрізнити
// «хаб не надіслав» від «клієнт отримав, але не віддав у ReadRTP».
type rawLogFactory struct{}

func (rawLogFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &rawLog{}, nil
}

type rawLog struct{ interceptor.NoOp }

func (r *rawLog) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	var n atomic.Int64
	return interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		i, attr, err := reader.Read(b, a)
		if err == nil && n.Add(1) <= int64(debugPkts) {
			var p rtp.Packet
			if p.Unmarshal(b[:i]) == nil {
				ty, _ := nalTypes(p.Payload)
				logf("raw ssrc=%d seq=%d ts=%d nal=%v", info.SSRC, p.SequenceNumber, p.Timestamp, ty)
			}
		}
		return i, attr, err
	})
}
