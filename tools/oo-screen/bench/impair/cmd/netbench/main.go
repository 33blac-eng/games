// netbench — один сценарій «corpus-player (драбина) -> hub-webrtc -> [реле з
// вадами] -> pion-viewer». Сам піднімає хаб і агента на приватних портах,
// читає NDJSON хаба (ctl/nack) і лог агента, веде фази чисто/вада/чисто і
// дописує один JSON-рядок результату в -out.
//
// Вади ставляться на ноги hub<->viewer реле bench/impair (без tc/netem):
// втрати/burst/затримка/джитер — в обидва боки (RTT = 2 x -owd), смуга й
// перестановка — лише hub->viewer.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/bench/impair"
)

type ctlPoint struct {
	T   float64 `json:"t"` // секунд від старту вади (від’ємні — розігрів)
	Bps uint64  `json:"bps"`
}

type phaseOut struct {
	impair.FrameStats
	CompletePct  float64 `json:"complete_pct"`
	DecodablePct float64 `json:"decodable_pct"`
	FreezePerMin float64 `json:"freeze_per_min"`
	FreezeSecMin float64 `json:"freeze_sec_per_min"`
	RecvMbps     float64 `json:"recv_mbps"`
}

type result struct {
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Warm   phaseOut       `json:"warm"`
	Imp    phaseOut       `json:"imp"`
	Post   phaseOut       `json:"post"`
	Ctl    []ctlPoint     `json:"ctl"`
	CtlImp struct {
		Changes      int     `json:"changes"`
		FirstCutS    float64 `json:"first_cut_s"` // -1 = не різав
		MinBps       uint64  `json:"min_bps"`
		EndBps       uint64  `json:"end_bps"`
		Last10Mean   float64 `json:"last10_mean_bps"`
		RecoverS     float64 `json:"recover90_s"` // -1 = не дійшов до 90% стелі за post
		PostFinalBps uint64  `json:"post_final_bps"`
	} `json:"ctl_imp"`
	Nack struct {
		ShimDropped   int     `json:"shim_dropped"`
		ShimRecovered int     `json:"shim_recovered"`
		RecoveryPct   float64 `json:"recovery_pct"`
		RecP50Ms      float64 `json:"rec_p50_ms"`
		RecP95Ms      float64 `json:"rec_p95_ms"`
		QueueDrops    int     `json:"queue_drops"`
		NackPktsSent  int64   `json:"viewer_nack_pkts"`
		HubReq        int     `json:"hub_req"`
		HubHit        int     `json:"hub_hit"`
	} `json:"nack"`
	PLI struct {
		ViewerPLI     int64   `json:"viewer_pli"`
		AgentKF       int     `json:"agent_keyframe_req"` // keyframe_request ctl
		AgentPLI      int     `json:"agent_pli"`
		ImpPerMin     float64 `json:"imp_kf_per_min"` // keyframe_request+PLI на агенті за вікно вади
		ViewerPerMin  float64 `json:"imp_viewer_pli_per_min"`
		IDRImpPerMin  float64 `json:"imp_idr_per_min"`
		AgentEvTimesS []float64
	} `json:"pli"`
}

const benchToken = "netbench-local-token"

var t0 time.Time // старт вади (заповнюється, коли пішов потік)
var tStream time.Time

func main() {
	hubBin := flag.String("hub-bin", "", "бінар hub-webrtc")
	agentBin := flag.String("agent-bin", "", "бінар corpus-player-webrtc")
	ladder := flag.String("ladder", "", "OO_CORPUS_LADDER для агента")
	port := flag.Int("port", 4481, "TCP-порт хаба")
	udpMin := flag.Int("udp-min", 4800, "UDP-діапазон хаба")
	name := flag.String("name", "run", "назва")
	out := flag.String("out", "netbench.jsonl", "куди дописати результат")
	logdir := flag.String("logdir", "", "тека для логів хаба/агента")
	warm := flag.Duration("warm", 10*time.Second, "розігрів без вад")
	imp := flag.Duration("imp", 30*time.Second, "тривалість вади")
	post := flag.Duration("post", 15*time.Second, "чисто після вади")
	loss := flag.Float64("loss", 0, "рівномірні втрати 0..1, обидва боки")
	geP := flag.Float64("ge-p", 0, "Gilbert-Elliott p (good->bad); >0 вмикає burst замість -loss")
	geR := flag.Float64("ge-r", 0.3, "Gilbert-Elliott r (bad->good)")
	rtt := flag.Duration("rtt", 0, "доданий RTT (по половині в кожен бік)")
	jitter := flag.Duration("jitter", 0, "± джитер на пакет, обидва боки")
	reorder := flag.Float64("reorder", 0, "частка перестановок hub->viewer")
	capBps := flag.Float64("cap", 0, "стеля смуги hub->viewer, біт/с")
	queueMs := flag.Int("queue-ms", 100, "черга вузького місця, мс при -cap")
	nackIvl := flag.Duration("nack-interval", 100*time.Millisecond, "інтервал NACK-генератора viewer-а (дефолт pion 100 мс)")
	fastup := flag.Bool("fastup", false, "OO_SCREEN_BITRATE_FASTUP=1 на хабі")
	// B6: вади на нозі АГЕНТ->хаб (друге реле + MITM сигналінгу агента).
	// Медіа агента йде через Uplink реле, RTCP хаба (NACK/PLI) — Downlink;
	// втрати — обидва боки, як -loss. Нога хаб->глядач при цьому чиста,
	// якщо -loss не задано.
	agentLoss := flag.Float64("agent-loss", 0, "рівномірні втрати на нозі агент<->хаб, 0..1")
	agentRTT := flag.Duration("agent-rtt", 0, "доданий RTT на нозі агент<->хаб")
	hubEnv := flag.String("hub-env", "", "додаткові env хаба через кому (K=V,K=V)")
	flag.Parse()

	if *logdir == "" {
		*logdir = filepath.Join(os.TempDir(), "netbench-"+*name)
	}
	os.MkdirAll(*logdir, 0o755)

	var res result
	res.Name = *name
	res.Args = map[string]any{"loss": *loss, "ge_p": *geP, "ge_r": *geR, "rtt_ms": rtt.Milliseconds(),
		"jitter_ms": jitter.Milliseconds(), "reorder": *reorder, "cap_bps": *capBps, "queue_ms": *queueMs,
		"fastup": *fastup, "nack_ivl_ms": nackIvl.Milliseconds(), "warm_s": warm.Seconds(), "imp_s": imp.Seconds(), "post_s": post.Seconds()}

	var mu sync.Mutex
	var ctl []struct {
		at  time.Time
		bps uint64
	}
	var hubReq, hubHit int
	var agentKF, agentPLI []time.Time

	// --- хаб ---
	env := append(os.Environ(), "OO_SCREEN_T1_TOKEN="+benchToken,
		fmt.Sprintf("OO_SCREEN_HUB_ADDR=127.0.0.1:%d", *port),
		fmt.Sprintf("OO_SCREEN_UDP_PORT_MIN=%d", *udpMin),
		fmt.Sprintf("OO_SCREEN_UDP_PORT_MAX=%d", *udpMin+40),
	)
	if *fastup {
		env = append(env, "OO_SCREEN_BITRATE_FASTUP=1")
	}
	if *hubEnv != "" {
		env = append(env, strings.Split(*hubEnv, ",")...)
	}
	res.Args["agent_loss"] = *agentLoss
	res.Args["agent_rtt_ms"] = agentRTT.Milliseconds()
	res.Args["hub_env"] = *hubEnv
	hub := exec.Command(*hubBin)
	hub.Env = env
	hub.Dir = filepath.Dir(*hubBin)
	hubLog, _ := os.Create(filepath.Join(*logdir, "hub.log"))
	defer hubLog.Close()
	scan(hub, hubLog, func(line string) {
		if !strings.HasPrefix(line, `{"leg":"ctl"`) && !strings.HasPrefix(line, `{"leg":"nack"`) {
			return
		}
		var m struct {
			Leg     string `json:"leg"`
			Bitrate uint64 `json:"bitrate"`
			Req     int    `json:"req"`
			Hit     int    `json:"hit"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if m.Leg == "ctl" {
			ctl = append(ctl, struct {
				at  time.Time
				bps uint64
			}{time.Now(), m.Bitrate})
		} else {
			hubReq += m.Req
			hubHit += m.Hit
		}
	})
	if err := hub.Start(); err != nil {
		log.Fatalf("hub: %v", err)
	}
	defer kill(hub)
	waitTCP(*port)

	// --- реле агента: агент шле offer на MITM, той переписує кандидатів на
	// сокети реле і пересилає хабу. Без -agent-loss/-agent-rtt — напряму.
	clean := impair.Config{}
	hubAgentURL := fmt.Sprintf("http://127.0.0.1:%d/offer/agent", *port)
	agentURL := hubAgentURL
	var apx *impair.Proxy
	if *agentLoss > 0 || *agentRTT > 0 {
		var err error
		apx, err = impair.NewProxy(clean, clean, time.Now().UnixNano()+7)
		must(err)
		defer apx.Close()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		must(err)
		defer ln.Close()
		go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			sdp, _ := req["sdp"].(string)
			osdp, err := impair.Rewrite(sdp, apx.Up)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			req["sdp"] = osdp
			body, _ := json.Marshal(req)
			resp, err := http.Post(hubAgentURL, "application/json", bytes.NewReader(body))
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				w.WriteHeader(resp.StatusCode)
				w.Write(rb)
				return
			}
			var ans map[string]any
			if err := json.Unmarshal(rb, &ans); err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			asdp, _ := ans["sdp"].(string)
			if err := apx.LearnHub(asdp); err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			if ans["sdp"], err = impair.Rewrite(asdp, apx.Down); err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			jb, _ := json.Marshal(ans)
			w.Header().Set("Content-Type", "application/json")
			w.Write(jb)
		}))
		agentURL = "http://" + ln.Addr().String() + "/offer/agent"
	}

	// --- агент ---
	ag := exec.Command(*agentBin)
	ag.Env = append(os.Environ(), "OO_SCREEN_T1_TOKEN="+benchToken, "OO_SCREEN_HUB_URL="+agentURL,
		"OO_CORPUS_LADDER="+*ladder)
	agLog, _ := os.Create(filepath.Join(*logdir, "agent.log"))
	defer agLog.Close()
	scan(ag, agLog, func(line string) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(line, "keyframe_request -> IDR"):
			agentKF = append(agentKF, time.Now())
		case strings.Contains(line, "PLI received from hub"):
			agentPLI = append(agentPLI, time.Now())
		}
	})
	if err := ag.Start(); err != nil {
		log.Fatalf("agent: %v", err)
	}
	defer kill(ag)
	time.Sleep(3 * time.Second)

	// --- реле + viewer ---
	px, err := impair.NewProxy(clean, clean, time.Now().UnixNano())
	if err != nil {
		log.Fatal(err)
	}
	defer px.Close()
	down := impair.Config{Loss: *loss, Delay: *rtt / 2, Jitter: *jitter, Reorder: *reorder, RateBps: *capBps}
	if *capBps > 0 {
		down.QueueBytes = int(*capBps / 8 * float64(*queueMs) / 1000)
	}
	up := impair.Config{Loss: *loss, Delay: *rtt / 2, Jitter: *jitter}
	if *geP > 0 {
		down.Burst = &impair.GE{P: *geP, R: *geR, LossBad: 1}
		up.Burst = &impair.GE{P: *geP, R: *geR, LossBad: 1}
		down.Loss, up.Loss = 0, 0
	}

	rec := impair.NewRecorder()
	var recMu sync.Mutex
	var nackPkts, pliPkts atomic.Int64
	tap := &tapI{nack: &nackPkts, pli: &pliPkts}

	m := &webrtc.MediaEngine{}
	must(m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64002a",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}},
		}, PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo))
	ir := &interceptor.Registry{}
	ir.Add(tapFactory{tap})
	// NACK-генератор pion: дефолт — опит дірок раз на 100 мс; -nack-interval
	// дозволяє наблизитись до libwebrtc (NACK одразу на дірку, повтор раз на RTT).
	must(webrtc.ConfigureNackWithOptions(m, ir, []nack.GeneratorOption{nack.GeneratorInterval(*nackIvl)}))
	must(webrtc.ConfigureRTCPReports(ir))
	// Вікно SRTP replay — як у libwebrtc (1024). Дефолт pion 64: NACK-ретрансмісія,
	// що приходить через >64 пакети (800 пак/с x RTT), мовчки відкидалась би
	// SRTP-шаром — такої втрати браузер не має.
	se := webrtc.SettingEngine{}
	se.SetSRTPReplayProtectionWindow(1024)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	must(err)
	defer pc.Close()
	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	must(err)

	// Емуляція браузера: якщо дірка в seq не закрилась (NACK не врятував) за
	// max(300 мс, 1.5 RTT) — PLI, як Chrome, коли jitter buffer здається.
	giveUp := 300 * time.Millisecond
	if g := *rtt * 3 / 2; g > giveUp {
		giveUp = g
	}
	started := make(chan struct{})
	var startOnce sync.Once
	pc.OnTrack(func(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		go func() {
			b := make([]byte, 1500)
			for {
				if _, _, err := r.Read(b); err != nil {
					return
				}
			}
		}()
		var high uint64
		missing := map[uint64]time.Time{}
		var lastPLI time.Time
		var u impair.Unwrapper
		for {
			p, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			now := time.Now()
			startOnce.Do(func() { tStream = now; close(started) })
			recMu.Lock()
			rec.Add(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload, now, px.Stats.SentAt(p.SequenceNumber, now))
			recMu.Unlock()
			e := u.Ext(p.SequenceNumber)
			if high != 0 && e > high+1 {
				for s := high + 1; s < e; s++ {
					missing[s] = now
				}
			}
			delete(missing, e)
			if e > high {
				high = e
			}
			for s, at := range missing {
				if now.Sub(at) > giveUp {
					delete(missing, s)
					if now.Sub(lastPLI) > 500*time.Millisecond {
						lastPLI = now
						_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(t.SSRC())}})
					}
				}
			}
		}
	})

	offer, err := pc.CreateOffer(nil)
	must(err)
	gc := webrtc.GatheringCompletePromise(pc)
	must(pc.SetLocalDescription(offer))
	<-gc
	osdp, err := impair.Rewrite(pc.LocalDescription().SDP, px.Up)
	must(err)
	body, _ := json.Marshal(map[string]string{"sdp": osdp, "token": benchToken})
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/offer/viewer", *port), "application/json", bytes.NewReader(body))
	must(err)
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Fatalf("viewer offer %d: %s", resp.StatusCode, rb)
	}
	var ans struct{ SDP string }
	must(json.Unmarshal(rb, &ans))
	must(px.LearnHub(ans.SDP))
	asdp, err := impair.Rewrite(ans.SDP, px.Down)
	must(err)
	must(pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: asdp}))

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		log.Fatal("потік не пішов за 15 с")
	}
	t0 = tStream.Add(*warm)
	time.Sleep(time.Until(t0))
	px.Downlink.Set(down)
	px.Uplink.Set(up)
	var rtcp0 int64
	if apx != nil {
		rtcp0 = apx.RTCPDown.Load()
		ac := impair.Config{Loss: *agentLoss, Delay: *agentRTT / 2}
		apx.Downlink.Set(ac)
		apx.Uplink.Set(ac)
	}
	d0, _, _ := px.Stats.Snapshot()
	nack0 := nackPkts.Load()
	pli0 := pliPkts.Load()
	time.Sleep(*imp)
	px.Downlink.Set(clean)
	px.Uplink.Set(clean)
	if apx != nil {
		apx.Downlink.Set(clean)
		apx.Uplink.Set(clean)
		res.Args["agent_rtcp_imp"] = apx.RTCPDown.Load() - rtcp0
		fmt.Printf("agent leg: hub->agent RTCP during impairment: %d\n", apx.RTCPDown.Load()-rtcp0)
	}
	pliImp := pliPkts.Load() - pli0
	time.Sleep(*post)
	time.Sleep(time.Second) // хвости ретрансмісій

	// --- аналіз ---
	recMu.Lock()
	fr := impair.Frames(rec.Pkts)
	pkts := rec.Pkts
	recMu.Unlock()
	tEnd := t0.Add(*imp)
	if df, err := os.Create(filepath.Join(*logdir, "pkts.tsv")); err == nil {
		for _, p := range pkts {
			fmt.Fprintf(df, "%d\t%d\t%v\t%v\t%v\t%.4f\n", p.Ext, p.TS, p.Marker, p.IDR, p.Start, p.At.Sub(t0).Seconds())
		}
		df.Close()
	}
	res.Warm = phase(fr, pkts, tStream.Add(2*time.Second), t0)
	res.Imp = phase(fr, pkts, t0, tEnd)
	res.Post = phase(fr, pkts, tEnd, tEnd.Add(*post-time.Second))

	mu.Lock()
	ceil := uint64(0)
	for _, c := range ctl {
		if c.bps > ceil {
			ceil = c.bps
		}
	}
	ci := &res.CtlImp
	ci.FirstCutS, ci.RecoverS = -1, -1
	cur := ceil
	var win []ctlPoint
	for _, c := range ctl {
		tt := c.at.Sub(t0).Seconds()
		res.Ctl = append(res.Ctl, ctlPoint{tt, c.bps})
		if c.at.Before(t0) {
			cur = c.bps
			continue
		}
		if c.at.Before(tEnd) {
			ci.Changes++
			if c.bps < cur && ci.FirstCutS < 0 {
				ci.FirstCutS = tt
			}
		}
		cur = c.bps
		_ = win
	}
	ci.MinBps, ci.EndBps = ceil, ceil
	valAt := func(t time.Time) uint64 {
		v := ceil
		for _, c := range ctl {
			if c.at.After(t) {
				break
			}
			v = c.bps
		}
		return v
	}
	for _, c := range ctl {
		if !c.at.Before(t0) && c.at.Before(tEnd) && c.bps < ci.MinBps {
			ci.MinBps = c.bps
		}
	}
	ci.EndBps = valAt(tEnd)
	// середнє за останні 10 с вади (зважене за часом)
	var acc float64
	for s := 0; s < 100; s++ {
		acc += float64(valAt(tEnd.Add(-10*time.Second + time.Duration(s)*100*time.Millisecond)))
	}
	ci.Last10Mean = acc / 100
	ci.PostFinalBps = valAt(tEnd.Add(*post))
	if ci.EndBps >= ceil*9/10 {
		ci.RecoverS = 0
	} else {
		for _, c := range ctl {
			if c.at.After(tEnd) && c.bps >= ceil*9/10 {
				ci.RecoverS = c.at.Sub(tEnd).Seconds()
				break
			}
		}
	}
	nk := &res.Nack
	nk.HubReq, nk.HubHit = hubReq, hubHit
	kfImp, pliA := 0, 0
	for _, t := range agentKF {
		if !t.Before(t0) && t.Before(tEnd) {
			kfImp++
		}
		res.PLI.AgentEvTimesS = append(res.PLI.AgentEvTimesS, t.Sub(t0).Seconds())
	}
	for _, t := range agentPLI {
		if !t.Before(t0) && t.Before(tEnd) {
			pliA++
		}
		res.PLI.AgentEvTimesS = append(res.PLI.AgentEvTimesS, t.Sub(t0).Seconds())
	}
	res.PLI.AgentKF, res.PLI.AgentPLI = len(agentKF), len(agentPLI)
	mu.Unlock()
	sort.Float64s(res.PLI.AgentEvTimesS)
	mins := imp.Minutes()
	res.PLI.ImpPerMin = float64(kfImp+pliA) / mins
	res.PLI.ViewerPLI = pliPkts.Load()
	res.PLI.ViewerPerMin = float64(pliImp) / mins
	res.PLI.IDRImpPerMin = float64(res.Imp.IDR) / mins

	dropped, recov, delays := px.Stats.Snapshot()
	_ = d0
	nk.ShimDropped, nk.ShimRecovered = dropped, recov
	if dropped > 0 {
		nk.RecoveryPct = 100 * float64(recov) / float64(dropped)
	}
	sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
	if len(delays) > 0 {
		nk.RecP50Ms = float64(delays[len(delays)/2]) / 1e6
		nk.RecP95Ms = float64(delays[len(delays)*95/100]) / 1e6
	}
	nk.QueueDrops = px.Stats.QueueDrops()
	nk.NackPktsSent = nackPkts.Load() - nack0

	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	must(err)
	jb, _ := json.Marshal(res)
	f.Write(append(jb, '\n'))
	f.Close()
	fmt.Printf("%s: imp complete=%.1f%% decodable=%.1f%% freeze=%.1fs/min p95=%.0fms | nack rec=%.1f%% (%d/%d) | kf/min=%.1f | ctl min=%d end=%d last10=%.0f recover=%.1fs\n",
		*name, res.Imp.CompletePct, res.Imp.DecodablePct, res.Imp.FreezeSecMin, res.Imp.LatP95Ms,
		nk.RecoveryPct, recov, dropped, res.PLI.ImpPerMin, ci.MinBps, ci.EndBps, ci.Last10Mean, ci.RecoverS)
}

func phase(fr []impair.Frame, pkts []impair.Pkt, from, to time.Time) phaseOut {
	s := impair.Summarize(fr, from, to)
	o := phaseOut{FrameStats: s}
	if s.Expected > 0 {
		o.CompletePct = 100 * float64(s.Complete) / float64(s.Expected)
		o.DecodablePct = 100 * float64(s.Decodable) / float64(s.Expected)
	}
	mins := to.Sub(from).Minutes()
	o.FreezePerMin = float64(s.Freezes) / mins
	o.FreezeSecMin = s.FreezeMs / 1000 / mins
	var bytes int
	for _, p := range pkts {
		if !p.At.Before(from) && p.At.Before(to) {
			bytes += p.Size + 12
		}
	}
	o.RecvMbps = float64(bytes*8) / to.Sub(from).Seconds() / 1e6
	return o
}

func scan(c *exec.Cmd, sink io.Writer, f func(string)) {
	pr, pw := io.Pipe()
	c.Stdout, c.Stderr = pw, pw
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			fmt.Fprintf(sink, "%s %s\n", time.Now().Format("15:04:05.000"), l)
			f(l)
		}
	}()
}

func kill(c *exec.Cmd) {
	if c.Process != nil {
		c.Process.Kill()
		c.Wait()
	}
}

func waitTCP(port int) {
	for i := 0; i < 100; i++ {
		r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Fatal("хаб не піднявся")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// tapI рахує NACK і PLI, які viewer шле в хаб.
type tapI struct {
	interceptor.NoOp
	nack, pli *atomic.Int64
}

func (t *tapI) BindRTCPWriter(w interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, a interceptor.Attributes) (int, error) {
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.TransportLayerNack:
				t.nack.Add(1)
			case *rtcp.PictureLossIndication:
				t.pli.Add(1)
			}
		}
		return w.Write(pkts, a)
	})
}

type tapFactory struct{ t *tapI }

func (f tapFactory) NewInterceptor(string) (interceptor.Interceptor, error) { return f.t, nil }
