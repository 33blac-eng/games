// corpus-player-wt — агент кандидата B (WebTransport): зачитує записаний
// Annex-B корпус, зациклює його, пейсить на 60fps і шле envelope-кадри
// на hub-wt через QUIC (ALPN "oo-screen-agent"). Персистентний control-стрім
// (перший стрім з'єднання): hello, heartbeat раз/5с, і відповідь на
// keyframe_request від hub-а — стрибок до найближчого наступного IDR AU.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/organicoils/oo-screen/internal/control"
	"github.com/organicoils/oo-screen/internal/envelope"
	"github.com/organicoils/oo-screen/internal/h264"
)

const (
	agentALPN         = "oo-screen-agent"
	frameDurUs        = 16667 // 60fps, мкс (за конвенцією README: PTS = seq*16667)
	configEpoch1      = 1
	heartbeatInterval = 5 * time.Second
)

func authToken() string {
	if v := os.Getenv("OO_SCREEN_T1_TOKEN"); v != "" {
		return v
	}
	return "t1-dev-token"
}

// idrIndices повертає індекси AU-кадрів з Keyframe=true, у порядку появи.
func idrIndices(aus []h264.AU) []int {
	var out []int
	for i, au := range aus {
		if au.Keyframe {
			out = append(out, i)
		}
	}
	return out
}

// nextIDR шукає найближчий НАСТУПНИЙ IDR (строго після cur, за модулем
// довжини корпусу — якщо жодного немає далі, повертається перший IDR
// корпусу, тобто після цикл-врапу). idr мусить бути непорожнім і
// відсортованим за зростанням (саме такий idrIndices і повертає).
func nextIDR(idr []int, cur int) int {
	for _, idx := range idr {
		if idx > cur {
			return idx
		}
	}
	return idr[0]
}

func main() {
	hubAddr := flag.String("hub", "localhost:4460", "адреса hub-wt (QUIC ingest)")
	corpusPath := flag.String("corpus", "bench/corpus/corpus-1080p60.h264", "шлях до Annex-B корпусу")
	flag.Parse()

	data, err := os.ReadFile(*corpusPath)
	if err != nil {
		log.Fatalf("corpus-player-wt: read corpus: %v", err)
	}
	aus := h264.SplitAUs(data)
	if len(aus) == 0 {
		log.Fatalf("corpus-player-wt: corpus produced 0 access units: %s", *corpusPath)
	}
	idr := idrIndices(aus)
	if len(idr) == 0 {
		log.Fatalf("corpus-player-wt: corpus has no IDR access units: %s", *corpusPath)
	}
	log.Printf("corpus-player-wt: loaded %d AUs (%d IDR) from %s", len(aus), len(idr), *corpusPath)

	tlsConf := &tls.Config{
		InsecureSkipVerify: true, // T1: самопідписаний сертифікат hub-wt
		NextProtos:         []string{agentALPN},
	}
	conn, err := quic.DialAddr(context.Background(), *hubAddr, tlsConf, &quic.Config{})
	if err != nil {
		log.Fatalf("corpus-player-wt: dial hub %s: %v", *hubAddr, err)
	}
	defer conn.CloseWithError(0, "")
	log.Printf("corpus-player-wt: connected to hub %s", *hubAddr)

	// Control stream: персистентний bidi. Перше повідомлення — hello.
	ctrlStr, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatalf("corpus-player-wt: open control stream: %v", err)
	}
	var ctrlSeq uint64
	ctrlSeq++
	if err := control.Write(ctrlStr, control.Hello(authToken(), ctrlSeq)); err != nil {
		log.Fatalf("corpus-player-wt: send hello: %v", err)
	}
	log.Printf("corpus-player-wt: sent hello seq=%d", ctrlSeq)

	// keyframeReqCh — сигнал з control-читача в основний цикл програвання:
	// "перестрибни на найближчий наступний IDR". Буфер 1 і неблокуюче
	// відправлення в читачі: кілька keyframe_request поспіль до того, як
	// основний цикл встиг обробити перший, не мають накопичуватись у черзі.
	keyframeReqCh := make(chan struct{}, 1)

	go func() {
		br := bufio.NewReader(ctrlStr)
		for {
			m, err := control.ReadKnown(br, nil)
			if err != nil {
				log.Printf("corpus-player-wt: control stream done: %v", err)
				return
			}
			switch m.Type {
			case control.TypeKeyframeRequest:
				log.Printf("corpus-player-wt: received keyframe_request seq=%d", m.Seq)
				select {
				case keyframeReqCh <- struct{}{}:
				default:
					// вже є необроблений запит — цей надлишковий
				}
			default:
				log.Printf("corpus-player-wt: control msg type=%s seq=%d", m.Type, m.Seq)
			}
		}
	}()

	go func() {
		hbTicker := time.NewTicker(heartbeatInterval)
		defer hbTicker.Stop()
		var seq uint64
		for range hbTicker.C {
			seq++
			if err := control.Write(ctrlStr, control.Heartbeat(seq)); err != nil {
				log.Printf("corpus-player-wt: heartbeat write failed: %v", err)
				return
			}
		}
	}()

	// Рівно один відео-стрім на epoch (T1: одна epoch за весь запуск).
	videoStr, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatalf("corpus-player-wt: open video stream: %v", err)
	}

	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()

	var seq uint64
	i := 0
	for range ticker.C {
		select {
		case <-keyframeReqCh:
			target := nextIDR(idr, i%len(aus))
			log.Printf("keyframe_request -> jump to AU %d", target)
			i = target
		default:
		}

		au := aus[i%len(aus)]
		i++

		flags := uint8(0)
		if au.Keyframe {
			flags |= envelope.FlagKeyframe
		}
		if au.HasSPS {
			flags |= envelope.FlagConfigured
		}
		f := &envelope.Frame{
			Flags:       flags,
			ConfigEpoch: configEpoch1,
			FrameSeq:    seq,
			PTS:         seq * frameDurUs,
			Payload:     bytes.Clone(au.Data),
		}
		buf, err := f.Marshal()
		if err != nil {
			log.Fatalf("corpus-player-wt: marshal frame seq=%d: %v", seq, err)
		}
		if _, err := videoStr.Write(buf); err != nil {
			log.Fatalf("corpus-player-wt: send frame seq=%d: %v", seq, err)
		}
		if seq%300 == 0 {
			fmt.Fprintf(os.Stderr, "corpus-player-wt: sent seq=%d key=%v pts=%d\n", seq, au.Keyframe, seq*frameDurUs)
		}
		seq++
	}
}
