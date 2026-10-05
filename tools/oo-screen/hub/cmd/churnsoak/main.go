// churnsoak — стиснутий soak хаба (R5): глядачі заходять і виходять, агенти
// перепідключаються з високою частотою, а харнес стежить за RSS, купою після
// примусового GC і кількістю горутин хаба.
//
// ЧОМУ СТИСНУТИЙ. Витік у хабі — це залишок на КОЖНУ подію (вхід/вихід ноги,
// перепідключення агента, нова нода), а не функція годинника. Тож добу
// реального парку можна відтворити за години, якщо прогнати ту саму кількість
// подій щільніше. Чого так НЕ перевірити: ефектів, що залежать саме від часу
// (таймери на години, ротація файлів за добою, фрагментація під довгим
// низьким навантаженням). Звіт друкує коефіцієнт стиснення відносно
// припущеного «реального дня» (-day-viewers / -day-agents), щоб цифра не
// видавала себе за справжні 24 год.
//
// ЯК ВІДРІЗНИТИ ВИТІК ВІД ПРОГРІВУ. Кожні -quiet-every харнес закриває ВСІ ноги
// (і глядачів, і агентів), чекає -quiet-wait і знімає «тиху точку»: горутини,
// HeapAlloc ПІСЛЯ GC (/debug/pprof/heap?gc=1) і RSS. У тихій точці хаб не
// тримає жодної сесії, тож будь-який стійкий ріст між тихими точками — це
// залишок подій, а не робоча памʼять. Ріст оцінюється лінійною регресією по
// тихих точках (без першої — прогрів).
//
// Глядачі на багатьох нодах потребують ticket-режиму, тому харнес піднімає
// фейковий ERP (consume повертає claims ноди з квитка, revocations — []).
//
// ЗАПУСК: див. run.sh поруч.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

var (
	hubURL     = flag.String("hub", "http://127.0.0.1:4471", "база URL хаба")
	metricsURL = flag.String("metrics", "http://127.0.0.1:4472/metrics", "/metrics хаба")
	pprofURL   = flag.String("pprof", "http://127.0.0.1:4473", "pprof хаба (порожній = без GC-точок)")
	erpAddr    = flag.String("erp", "127.0.0.1:4474", "адреса фейкового ERP (хаб: OO_SCREEN_ERP_BASE)")
	authToken  = flag.String("token", "soak-token", "OO_SCREEN_T1_TOKEN хаба (легасі-токен агента)")
	dur        = flag.Duration("dur", time.Hour, "тривалість прогону")
	nodes      = flag.Int("nodes", 8, "постійних нод")
	viewers    = flag.Int("viewers", 16, "паралельних «слотів» глядачів")
	vHoldMin   = flag.Duration("vhold-min", 1*time.Second, "мін. час життя глядача")
	vHoldMax   = flag.Duration("vhold-max", 8*time.Second, "макс. час життя глядача")
	aHoldMin   = flag.Duration("ahold-min", 10*time.Second, "мін. час життя агента до перепідключення")
	aHoldMax   = flag.Duration("ahold-max", 40*time.Second, "макс. час життя агента")
	newNodePct = flag.Int("new-node-pct", 10, "%% перепідключень агента під НОВИМ node_id (ріст парку)")
	pps        = flag.Int("pps", 120, "пакетів/с на агента")
	size       = flag.Int("size", 900, "payload, байт")
	sampleEv   = flag.Duration("sample", 30*time.Second, "крок семплу під навантаженням")
	quietEv    = flag.Duration("quiet-every", 10*time.Minute, "як часто знімати тиху точку")
	quietWait  = flag.Duration("quiet-wait", 45*time.Second, "пауза після закриття всіх ніг перед тихою точкою")
	outDir     = flag.String("out", "soak-out", "куди писати CSV і дампи горутин")
	dayViewers = flag.Int("day-viewers", 2000, "припущення: глядацьких сесій за реальну добу")
	dayAgents  = flag.Int("day-agents", 300, "припущення: перепідключень агентів за реальну добу")
)

var (
	statViewerOK, statViewerFail, statViewerMedia atomic.Int64
	statAgentOK, statAgentFail                    atomic.Int64
	statNewNodes                                  atomic.Int64
	paused                                        atomic.Bool
	ticketSeq                                     atomic.Int64
)

// ---------- фейковий ERP ----------

func startFakeERP(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/remote-access/screen/internal/consume", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ticket string `json:"ticket"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// квиток = "<node>|<seq>"
		node, _, ok := strings.Cut(req.Ticket, "|")
		if !ok {
			http.Error(w, "bad", 403)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "u-soak", "org_id": "o-soak", "node_id": node, "grant": "view"})
	})
	mux.HandleFunc("/remote-access/screen/revocations", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("fake erp: %v", err)
	}
	go func() { _ = http.Serve(ln, mux) }()
}

// ---------- WebRTC ----------

var apiOnce sync.Once
var sharedAPI *webrtc.API

func api() *webrtc.API {
	apiOnce.Do(func() {
		m := &webrtc.MediaEngine{}
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpLine,
				RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}},
			},
			PayloadType: 102,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			log.Fatal(err)
		}
		i := &interceptor.Registry{}
		if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
			log.Fatal(err)
		}
		se := webrtc.SettingEngine{}
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		sharedAPI = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(se))
	})
	return sharedAPI
}

var httpc = &http.Client{Timeout: 15 * time.Second}

func negotiate(pc *webrtc.PeerConnection, path string, body map[string]any) error {
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gather:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("gather timeout")
	}
	body["sdp"] = pc.LocalDescription().SDP
	buf, _ := json.Marshal(body)
	resp, err := httpc.Post(*hubURL+path, "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s %s", path, resp.Status, bytes.TrimSpace(raw))
	}
	var ans struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return err
	}
	return pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP})
}

func waitConnected(pc *webrtc.PeerConnection, d time.Duration) bool {
	ch := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(ch) })
		}
	})
	if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// runAgent — одна сесія агента ноди node; повертає, коли hold минув або stop.
func runAgent(node string, hold time.Duration, stop <-chan struct{}) error {
	pc, err := api().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	defer pc.Close()
	trk, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpLine,
	}, "video", "soak")
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(trk)
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	bps := uint64(*pps) * uint64(*size+12) * 8
	if err := negotiate(pc, "/offer/agent", map[string]any{"token": *authToken, "node": node, "bitrate": bps}); err != nil {
		return err
	}
	if !waitConnected(pc, 10*time.Second) {
		return fmt.Errorf("agent %s: not connected", node)
	}
	statAgentOK.Add(1)
	publish(trk, hold, stop)
	return nil
}

// publish ллє 30 кадрів/с: кожні 2 с SPS+PPS+IDR (кілька FU-A не потрібні —
// одиночні NAL), решта — P-слайси. Пакетів на кадр = pps/30.
func publish(trk *webrtc.TrackLocalStaticRTP, hold time.Duration, stop <-chan struct{}) {
	end := time.After(hold)
	tick := time.NewTicker(time.Second / 30)
	defer tick.Stop()
	perFrame := *pps / 30
	if perFrame < 1 {
		perFrame = 1
	}
	payload := make([]byte, *size)
	rand.Read(payload)
	var seq uint16
	ssrc := rand.Uint32()
	frame := 0
	send := func(nal byte, ts uint32, marker bool, n int) bool {
		p := payload[:n]
		p[0] = nal
		return trk.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 102, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc, Marker: marker}, Payload: p}) == nil
	}
	for {
		select {
		case <-stop:
			return
		case <-end:
			return
		case <-tick.C:
		}
		ts := uint32(frame * 3000)
		if frame%60 == 0 {
			send(0x67, ts, false, 16)
			seq++
			send(0x68, ts, false, 8)
			seq++
			for i := 0; i < perFrame*3; i++ {
				if !send(0x65, ts, i == perFrame*3-1, *size) {
					return
				}
				seq++
			}
		} else {
			for i := 0; i < perFrame; i++ {
				if !send(0x41, ts, i == perFrame-1, *size) {
					return
				}
				seq++
			}
		}
		frame++
	}
}

func runViewer(node string, hold time.Duration, stop <-chan struct{}) error {
	pc, err := api().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return err
	}
	var got atomic.Int64
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				if _, _, err := tr.ReadRTP(); err != nil {
					return
				}
				got.Add(1)
			}
		}()
	})
	tk := fmt.Sprintf("%s|%d", node, ticketSeq.Add(1))
	if err := negotiate(pc, "/offer/viewer", map[string]any{"ticket": tk}); err != nil {
		return err
	}
	if !waitConnected(pc, 10*time.Second) {
		return fmt.Errorf("viewer %s: not connected", node)
	}
	select {
	case <-stop:
	case <-time.After(hold):
	}
	if got.Load() > 0 {
		statViewerMedia.Add(1)
	}
	return nil
}

func randDur(a, b time.Duration) time.Duration {
	if b <= a {
		return a
	}
	return a + time.Duration(rand.Int63n(int64(b-a)))
}

// ---------- churn-цикли ----------

// gate — «кран» навантаження: тиха точка закриває його і чекає, поки всі
// активні ноги завершаться.
type gate struct {
	mu     sync.Mutex
	stop   chan struct{}
	active sync.WaitGroup
}

func (g *gate) cur() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stop
}

func (g *gate) closeAll() {
	g.mu.Lock()
	close(g.stop)
	g.mu.Unlock()
	g.active.Wait()
}

func (g *gate) reopen() {
	g.mu.Lock()
	g.stop = make(chan struct{})
	g.mu.Unlock()
}

var nodeNames struct {
	mu  sync.Mutex
	ids []string
}

func pickNode() string {
	nodeNames.mu.Lock()
	defer nodeNames.mu.Unlock()
	return nodeNames.ids[rand.Intn(len(nodeNames.ids))]
}

func agentLoop(slot int, g *gate, done <-chan struct{}) {
	node := fmt.Sprintf("soak-n%d", slot)
	for {
		select {
		case <-done:
			return
		default:
		}
		if paused.Load() {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		g.active.Add(1)
		stop := g.cur()
		if err := runAgent(node, randDur(*aHoldMin, *aHoldMax), stop); err != nil {
			statAgentFail.Add(1)
			log.Printf("agent %s: %v", node, err)
			time.Sleep(time.Second)
		}
		g.active.Done()
		// Ріст парку: частина перепідключень приходить уже під новим node_id
		// (новий ПК замість старого) — старий має зникнути з хаба повністю.
		if rand.Intn(100) < *newNodePct {
			n := statNewNodes.Add(1)
			old := node
			node = fmt.Sprintf("soak-n%d-g%d", slot, n)
			nodeNames.mu.Lock()
			for i, id := range nodeNames.ids {
				if id == old {
					nodeNames.ids[i] = node
				}
			}
			nodeNames.mu.Unlock()
		}
		time.Sleep(randDur(100*time.Millisecond, 1500*time.Millisecond))
	}
}

func viewerLoop(g *gate, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		if paused.Load() {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		g.active.Add(1)
		stop := g.cur()
		err := runViewer(pickNode(), randDur(*vHoldMin, *vHoldMax), stop)
		g.active.Done()
		if err != nil {
			statViewerFail.Add(1)
			log.Printf("viewer: %v", err)
			time.Sleep(500 * time.Millisecond)
		} else {
			statViewerOK.Add(1)
		}
		time.Sleep(randDur(20*time.Millisecond, 300*time.Millisecond))
	}
}

// ---------- семплінг ----------

type sample struct {
	T                                   float64
	Kind                                string
	Goroutines, HeapAlloc, HeapSys, RSS float64
	GCHeap, GCInuse                     float64
	Nodes, Agents                       float64
	VOK, VFail, AOK, AFail, VMedia      int64
}

func scrapeMetrics() map[string]float64 {
	out := map[string]float64{}
	resp, err := httpc.Get(*metricsURL)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		ln := sc.Text()
		if strings.HasPrefix(ln, "#") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		name := f[0]
		if i := strings.IndexByte(name, '{'); i >= 0 {
			name = name[:i]
		}
		if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil {
			out[name] += v
		}
	}
	return out
}

// gcHeap — HeapAlloc/HeapInuse ПІСЛЯ примусового GC (pprof heap?gc=1&debug=1).
func gcHeap() (alloc, inuse float64) {
	if *pprofURL == "" {
		return 0, 0
	}
	// Два GC поспіль: після першого вміст sync.Pool переходить у victim-кеш і
	// ще лічиться в HeapAlloc; лише другий його звільняє.
	if r, err := httpc.Get(*pprofURL + "/debug/pprof/heap?gc=1"); err == nil {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	resp, err := httpc.Get(*pprofURL + "/debug/pprof/heap?gc=1&debug=1")
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 4<<20), 4<<20)
	for sc.Scan() {
		ln := sc.Text()
		if v, ok := strings.CutPrefix(ln, "# HeapAlloc = "); ok {
			alloc, _ = strconv.ParseFloat(v, 64)
		}
		if v, ok := strings.CutPrefix(ln, "# HeapInuse = "); ok {
			inuse, _ = strconv.ParseFloat(v, 64)
		}
	}
	return
}

func dump(name, path string) {
	if *pprofURL == "" {
		return
	}
	resp, err := httpc.Get(*pprofURL + path)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	_ = os.WriteFile(filepath.Join(*outDir, name), b, 0o644)
}

func take(t0 time.Time, kind string, gc bool) sample {
	s := sample{T: time.Since(t0).Seconds(), Kind: kind}
	if gc {
		s.GCHeap, s.GCInuse = gcHeap()
	}
	m := scrapeMetrics()
	s.Goroutines = m["oo_hub_goroutines"]
	s.HeapAlloc = m["oo_hub_heap_alloc_bytes"]
	s.HeapSys = m["oo_hub_heap_sys_bytes"]
	s.RSS = m["oo_hub_process_resident_memory_bytes"]
	s.Nodes = m["oo_hub_nodes"]
	s.Agents = m["oo_hub_agents"]
	s.VOK, s.VFail = statViewerOK.Load(), statViewerFail.Load()
	s.AOK, s.AFail = statAgentOK.Load(), statAgentFail.Load()
	s.VMedia = statViewerMedia.Load()
	return s
}

func (s sample) csv() string {
	return fmt.Sprintf("%.0f,%s,%.0f,%.2f,%.2f,%.2f,%.2f,%.2f,%.0f,%.0f,%d,%d,%d,%d,%d",
		s.T, s.Kind, s.Goroutines, s.HeapAlloc/1e6, s.HeapSys/1e6, s.RSS/1e6, s.GCHeap/1e6, s.GCInuse/1e6,
		s.Nodes, s.Agents, s.VOK, s.VFail, s.AOK, s.AFail, s.VMedia)
}

// slope — нахил МНК y(x) на годину.
func slope(xs, ys []float64) float64 {
	n := float64(len(xs))
	if n < 2 {
		return math.NaN()
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		x := xs[i] / 3600
		sx += x
		sy += ys[i]
		sxx += x * x
		sxy += x * ys[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return math.NaN()
	}
	return (n*sxy - sx*sy) / d
}

func main() {
	flag.Parse()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	startFakeERP(*erpAddr)
	for i := 0; i < *nodes; i++ {
		nodeNames.ids = append(nodeNames.ids, fmt.Sprintf("soak-n%d", i))
	}
	csvF, err := os.Create(filepath.Join(*outDir, "samples.csv"))
	if err != nil {
		log.Fatal(err)
	}
	defer csvF.Close()
	fmt.Fprintln(csvF, "t_s,kind,goroutines,heap_alloc_mb,heap_sys_mb,rss_mb,gc_heap_mb,gc_inuse_mb,nodes,agents,viewers_ok,viewers_fail,agents_ok,agents_fail,viewers_with_media")

	t0 := time.Now()
	write := func(s sample) {
		fmt.Fprintln(csvF, s.csv())
		_ = csvF.Sync()
		log.Printf("%s", s.csv())
	}
	base := take(t0, "start", true)
	write(base)
	dump("goroutines-start.txt", "/debug/pprof/goroutine?debug=1")

	g := &gate{stop: make(chan struct{})}
	done := make(chan struct{})
	for i := 0; i < *nodes; i++ {
		go agentLoop(i, g, done)
	}
	time.Sleep(3 * time.Second)
	for i := 0; i < *viewers; i++ {
		go viewerLoop(g, done)
	}

	var quiet []sample
	end := time.After(*dur)
	sampleT := time.NewTicker(*sampleEv)
	quietT := time.NewTicker(*quietEv)
	qi := 0
	doQuiet := func(label string) {
		paused.Store(true)
		g.closeAll()
		time.Sleep(*quietWait)
		s := take(t0, label, true)
		write(s)
		quiet = append(quiet, s)
		dump(fmt.Sprintf("goroutines-quiet%02d.txt", qi), "/debug/pprof/goroutine?debug=1")
		dump(fmt.Sprintf("heap-quiet%02d.pb.gz", qi), "/debug/pprof/heap?gc=1")
		qi++
		g.reopen()
		paused.Store(false)
	}
loop:
	for {
		select {
		case <-end:
			break loop
		case <-sampleT.C:
			write(take(t0, "load", false))
		case <-quietT.C:
			doQuiet("quiet")
		}
	}
	close(done)
	doQuiet("final")
	dump("heap-final.pb.gz", "/debug/pprof/heap?gc=1")

	el := time.Since(t0)
	vOK, aOK := statViewerOK.Load(), statAgentOK.Load()
	fmt.Printf("\n=== churnsoak: %s ===\n", el.Round(time.Second))
	fmt.Printf("глядачів: ok=%d fail=%d (з медіа %d); агентів: ok=%d fail=%d; нових node_id: %d\n",
		vOK, statViewerFail.Load(), statViewerMedia.Load(), aOK, statAgentFail.Load(), statNewNodes.Load())
	fmt.Printf("еквівалент реальних діб: глядачі %.1f (при %d/добу), агенти %.1f (при %d/добу)\n",
		float64(vOK)/float64(*dayViewers), *dayViewers, float64(aOK)/float64(*dayAgents), *dayAgents)
	fmt.Printf("старт: goroutines=%.0f gcHeap=%.2fMB RSS=%.1fMB\n", base.Goroutines, base.GCHeap/1e6, base.RSS/1e6)
	for _, q := range quiet {
		fmt.Printf("тиха @%5.0fс: goroutines=%.0f gcHeap=%.2fMB inuse=%.2fMB RSS=%.1fMB nodes=%.0f\n",
			q.T, q.Goroutines, q.GCHeap/1e6, q.GCInuse/1e6, q.RSS/1e6, q.Nodes)
	}
	if len(quiet) >= 3 {
		q := quiet[1:]
		var xs, gr, hp, rs []float64
		for _, s := range q {
			xs = append(xs, s.T)
			gr = append(gr, s.Goroutines)
			hp = append(hp, s.GCHeap/1e6)
			rs = append(rs, s.RSS/1e6)
		}
		fmt.Printf("нахил по тихих точках (без першої): goroutines %+.2f/год, gcHeap %+.3f МБ/год, RSS %+.2f МБ/год\n",
			slope(xs, gr), slope(xs, hp), slope(xs, rs))
	}
}
