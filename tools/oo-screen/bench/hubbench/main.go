// hubbench — вимірювач продуктивності hub-webrtc на одній машині.
//
// Режими (перший аргумент):
//
//	erp       фейковий ERP: consume квитків "<node>~<будь-що>" -> node_id,
//	          порожні revocations. Потрібен, щоб хаб працював у ticket-режимі —
//	          лише в ньому глядач обирає НОДУ (у T1 усі глядачі на одній ноді).
//	scale     N нод x V глядачів, корпус 8 Мбіт/с; CPU/RSS/горутини хаба,
//	          втрати, бітрейт глядача, (опційно) затримка агент->глядач.
//	ttff      один агент, послідовні нові глядачі: offer -> перший повний IDR.
//	baseline  агент -> глядач НАПРЯМУ, без хаба: та сама затримка для відліку.
//	agent     окремий процес-агент (для kill -9 у тесті реконекту).
//	watch     окремий процес-глядачі: друкує JSON-події «потік став/відновився».
//	soak      N нод, глядачі приходять і йдуть; CSV із RSS/горутинами хаба.
//
// Глядачі pion recvonly, нічого не декодують. Клієнт і хаб на одній машині —
// див. bench/RESULTS-hub.md, розділ «Обмеження».
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hubbench erp|scale|ttff|baseline|agent|watch|soak [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "erp":
		cmdERP(args)
	case "scale":
		cmdScale(args)
	case "ttff":
		cmdTTFF(args)
	case "baseline":
		cmdBaseline(args)
	case "agent":
		cmdAgent(args)
	case "watch":
		cmdWatch(args)
	case "soak":
		cmdSoak(args)
	default:
		log.Fatalf("unknown mode %q", cmd)
	}
}

type common struct {
	hub, token, corpusPath, pprof string
	hubPID                        int
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.hub, "hub", "http://127.0.0.1:4471", "база URL хаба")
	fs.StringVar(&c.token, "token", os.Getenv("OO_SCREEN_T1_TOKEN"), "агентський токен (OO_SCREEN_T1_TOKEN хаба)")
	fs.StringVar(&c.corpusPath, "corpus", "", "Annex-B H.264 корпус")
	fs.StringVar(&c.pprof, "pprof", "127.0.0.1:6061", "OO_SCREEN_PPROF_ADDR хаба (горутини)")
	fs.IntVar(&c.hubPID, "pid", 0, "PID хаба (CPU/RSS із /proc)")
}

func ticket(node string) string { return fmt.Sprintf("%s~%d", node, rand.Int63()) }

// ---------------------------------------------------------------- erp

// erpConsumeNode — node_id з квитка "<node>~<suffix>".
func erpConsumeNode(ticket string) string {
	node, _, _ := strings.Cut(ticket, "~")
	return node
}

func cmdERP(args []string) {
	fs := flag.NewFlagSet("erp", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:4499", "адреса")
	fs.Parse(args)
	mux := http.NewServeMux()
	mux.HandleFunc("/remote-access/screen/internal/consume", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ticket string `json:"ticket"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"user_id": "bench", "org_id": "bench", "node_id": erpConsumeNode(req.Ticket), "grant": "view"})
	})
	mux.HandleFunc("/remote-access/screen/revocations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	})
	log.Printf("fake ERP on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// ---------------------------------------------------------------- scale

type scaleResult struct {
	Nodes, ViewersPerNode          int
	ViewersOK, Failed              int
	Silent                         int // глядачі, що за вікно не отримали жодного пакета
	CorpusMbps                     float64
	HubCPU                         float64 // % (100 = ядро), середнє за вікно
	HubRSSMB, HubRSSMaxMB          float64
	HubThreads                     int
	Goroutines                     int
	ClientCPU                      float64
	RecvMbpsPerViewer              float64 // середнє
	RecvMbpsMin                    float64
	LossPct                        float64
	LatP50, LatP95, LatP99, LatMax float64
	LatSamples                     int
	PLIs                           int64
	// Машина спільна: скільки CPU за вікно було вільним у всієї системи і
	// скільки UDP-датаграм ядро викинуло через повний буфер приймача.
	SysIdlePct   float64
	RcvbufErrors uint64
}

func cmdScale(args []string) {
	fs := flag.NewFlagSet("scale", flag.ExitOnError)
	var c common
	c.register(fs)
	nodes := fs.Int("nodes", 1, "нод")
	vpn := fs.Int("viewers", 1, "глядачів на ноду")
	dur := fs.Duration("dur", 30*time.Second, "вікно вимірювання")
	warm := fs.Duration("warm", 5*time.Second, "прогрів після підключення всіх")
	lat := fs.Bool("lat", false, "міряти затримку агент->глядач")
	gap := fs.Duration("gap", 15*time.Millisecond, "пауза між підключеннями")
	idle := fs.Duration("idle", 0, "лише ноди БЕЗ глядачів: знімок через стільки часу, потім вихід")
	fs.Parse(args)

	cor, err := loadCorpus(c.corpusPath)
	if err != nil {
		log.Fatal(err)
	}
	api, err := newAPI(cor.profile)
	if err != nil {
		log.Fatal(err)
	}
	logs := make([]*sendLog, *nodes)
	agents := make([]*agent, *nodes)
	for i := range agents {
		if *lat {
			logs[i] = &sendLog{m: map[uint64]int64{}}
		}
		a, err := startAgent(api, cor, c.hub, c.token, fmt.Sprintf("n%03d", i), logs[i], true)
		if err != nil {
			log.Fatalf("agent %d: %v", i, err)
		}
		agents[i] = a
		time.Sleep(*gap)
	}
	log.Printf("%d agents up", *nodes)
	if *idle > 0 {
		time.Sleep(*idle)
		h, _ := readProc(c.hubPID)
		fmt.Printf("IDLE nodes=%d rssMB=%.1f goroutines=%d threads=%d\n", *nodes, float64(h.rssKB)/1024, goroutines(c.pprof), h.threads)
		return
	}
	var (
		mu      sync.Mutex
		viewers []*viewer
		failed  int
	)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for k := 0; k < *vpn; k++ {
		for i := 0; i < *nodes; i++ {
			node := fmt.Sprintf("n%03d", i)
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				v, err := startViewer(api, c.hub, offerBody{Ticket: ticket(node)}, logs[i], nil)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failed++
					if failed < 5 {
						log.Printf("viewer %s: %v", node, err)
					}
					return
				}
				viewers = append(viewers, v)
			}(i)
			time.Sleep(*gap)
		}
	}
	wg.Wait()
	log.Printf("%d viewers up, %d failed; warm %s", len(viewers), failed, *warm)
	time.Sleep(*warm)

	type snap struct{ pkts, bytes, lost uint64 }
	snaps := make([]snap, len(viewers))
	for i, v := range viewers {
		snaps[i] = snap{v.pkts.Load(), v.bytes.Load(), v.lost.Load()}
		v.takeLat()
		v.latOn.Store(*lat)
	}
	var pli0 int64
	for _, a := range agents {
		pli0 += a.pli.Load()
	}
	h0, _ := readProc(c.hubPID)
	sys0, rb0 := readSys()
	s0, _ := readProc(os.Getpid())
	var rssMax uint64
	var gr int
	end := time.Now().Add(*dur)
	for time.Now().Before(end) {
		time.Sleep(time.Second)
		if h, err := readProc(c.hubPID); err == nil && h.rssKB > rssMax {
			rssMax = h.rssKB
		}
	}
	h1, _ := readProc(c.hubPID)
	sys1, rb1 := readSys()
	s1, _ := readProc(os.Getpid())
	gr = goroutines(c.pprof)
	el := h1.at.Sub(h0.at).Seconds()

	r := scaleResult{Nodes: *nodes, ViewersPerNode: *vpn, ViewersOK: len(viewers), Failed: failed, CorpusMbps: cor.mbps}
	r.HubCPU = cpuPct(h0, h1)
	r.ClientCPU = cpuPct(s0, s1)
	r.HubRSSMB = float64(h1.rssKB) / 1024
	r.HubRSSMaxMB = float64(rssMax) / 1024
	r.HubThreads = h1.threads
	r.Goroutines = gr
	var all []float64
	var rates []float64
	var got, lost uint64
	for i, v := range viewers {
		dp := v.pkts.Load() - snaps[i].pkts
		dl := v.lost.Load() - snaps[i].lost
		db := v.bytes.Load() - snaps[i].bytes
		if dp == 0 {
			r.Silent++
		}
		got += dp
		lost += dl
		rates = append(rates, float64(db)*8/el/1e6)
		all = append(all, v.takeLat()...)
	}
	r.RecvMbpsPerViewer = mean(rates)
	r.RecvMbpsMin = percentile(rates, 0)
	if got+lost > 0 {
		r.LossPct = float64(lost) / float64(got+lost) * 100
	}
	r.LatSamples = len(all)
	r.LatP50, r.LatP95, r.LatP99, r.LatMax = percentile(all, 50), percentile(all, 95), percentile(all, 99), percentile(all, 100)
	for _, a := range agents {
		r.PLIs += a.pli.Load()
	}
	r.PLIs -= pli0
	r.SysIdlePct = sys1.idlePct(sys0)
	r.RcvbufErrors = rb1 - rb0
	b, _ := json.Marshal(r)
	fmt.Println("RESULT " + string(b))
	for _, v := range viewers {
		v.close()
	}
	for _, a := range agents {
		a.close()
	}
}

// ---------------------------------------------------------------- ttff

func cmdTTFF(args []string) {
	fs := flag.NewFlagSet("ttff", flag.ExitOnError)
	var c common
	c.register(fs)
	n := fs.Int("n", 30, "скільки приєднань")
	pli := fs.Bool("pli", true, "агент відповідає на PLI позачерговим IDR")
	settle := fs.Duration("settle", 4*time.Second, "пауза після старту агента")
	spread := fs.Duration("spread", 2*time.Second, "розкид фази приєднання (ставити = GOP корпусу)")
	bg := fs.Bool("bg", true, "тримати фоновий глядач на ноді (нода вже «дивиться»)")
	fs.Parse(args)
	cor, err := loadCorpus(c.corpusPath)
	if err != nil {
		log.Fatal(err)
	}
	api, _ := newAPI(cor.profile)
	a, err := startAgent(api, cor, c.hub, c.token, "ttff", nil, *pli)
	if err != nil {
		log.Fatal(err)
	}
	defer a.close()
	// Фоновий глядач: хаб пише в GOP-кеш лише поки є живий глядач
	// (forwardToViewers рано виходить без них) — як у проді, де нода, на яку
	// приходить ДРУГИЙ глядач, уже кимось дивиться. Окремо міряємо і випадок
	// «перший глядач ноди» (-n з -bg=false).
	if *bg {
		b, err := startViewer(api, c.hub, offerBody{Ticket: ticket("ttff")}, nil, nil)
		if err != nil {
			log.Fatalf("bg viewer: %v", err)
		}
		defer b.close()
	}
	time.Sleep(*settle)
	var ans, conn, first, idr []float64
	timeouts := 0
	for k := 0; k < *n; k++ {
		v, err := startViewer(api, c.hub, offerBody{Ticket: ticket("ttff")}, nil, nil)
		if err != nil {
			log.Fatalf("viewer: %v", err)
		}
		dl := time.Now().Add(15 * time.Second)
		for v.tFirstIDR.Load() == 0 && time.Now().Before(dl) {
			time.Sleep(2 * time.Millisecond)
		}
		if v.tFirstIDR.Load() == 0 {
			timeouts++
		} else {
			ans = append(ans, float64(v.tAnswer.Sub(v.tStart).Microseconds())/1000)
			conn = append(conn, ms(v.tStart, v.tConn.Load()))
			first = append(first, ms(v.tStart, v.tFirstPkt.Load()))
			idr = append(idr, ms(v.tStart, v.tFirstIDR.Load()))
		}
		v.close()
		// Випадкова фаза приєднання відносно GOP: пауза рівномірна на
		// [0.3 с, 0.3 с + spread]. spread має бути >= GOP, інакше кожне
		// наступне приєднання стоїть у тій самій фазі після IDR попереднього.
		time.Sleep(300*time.Millisecond + time.Duration(rand.Int63n(int64(*spread))))
	}
	row := func(name string, xs []float64) {
		fmt.Printf("%-22s p50=%7.1f p90=%7.1f max=%7.1f mean=%7.1f ms\n", name, percentile(xs, 50), percentile(xs, 90), percentile(xs, 100), mean(xs))
	}
	fmt.Printf("TTFF n=%d ok=%d timeouts=%d pli_honored=%v agent_PLIs=%d\n", *n, len(idr), timeouts, *pli, a.pli.Load())
	row("offer->answer", ans)
	row("offer->connected", conn)
	row("offer->first RTP", first)
	row("offer->first full IDR", idr)
}

// ---------------------------------------------------------------- baseline

// cmdBaseline — те саме агент->глядач, але PeerConnection-и з'єднані напряму,
// без хаба. Різниця з scale -lat = скільки додає саме хаб.
func cmdBaseline(args []string) {
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	path := fs.String("corpus", "", "корпус")
	dur := fs.Duration("dur", 30*time.Second, "вікно")
	fs.Parse(args)
	cor, err := loadCorpus(*path)
	if err != nil {
		log.Fatal(err)
	}
	api, _ := newAPI(cor.profile)
	sent := &sendLog{m: map[uint64]int64{}}
	apc, _ := api.NewPeerConnection(webrtc.Configuration{})
	tr, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fmtp(cor.profile)}, "video", "b")
	apc.AddTrack(tr)
	vpc, _ := api.NewPeerConnection(webrtc.Configuration{})
	vpc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	v := &viewer{pc: vpc, tStart: time.Now()}
	var lat []float64
	var lmu sync.Mutex
	vpc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			now := time.Now().UnixNano()
			if t0, ok := sent.get(p.Payload); ok && v.latOn.Load() {
				lmu.Lock()
				lat = append(lat, float64(now-t0)/1e6)
				lmu.Unlock()
			}
		}
	})
	off, _ := vpc.CreateOffer(nil)
	g := webrtc.GatheringCompletePromise(vpc)
	vpc.SetLocalDescription(off)
	<-g
	apc.SetRemoteDescription(*vpc.LocalDescription())
	ans, _ := apc.CreateAnswer(nil)
	g2 := webrtc.GatheringCompletePromise(apc)
	apc.SetLocalDescription(ans)
	<-g2
	vpc.SetRemoteDescription(*apc.LocalDescription())
	a := &agent{pc: apc, track: tr, c: cor, sent: sent, stop: make(chan struct{})}
	go a.run()
	time.Sleep(3 * time.Second)
	v.latOn.Store(true)
	time.Sleep(*dur)
	v.latOn.Store(false)
	lmu.Lock()
	defer lmu.Unlock()
	fmt.Printf("BASELINE samples=%d p50=%.3f p95=%.3f p99=%.3f max=%.3f ms\n", len(lat), percentile(lat, 50), percentile(lat, 95), percentile(lat, 99), percentile(lat, 100))
}

// ---------------------------------------------------------------- agent / watch

func cmdAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	var c common
	c.register(fs)
	node := fs.String("node", "rc", "node_id")
	fs.Parse(args)
	t0 := time.Now()
	cor, err := loadCorpus(c.corpusPath)
	if err != nil {
		log.Fatal(err)
	}
	api, _ := newAPI(cor.profile)
	if _, err := startAgent(api, cor, c.hub, c.token, *node, nil, true); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("{\"ev\":\"agent_streaming\",\"t\":%d,\"start\":%d}\n", time.Now().UnixNano(), t0.UnixNano())
	select {}
}

// cmdWatch — глядачі однієї ноди. Подія stall: пакетів немає довше за -gap;
// resume: перший пакет після паузи і перший ПОВНИЙ IDR після неї.
func cmdWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	var c common
	c.register(fs)
	node := fs.String("node", "rc", "node_id")
	nv := fs.Int("viewers", 3, "глядачів")
	gapD := fs.Duration("gap", 200*time.Millisecond, "пауза, що вважається обривом")
	fs.Parse(args)
	cor, err := loadCorpus(c.corpusPath)
	if err != nil {
		log.Fatal(err)
	}
	api, _ := newAPI(cor.profile)
	var outMu sync.Mutex
	emit := func(m map[string]any) {
		b, _ := json.Marshal(m)
		outMu.Lock()
		fmt.Println(string(b))
		outMu.Unlock()
	}
	type st struct {
		prev    int64
		waiting bool
		pktAt   int64
		idr     idrTracker
	}
	var vs []*viewer
	for i := 0; i < *nv; i++ {
		id := i
		s := &st{}
		v, err := startViewer(api, c.hub, offerBody{Ticket: ticket(*node)}, nil, func(v *viewer, p *rtp.Packet, now int64) {
			if s.prev != 0 && now-s.prev > gapD.Nanoseconds() {
				s.waiting, s.pktAt, s.idr = true, now, idrTracker{}
				emit(map[string]any{"ev": "stall", "viewer": id, "last": s.prev, "resume_pkt": now})
			}
			s.prev = now
			if s.waiting && s.idr.feed(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload) {
				s.waiting = false
				emit(map[string]any{"ev": "resume_idr", "viewer": id, "t": now, "resume_pkt": s.pktAt})
			}
		})
		if err != nil {
			log.Fatal(err)
		}
		vs = append(vs, v)
		v.pc.OnConnectionStateChange(func(ps webrtc.PeerConnectionState) {
			emit(map[string]any{"ev": "pc_state", "viewer": id, "state": ps.String(), "t": time.Now().UnixNano()})
		})
	}
	emit(map[string]any{"ev": "watching", "t": time.Now().UnixNano()})
	select {}
}

// ---------------------------------------------------------------- soak

func cmdSoak(args []string) {
	fs := flag.NewFlagSet("soak", flag.ExitOnError)
	var c common
	c.register(fs)
	nodes := fs.Int("nodes", 10, "нод")
	maxV := fs.Int("max", 4, "макс. глядачів на ноду")
	dur := fs.Duration("dur", 25*time.Minute, "тривалість")
	every := fs.Duration("every", 1500*time.Millisecond, "темп churn: одна дія на стільки")
	sample := fs.Duration("sample", 15*time.Second, "крок семплу")
	out := fs.String("csv", "soak-hub.csv", "CSV")
	fs.Parse(args)
	cor, err := loadCorpus(c.corpusPath)
	if err != nil {
		log.Fatal(err)
	}
	api, _ := newAPI(cor.profile)
	var agents []*agent
	for i := 0; i < *nodes; i++ {
		a, err := startAgent(api, cor, c.hub, c.token, fmt.Sprintf("s%03d", i), nil, true)
		if err != nil {
			log.Fatal(err)
		}
		agents = append(agents, a)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, "t_s,phase,viewers,joins,leaves,join_fail,hub_rss_mb,goroutines,threads,hub_cpu_pct,client_rss_mb,heap_inuse_mb")
	per := make([][]*viewer, *nodes)
	var joins, leaves, fails atomic.Int64
	count := func() int {
		n := 0
		for _, l := range per {
			n += len(l)
		}
		return n
	}
	start := time.Now()
	prev, _ := readProc(c.hubPID)
	write := func(phase string) {
		h, _ := readProc(c.hubPID)
		me, _ := readProc(os.Getpid())
		fmt.Fprintf(f, "%.0f,%s,%d,%d,%d,%d,%.1f,%d,%d,%.1f,%.1f,%.1f\n", time.Since(start).Seconds(), phase, count(), joins.Load(), leaves.Load(), fails.Load(),
			float64(h.rssKB)/1024, goroutines(c.pprof), h.threads, cpuPct(prev, h), float64(me.rssKB)/1024, heapInuseMB(c.pprof))
		f.Sync()
		prev = h
	}
	write("start")
	nextSample := time.Now().Add(*sample)
	for time.Since(start) < *dur {
		i := rand.Intn(*nodes)
		if len(per[i]) < *maxV && (len(per[i]) == 0 || rand.Intn(2) == 0) {
			v, err := startViewer(api, c.hub, offerBody{Ticket: ticket(fmt.Sprintf("s%03d", i))}, nil, nil)
			if err != nil {
				fails.Add(1)
			} else {
				per[i] = append(per[i], v)
				joins.Add(1)
			}
		} else if len(per[i]) > 0 {
			j := rand.Intn(len(per[i]))
			per[i][j].close()
			per[i] = append(per[i][:j], per[i][j+1:]...)
			leaves.Add(1)
		}
		time.Sleep(*every)
		if time.Now().After(nextSample) {
			write("churn")
			nextSample = time.Now().Add(*sample)
		}
	}
	for i := range per {
		for _, v := range per[i] {
			v.close()
			leaves.Add(1)
		}
		per[i] = nil
	}
	for k := 0; k < 6; k++ {
		time.Sleep(10 * time.Second)
		write("drain")
	}
	for _, a := range agents {
		a.close()
	}
	for k := 0; k < 6; k++ {
		time.Sleep(10 * time.Second)
		write("no_agents")
	}
}
