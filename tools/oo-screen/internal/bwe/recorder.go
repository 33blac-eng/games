package bwe

import (
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

// RecorderFactory — pion-interceptor для відправника, який сам кодує (агент
// на прямій нозі): кожному ВІДЕО-пакету ставить transport-wide seq (якщо
// transport-cc узгоджено) і записує момент і розмір відправки в TWCC. Хаб
// цього не потребує — він штампує копії пакетів сам (delaybwe.go stamp).
//
// Без узгодженого розширення (браузер його не запропонував, або MediaEngine
// його не реєструвала) пакет іде як є — нуль змін на дроті.
type RecorderFactory struct {
	TWCC *TWCC
	Now  func() time.Time // nil = time.Now
}

// NewInterceptor — interceptor.Factory.
func (f *RecorderFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	now := f.Now
	if now == nil {
		now = time.Now
	}
	return &recorder{twcc: f.TWCC, now: now}, nil
}

type recorder struct {
	interceptor.NoOp
	twcc *TWCC
	now  func() time.Time
}

func (r *recorder) BindLocalStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	var id uint8
	for _, e := range info.RTPHeaderExtensions {
		if e.URI == sdp.TransportCCURI {
			id = uint8(e.ID)
		}
	}
	if id == 0 || r.twcc == nil || !strings.HasPrefix(strings.ToLower(info.MimeType), "video/") {
		return w
	}
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		// Спершу місце під seq (розмір пакета з розширенням), потім сам seq.
		if err := h.SetExtension(id, []byte{0, 0}); err != nil {
			return w.Write(h, payload, a)
		}
		seq := r.twcc.NextSeq(r.now(), h.MarshalSize()+len(payload))
		_ = h.SetExtension(id, []byte{byte(seq >> 8), byte(seq)})
		return w.Write(h, payload, a)
	})
}
