package main

// Затримка агент -> глядач, коли агент і глядачі — ОКРЕМІ процеси (B7,
// чистий замір). У `scale -lat` агент і всі N глядачів сидять в одному
// процесі на тих самих ядрах: клієнт приймає IDR-пачку N разів і сам стає
// частиною виміряного хвоста. Тут:
//
//   hubbench agent    -sendlog F        — пише (хеш payload, час відправки)
//                                         кожного пакета у файл F;
//   hubbench latwatch -sendlog F ...    — M глядачів однієї ноди пишуть
//                                         (хеш, час прийому) у памʼять, після
//                                         вікна читають F і зводять.
//
// Годинник один (той самий хост, time.Now = CLOCK_REALTIME), тож різниця
// часів між процесами чесна. Пакети з однаковим payload відкидаються як
// неоднозначні — як і в scale -lat.

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// fileSendLog — запис (хеш, час) у файл; скидається на диск раз на 200 мс.
type fileSendLog struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func newFileSendLog(path string) (*fileSendLog, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	l := &fileSendLog{w: bufio.NewWriterSize(f, 1<<20)}
	go func() {
		for range time.Tick(200 * time.Millisecond) {
			l.mu.Lock()
			_ = l.w.Flush()
			l.mu.Unlock()
		}
	}()
	return l, nil
}

func (l *fileSendLog) put(h uint64, t int64) {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:8], h)
	binary.LittleEndian.PutUint64(b[8:], uint64(t))
	l.mu.Lock()
	_, _ = l.w.Write(b[:])
	l.mu.Unlock()
}

// readSendLog — хеш -> час відправки; хеші, що траплялись більше одного
// разу, позначені -1 (неоднозначні).
func readSendLog(path string) (map[uint64]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	m := map[uint64]int64{}
	var b [16]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			break
		}
		h := binary.LittleEndian.Uint64(b[:8])
		t := int64(binary.LittleEndian.Uint64(b[8:]))
		if _, dup := m[h]; dup {
			m[h] = -1
		} else {
			m[h] = t
		}
	}
	return m, nil
}

type recvRec struct {
	h uint64
	t int64
}

func cmdLatWatch(args []string) {
	fs := flag.NewFlagSet("latwatch", flag.ExitOnError)
	var c common
	c.register(fs)
	node := fs.String("node", "rc", "node_id")
	nv := fs.Int("viewers", 4, "глядачів у цьому процесі")
	warm := fs.Duration("warm", 5*time.Second, "прогрів після підключення")
	dur := fs.Duration("dur", 30*time.Second, "вікно вимірювання")
	sendlog := fs.String("sendlog", "", "файл агента (hubbench agent -sendlog)")
	fs.Parse(args)
	if *sendlog == "" {
		log.Fatal("потрібен -sendlog")
	}
	cor, err := loadCorpus(c.corpusPath)
	if err != nil {
		log.Fatal(err)
	}
	api, err := newAPI(cor.profile)
	if err != nil {
		log.Fatal(err)
	}
	var (
		mu  sync.Mutex
		on  bool
		rec = make([]recvRec, 0, 1<<20)
	)
	var vs []*viewer
	for i := 0; i < *nv; i++ {
		v, err := startViewer(api, c.hub, offerBody{Ticket: ticket(*node)}, nil, func(_ *viewer, p *rtp.Packet, now int64) {
			h := payloadHash(p.Payload)
			mu.Lock()
			if on {
				rec = append(rec, recvRec{h, now})
			}
			mu.Unlock()
		})
		if err != nil {
			log.Fatal(err)
		}
		vs = append(vs, v)
	}
	time.Sleep(*warm)
	var lost0, got0 uint64
	for _, v := range vs {
		lost0 += v.lost.Load()
		got0 += v.pkts.Load()
	}
	mu.Lock()
	on = true
	mu.Unlock()
	time.Sleep(*dur)
	mu.Lock()
	on = false
	all := rec
	mu.Unlock()
	var lost, got uint64
	silent := 0
	for _, v := range vs {
		lost += v.lost.Load()
		got += v.pkts.Load()
		if v.pkts.Load() == 0 {
			silent++
		}
	}
	lost, got = lost-lost0, got-got0
	time.Sleep(time.Second) // агент скидає лог раз на 200 мс
	sent, err := readSendLog(*sendlog)
	if err != nil {
		log.Fatal(err)
	}
	lat := make([]float64, 0, len(all))
	for _, r := range all {
		if t0, ok := sent[r.h]; ok && t0 > 0 {
			if d := float64(r.t-t0) / 1e6; d >= 0 && d < 5000 {
				lat = append(lat, d)
			}
		}
	}
	lossPct := 0.0
	if got+lost > 0 {
		lossPct = float64(lost) / float64(got+lost) * 100
	}
	fmt.Printf("LATWATCH viewers=%d silent=%d samples=%d loss=%.3f%% p50=%.3f p95=%.3f p99=%.3f max=%.3f\n",
		*nv, silent, len(lat), lossPct, percentile(lat, 50), percentile(lat, 95), percentile(lat, 99), percentile(lat, 100))
	// Сирі затримки — для зведення по кількох процесах-глядачах.
	if p := os.Getenv("HUBBENCH_LAT_OUT"); p != "" {
		f, err := os.Create(p)
		if err == nil {
			w := bufio.NewWriter(f)
			for _, d := range lat {
				fmt.Fprintf(w, "%.4f\n", d)
			}
			w.Flush()
			f.Close()
		}
	}
	os.Exit(0)
}

// payloadHash — детермінований між процесами (maphash у rtc.go має
// випадковий seed на процес і тут не годиться).
func payloadHash(p []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(p)
	return h.Sum64()
}
