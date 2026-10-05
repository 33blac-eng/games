package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/organicoils/oo-screen/internal/pcmu"
)

// dialViewerLeg — «браузер», який ходить рівно тим самим шляхом, що прод:
// recvonly-трансивери -> POST /offer/viewer -> handleOffer -> answer. withAudio
// відповідає рядку `peer.addTransceiver('audio', {direction:'recvonly'})` у
// desktop-oo-webrtc.js. Повертає ногу браузера, SDP відповіді хаба і канал, у
// який лягають доріжки, що реально приїхали.
func dialViewerLeg(t *testing.T, withAudio bool) (*webrtc.PeerConnection, string, <-chan *webrtc.TrackRemote) {
	t.Helper()

	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("viewer pc: %v", err)
	}
	t.Cleanup(func() { _ = remote.Close() })

	recv := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}
	if _, err := remote.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, recv); err != nil {
		t.Fatalf("AddTransceiver(video): %v", err)
	}
	if withAudio {
		if _, err := remote.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, recv); err != nil {
			t.Fatalf("AddTransceiver(audio): %v", err)
		}
	}

	tracks := make(chan *webrtc.TrackRemote, 4)
	remote.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case tracks <- tr:
		default:
		}
	})

	offer, err := remote.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(remote)
	if err := remote.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gathered

	body, _ := json.Marshal(offerReq{SDP: remote.LocalDescription().SDP, Token: token})
	req := httptest.NewRequest(http.MethodPost, "/offer/viewer", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleOffer("viewer")(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /offer/viewer: %d %s", w.Code, w.Body.String())
	}
	var ans answerResp
	if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil {
		t.Fatalf("answer json: %v", err)
	}
	if err := remote.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: ans.SDP,
	}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	return remote, ans.SDP, tracks
}

// mediaPorts — порти m-рядків заданого типу у SDP. Порт 0 = доріжку відхилено
// (саме так виглядає «зайвого в SDP немає»), ненульовий = доріжка жива.
func mediaPorts(sdp, kind string) []string {
	var out []string
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "m="+kind+" ") {
			continue
		}
		f := strings.Fields(line)
		out = append(out, f[1])
	}
	return out
}

// withAudioFlag ставить прапорець на час тесту і повертає його назад, разом зі
// свіжим реєстром — щоб ноги одного тесту не потрапляли в інший.
func withAudioFlag(t *testing.T, on bool) {
	t.Helper()
	prevFlag, prevReg := audioEnabled, reg
	mine := newRegistry()
	audioEnabled, reg = on, mine
	t.Cleanup(func() {
		audioEnabled, reg = prevFlag, prevReg
		// Хабові ноги тесту закриваємо разом із ним. Інакше вони лишались
		// жити на спільному ICE-mux і ще ~30 с слали STUN-перевірки, поки
		// pion не визнає їх failed, — тобто крали CPU і сокет у НАСТУПНОГО
		// тесту саме тоді, коли той піднімає свою ногу.
		mine.mu.Lock()
		var pcs []*webrtc.PeerConnection
		for _, ns := range mine.nodes {
			ns.mu.Lock()
			if ns.agentPC != nil {
				pcs = append(pcs, ns.agentPC)
			}
			for _, vl := range ns.viewers {
				pcs = append(pcs, vl.pc)
			}
			ns.mu.Unlock()
		}
		mine.mu.Unlock()
		for _, pc := range pcs {
			_ = pc.Close()
		}
	})
}

// awaitAudioTrack чекає, поки в браузерну ногу приїде саме аудіо-доріжка.
//
// Вердикт дає ПОДІЯ, а не секундомір: доріжка приїхала — успіх; нога впала
// в failed/closed — провал. Раніше тут стояли фіксовані 20 с, і на завантаженій
// машині (гейт під -race поруч з іншими збираннями) ICE встигав лише дійти до
// checking — тест падав, хоча нога ще жила й конектилась. Скільки чекати на
// ICE, вирішує сам pion (його failed-тайм-аут), а stuckGuard — лише запобіжник
// від вічного зависання, якщо не прийде взагалі жодної події.
func awaitAudioTrack(t *testing.T, pc *webrtc.PeerConnection, tracks <-chan *webrtc.TrackRemote) *webrtc.TrackRemote {
	t.Helper()
	// Стан ноги опитуємо, а не ставимо OnConnectionStateChange: у pion обробник
	// один на PeerConnection, і наш затирав би обробник того, хто ногу підняв.
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	const stuckGuard = 3 * time.Minute
	guard := time.After(stuckGuard)
	for {
		select {
		case tr := <-tracks:
			if tr.Kind() == webrtc.RTPCodecTypeAudio {
				return tr
			}
		case <-poll.C:
			if s := pc.ConnectionState(); s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
				t.Fatalf("браузерна нога впала (%s), аудіо-доріжка так і не приїхала", s)
				return nil
			}
		case <-guard:
			t.Fatalf("за %s браузерна нога не отримала аудіо-доріжки і не впала", stuckGuard)
			return nil
		}
	}
}

// TestAudioFlagOffChangesNothing — ГОЛОВНИЙ тест кроку: без OO_SCREEN_AUDIO хаб
// поводиться рівно як до появи звуку. Це захист робочого проду, а не фічі.
//
// Три половини правди:
//  1. Браузер, який просить ЛИШЕ відео (сьогоднішній прод-JS), отримує відповідь
//     з одним-єдиним m-рядком і без згадки кодека звуку.
//  2. Браузер, який ПОПРОСИВ звук, його не отримує: m=audio відхилено портом 0.
//     Тобто прапорець гейтить хаб, а не ввічливість клієнта.
//  3. Агент, який звук ШЛЕ, теж його не віддає: хаб не бере оголошення й не
//     піднімає приймальний трансивер, тож агент не кодує в нікуди.
func TestAudioFlagOffChangesNothing(t *testing.T) {
	withAudioFlag(t, false)

	_, videoOnly, _ := dialViewerLeg(t, false)
	if n := len(mediaPorts(videoOnly, "audio")); n != 0 {
		t.Fatalf("offer лише з відео дав %d аудіо-m-рядків у answer, want 0:\n%s", n, videoOnly)
	}
	if n := len(mediaPorts(videoOnly, "video")); n != 1 {
		t.Fatalf("m=video у answer: %d, want 1:\n%s", n, videoOnly)
	}
	if strings.Contains(strings.ToUpper(videoOnly), "PCMU") {
		t.Fatalf("PCMU у answer при вимкненому прапорці:\n%s", videoOnly)
	}

	_, withAudio, _ := dialViewerLeg(t, true)
	ports := mediaPorts(withAudio, "audio")
	if len(ports) != 1 {
		t.Fatalf("offer зі звуком дав %d аудіо-m-рядків у answer, want 1:\n%s", len(ports), withAudio)
	}
	if ports[0] != "0" {
		t.Fatalf("m=audio port=%s при вимкненому прапорці, want 0 (доріжку мали відхилити):\n%s", ports[0], withAudio)
	}
	if strings.Contains(strings.ToUpper(withAudio), "PCMU") {
		t.Fatalf("PCMU у answer при вимкненому прапорці:\n%s", withAudio)
	}

	// Агентська нога: оголошення звуку не має бути прийняте.
	if _, atrk, _ := dialAgentLegWith(t, "audio-off", true); atrk == nil {
		t.Fatal("тестовий агент не створив аудіо-доріжку — перевіряти нема чого")
	}
	if ns := reg.get("audio-off"); ns == nil || ns.hasAgentAudio() {
		t.Fatal("хаб запамʼятав звук агента при вимкненому прапорці")
	}
}

// TestAudioFlagOnDeliversPackets — під прапорцем звук ДОХОДИТЬ: у відповіді є
// жива PCMU-доріжка, і в неї реально йдуть RTP-пакети. Перевіряємо саме на
// приймальному боці (браузерна нога), а не за лічильником хаба: лічильник
// доводить, що ми викликали WriteSample, а не що пакет доїхав.
//
// Publisher-а тут немає навмисно — джерелом лишається запасний тон, і саме це
// робить тест перевіркою ТРУБИ, а не джерела.
func TestAudioFlagOnDeliversPackets(t *testing.T) {
	withAudioFlag(t, true)

	vpc, sdp, tracks := dialViewerLeg(t, true)

	ports := mediaPorts(sdp, "audio")
	if len(ports) != 1 || ports[0] == "0" {
		t.Fatalf("під прапорцем немає живого m=audio (ports=%v):\n%s", ports, sdp)
	}
	if !strings.Contains(strings.ToUpper(sdp), "PCMU/8000") {
		t.Fatalf("у answer немає PCMU/8000:\n%s", sdp)
	}

	audio := awaitAudioTrack(t, vpc, tracks)
	if err := audio.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	for i := 0; i < 5; i++ {
		pkt, _, err := audio.ReadRTP()
		if err != nil {
			t.Fatalf("аудіо-пакет %d/5 не прийшов: %v", i+1, err)
		}
		if len(pkt.Payload) == 0 {
			t.Fatalf("аудіо-пакет %d порожній — доріжка є, звуку немає", i+1)
		}
	}
}

// TestAgentAudioReachesViewer — ГОЛОВНИЙ тест цього кроку: до глядача доходить
// звук АГЕНТА, а не запасний тон.
//
// Джерела розрізняємо за ФОРМОЮ кадру, а не за лічильником хаба: агент шле кадр
// з однакових байтів, а тон — синусоїду, у якій постійного кадру не буває
// взагалі. Тобто 160 однакових байтів на приймальному боці можна побачити
// ТІЛЬКИ якщо вони справді проїхали від агента через readAgentAudio ->
// forwardAudioToViewers -> audioPump, а не народились у хабі.
func TestAgentAudioReachesViewer(t *testing.T) {
	withAudioFlag(t, true)

	// Агент і глядач мусять потрапити в ОДНУ ноду: у static-token режимі
	// глядач іде в agentNodeIDEnv, тож агента піднімаємо туди ж.
	_, atrk, _ := dialAgentLegWith(t, agentNodeIDEnv, true)
	if atrk == nil {
		t.Fatal("агентська нога без аудіо-доріжки")
	}

	// Мітка джерела: кадр з однакових байтів, який не є ані тишею (0xFF), ані
	// чимось, що синусоїда тону може дати на всі 160 семплів.
	marked := bytes.Repeat([]byte{0x11}, pcmu.FrameSamples)
	frameDur := pcmu.Duration(pcmu.FrameSamples)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(frameDur)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = atrk.WriteSample(media.Sample{Data: marked, Duration: frameDur})
			}
		}
	}()

	vpc, sdp, tracks := dialViewerLeg(t, true)
	if !strings.Contains(strings.ToUpper(sdp), "PCMU/8000") {
		t.Fatalf("у answer глядача немає PCMU/8000:\n%s", sdp)
	}

	audio := awaitAudioTrack(t, vpc, tracks)
	if err := audio.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	// Шукаємо ТРИ поспіль мічені кадри: один міг би бути збігом, три — ні.
	streak := 0
	for i := 0; i < 400 && streak < 3; i++ {
		pkt, _, err := audio.ReadRTP()
		if err != nil {
			t.Fatalf("аудіо-пакет %d не прийшов (мічених поспіль %d): %v", i, streak, err)
		}
		if bytes.Equal(pkt.Payload, marked) {
			streak++
			continue
		}
		streak = 0
	}
	if streak < 3 {
		t.Fatal("до глядача не дійшло трьох поспіль кадрів агента — форвардинг звуку не працює")
	}
}

// TestAgentWithoutAudioFallsBackToTone — запасне джерело лишилось на місці:
// нода, чий агент звуку не оголосив (старий агент, вимкнений у нього прапорець,
// ПК без звукової карти), досі отримує тон. Це і є та перевірка тракту, заради
// якої тон свого часу й писався.
func TestAgentWithoutAudioFallsBackToTone(t *testing.T) {
	withAudioFlag(t, true)

	dialAgentLegWith(t, agentNodeIDEnv, false)
	ns := reg.get(agentNodeIDEnv)
	if ns == nil {
		t.Fatal("агентська нога не зареєструвала ноду")
	}
	if ns.hasAgentAudio() {
		t.Fatal("хаб вирішив, що агент шле звук, хоча той цього не оголошував")
	}

	vpc, _, tracks := dialViewerLeg(t, true)
	audio := awaitAudioTrack(t, vpc, tracks)
	if err := audio.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	for i := 0; i < 5; i++ {
		pkt, _, err := audio.ReadRTP()
		if err != nil {
			t.Fatalf("кадр тону %d/5 не прийшов: %v", i+1, err)
		}
		if len(pkt.Payload) != pcmu.FrameSamples {
			t.Fatalf("кадр тону %d має %d байт, want %d", i, len(pkt.Payload), pcmu.FrameSamples)
		}
	}
}

// TestToneIsNotConstant — санітарна перевірка самої мітки з тесту вище: якщо
// тон раптом стане постійним кадром, TestAgentAudioReachesViewer почне брехати
// («агент дійшов», хоча то тон). Дешевше перевірити тут, ніж ловити потім.
func TestToneIsNotConstant(t *testing.T) {
	var tone audioTone
	for i := 0; i < 10; i++ {
		f := tone.next()
		if len(f) != pcmu.FrameSamples {
			t.Fatalf("кадр тону %d байт, want %d", len(f), pcmu.FrameSamples)
		}
		if bytes.Equal(f, bytes.Repeat(f[:1], len(f))) {
			t.Fatalf("кадр %d тону складається з однакових байтів — мітка джерела втратила сенс", i)
		}
	}
}

// TestRecordAudioSlotFollowsListeningLeg — R5-G6: слот запису звуку тримає нога,
// якій звук реально йде. Два глядачі, слот узяв A; A сховав вкладку без звуку
// (audioLive=false), B чує. Звук мусить і далі потрапляти у файл — через B.
// Прибери віддачу слота на тиші в audioPump — A триматиме його, і запис
// оглухне, поки A прихований.
func TestRecordAudioSlotFollowsListeningLeg(t *testing.T) {
	withAudioFlag(t, true)
	ns := readyNode(t, "rec-slot")
	a := ns.onlyViewer(t)
	b := addReadyViewerNotFirst(t, ns)
	setAgentAudio(ns, true)

	// Рекордер без писаря: offerAudio кладе кадри в чергу, звідки їх і читаємо.
	rec := &recorder{ch: make(chan recItem, 4096), done: make(chan struct{})}
	ns.rec.Store(rec)
	for _, vl := range []*viewerLeg{a, b} {
		trk, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: pcmu.Rate}, "audio", "oo-screen")
		if err != nil {
			t.Fatal(err)
		}
		go vl.audioPump(ns, trk)
	}

	frame := bytes.Repeat([]byte{0x11}, pcmu.FrameSamples)
	// feedUntil годує ноду кадрами в темпі агента, доки у файл не ляже want
	// кадрів. Вердикт — за ПОДІЄЮ (кадри дійшли), а не за вікном у N мс: раніше
	// тест давав pump-ам рівно 400+500 мс настінного часу, і під -race на
	// завантаженій машині горутина A просто не встигала прокинутись на своєму
	// audioIdleTick — «0 кадрів» без жодної вади в коді. stuckGuard — лише
	// запобіжник: без віддачі слота на тиші (регресія, яку ловить тест) A
	// тримає його вічно, і кадрів не буде за будь-який час.
	const stuckGuard = 30 * time.Second
	feedUntil := func(want int) (recorded int) {
		for end := time.Now().Add(stuckGuard); recorded < want && time.Now().Before(end); {
			forwardAudioToViewers(ns, frame)
			time.Sleep(pcmu.Duration(pcmu.FrameSamples))
			for drained := false; !drained; {
				select {
				case it := <-rec.ch:
					if it.aud != nil {
						recorded++
					}
				default:
					drained = true
				}
			}
		}
		return recorded
	}

	// Слот віддаємо саме A: B поки не чує.
	setViewerHidden(ns, b, true)
	if n := feedUntil(1); n == 0 {
		t.Fatal("звук не пишеться навіть з одним слухачем")
	}
	// A сховався без звуку, B повернувся й чує. A ще може дописати свою
	// чергу (≤ audioQueueDepth кадрів) — тому чекаємо на 10 кадрів ПОНАД неї:
	// їх уже може написати лише B.
	setViewerHidden(ns, b, false)
	setViewerHidden(ns, a, true)
	if n := feedUntil(10 + audioQueueDepth); n < 10+audioQueueDepth {
		t.Fatalf("B чує звук, а у файл за %s лягло %d кадрів — слот застряг у прихованого A", stuckGuard, n)
	}
}
