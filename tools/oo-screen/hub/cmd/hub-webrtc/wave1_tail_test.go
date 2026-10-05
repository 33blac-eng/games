// Гейти хвоста хвилі 1 по хабу: H-07/H-27 (безшовна заміна публікатора),
// H-12 (drop-to-IDR замість розриву ноги), H-18 (неузгоджений кодек у SDP).
//
// Як і в wave1_hub_test.go, кожен тест тут вміє почервоніти: під кожним стоїть
// рядок «зніми правку X — і цей тест падає». Гейт, зелений і до фікса, вартий
// рівно нуля.
package main

import (
	"testing"

	"github.com/pion/rtp"
)

// Покоління агентської ноги у тестах. Один і той самий agentGen1 у всіх старих
// викликах forwardToViewers означає «публікатора не міняли» — тобто стару
// поведінку слово в слово.
const (
	agentGen1 = uint64(1)
	agentGen2 = uint64(2)
)

// genPacket — пакет із явними seq/ts, щоб тест міг задати РІЗНІ простори
// нумерації двох поколінь.
func genPacket(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts},
		Payload: []byte{0x21, 0x01, 0x02}, // NAL type 1 — не ключовий
	}
}

// drainSeq вигрібає чергу ноги у два зрізи: egress-seq і egress-ts.
func drainSeq(vl *viewerLeg) ([]uint16, []uint32) {
	var seqs []uint16
	var tss []uint32
	for {
		select {
		case p := <-vl.out:
			seqs = append(seqs, p.SequenceNumber)
			tss = append(tss, p.Timestamp)
		default:
			return seqs, tss
		}
	}
}

// ── H-07/H-27 ───────────────────────────────────────────────────────────────

// TestPublisherSwapKeepsEgressSeamless — заміна агента НЕ видно в нумерації,
// яку читає глядач. Новий агент починає з власного випадкового seq/ts; якщо
// хаб порахує від нього звичайну дельту до чужого відліку, глядач побачить
// діру в десятки тисяч пакетів — NACK-шторм на весь уявний проміжок і скид
// джитер-буфера.
//
// Зніми гілку `case ns.egressGen != gen` у forwardToViewers (щоб delta
// рахувалась завжди) — і egress-seq тут стрибне з 3 на ~39900, тест впаде.
func TestPublisherSwapKeepsEgressSeamless(t *testing.T) {
	quietNDJSON(t, nil)

	ns := &nodeSession{nodeID: "swap"}
	vl := quietViewer(ns)

	// Покоління 1: три пакети поспіль у своєму просторі нумерації.
	forwardToViewers(ns, agentGen1, genPacket(100, 300_000))
	forwardToViewers(ns, agentGen1, genPacket(101, 303_000))
	forwardToViewers(ns, agentGen1, genPacket(102, 306_000))

	// Покоління 2: агента замінили. seq і ts — з іншого всесвіту, причому seq
	// МЕНШИЙ за попередній вхідний, а ts — набагато більший.
	forwardToViewers(ns, agentGen2, genPacket(40_000, 7_000_000))
	forwardToViewers(ns, agentGen2, genPacket(40_001, 7_003_000))
	forwardToViewers(ns, agentGen2, genPacket(40_002, 7_006_000))

	seqs, tss := drainSeq(vl)
	if len(seqs) != 6 {
		t.Fatalf("глядач отримав %d пакетів, want 6", len(seqs))
	}

	// Головний інваріант: жодної діри і жодного стрибка назад.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("egress-seq %v: розрив %d -> %d на кроці %d (глядач вимагатиме NACK на все, чого немає)",
				seqs, seqs[i-1], seqs[i], i)
		}
	}

	// Час строго зростає — інакше джитер-буфер відкидає кадр як застарілий.
	for i := 1; i < len(tss); i++ {
		if tss[i] <= tss[i-1] {
			t.Fatalf("egress-ts %v: %d не більший за попередній %d на кроці %d", tss, tss[i], tss[i-1], i)
		}
	}

	// Крок на самій заміні — рівно seamlessTSStep, а не дельта чужого простору.
	if got := tss[3] - tss[2]; got != seamlessTSStep {
		t.Fatalf("крок ts на заміні агента %d, want %d", got, seamlessTSStep)
	}
	// А одразу після заміни темп веде вже НОВИЙ агент, своїми дельтами.
	if got := tss[4] - tss[3]; got != 3000 {
		t.Fatalf("крок ts першої дельти нового покоління %d, want 3000", got)
	}
}

// TestPublisherSwapDropsStaleGop — кеш GOP від ПОПЕРЕДНЬОГО кодера не має
// пережити заміну: його SPS/PPS новому потоку не підходять, і нова нога
// отримала б кадри, яких у поточному потоці вже немає.
//
// Прибери ns.gop.reset() з гілки genSwitched — і в кеші лишиться пакет
// старого покоління, тест впаде.
func TestPublisherSwapDropsStaleGop(t *testing.T) {
	quietNDJSON(t, nil)

	ns := &nodeSession{nodeID: "swap-gop"}
	quietViewer(ns)

	forwardToViewers(ns, agentGen1, keyPacket(1))
	forwardToViewers(ns, agentGen1, nonKeyPacket(2))

	ns.mu.Lock()
	before := len(ns.gop.replay())
	ns.mu.Unlock()
	if before != 2 {
		t.Fatalf("до заміни в кеші %d пакетів, want 2", before)
	}

	// Перший пакет нового покоління — НЕ ключовий: кешувати нема від чого.
	forwardToViewers(ns, agentGen2, genPacket(40_000, 7_000_000))

	ns.mu.Lock()
	after := len(ns.gop.replay())
	ns.mu.Unlock()
	if after != 0 {
		t.Fatalf("після заміни агента в кеші лишилось %d пакетів старого кодера, want 0", after)
	}
}

// ── H-12 ────────────────────────────────────────────────────────────────────

// stalledLeg — нога з ПОВНОЮ чергою, яку ніхто не читає: рівно стан «глядач не
// встигає за джерелом». PeerConnection тут СПРАВЖНЯ (stalledViewer -> newPC):
// шлях H-12 закінчується dropViewer-ом, а той кличе Close(), і на підробці
// pion падає на закритті nil-каналу.
func stalledLeg(t *testing.T, ns *nodeSession) *viewerLeg {
	t.Helper()
	return stalledViewer(t, ns, newPC(t))
}

// TestOverflowEntersDropToIDRInsteadOfKillingLeg — переповнення черги НЕ рве
// сесію. Нога лишається в ноді й переходить у drop-to-IDR: наступні НЕключові
// пакети до неї не кладуть узагалі, і саме ця пауза дає їй розібрати чергу.
//
// Поверни `slow = append(slow, vl)` беззастережно у гілку default
// forwardToViewers — і нога зникне з ns.viewers, тест впаде.
func TestOverflowEntersDropToIDRInsteadOfKillingLeg(t *testing.T) {
	quietNDJSON(t, nil)

	ns := &nodeSession{nodeID: "overflow"}
	vl := stalledLeg(t, ns)

	// Перше переповнення.
	forwardToViewers(ns, agentGen1, nonKeyPacket(1))

	ns.mu.Lock()
	_, alive := ns.viewers[vl.pc]
	discarding, streak := vl.discarding, vl.overflowStreak
	ns.mu.Unlock()

	if !alive {
		t.Fatalf("ногу відірвано на першому ж переповненні — сесія глядача вбита замість скидання кадрів")
	}
	if !discarding {
		t.Fatalf("нога не перейшла в drop-to-IDR (discarding=false)")
	}
	if streak != 1 {
		t.Fatalf("overflowStreak = %d, want 1", streak)
	}

	// Поки нога в drop-to-IDR, НЕключові пакети до неї навіть не пробують
	// потрапити: лічильник переповнень не росте.
	forwardToViewers(ns, agentGen1, nonKeyPacket(2))
	forwardToViewers(ns, agentGen1, nonKeyPacket(3))

	ns.mu.Lock()
	streak = vl.overflowStreak
	ns.mu.Unlock()
	if streak != 1 {
		t.Fatalf("overflowStreak = %d після двох скинутих кадрів, want 1 (кадри мали піти в смітник, а не в чергу)", streak)
	}
}

// TestDropToIDRResumesOnKeyframe — нога виходить із drop-to-IDR саме на
// ключовому пакеті: це єдина точка, з якої декодер уміє почати заново.
//
// Прибери перевірку isKey (щоб discarding знімався на будь-якому пакеті) — і
// нога поновиться посеред GOP, тест впаде на discarding після НЕключового.
func TestDropToIDRResumesOnKeyframe(t *testing.T) {
	quietNDJSON(t, nil)

	ns := &nodeSession{nodeID: "resume"}
	vl := stalledLeg(t, ns)

	forwardToViewers(ns, agentGen1, nonKeyPacket(1)) // -> drop-to-IDR
	forwardToViewers(ns, agentGen1, nonKeyPacket(2)) // не знімає режим

	ns.mu.Lock()
	stillDiscarding := vl.discarding
	ns.mu.Unlock()
	if !stillDiscarding {
		t.Fatalf("нога вийшла з drop-to-IDR на НЕключовому пакеті — декодер почати не зможе")
	}

	// Звільняємо місце в черзі — нога наздогнала — і даємо ключовий пакет.
	<-vl.out
	forwardToViewers(ns, agentGen1, keyPacket(3))

	ns.mu.Lock()
	resumed := !vl.discarding
	ns.mu.Unlock()
	if !resumed {
		t.Fatalf("нога не поновилась на ключовому пакеті")
	}
	// Останній у черзі — саме той ключовий пакет.
	var last *rtp.Packet
	for len(vl.out) > 0 {
		last = <-vl.out
	}
	if last == nil || !h264KeyPart(last.Payload) {
		t.Fatalf("у чергу після поновлення поклали не ключовий пакет")
	}
}

// TestSlowViewerDroppedOnlyAfterStreak — друга половина H-12: скидання кадрів
// не має стати вічним. Нога, якій воно не допомогло viewerOverflowStreakMax
// разів ПОСПІЛЬ, усе ж рветься — інакше безнадійний глядач тримав би ресурси
// ноди назавжди.
//
// Зроби поріг нескінченним (прибери гілку overflowStreak >= max) — і нога
// доживе до кінця тесту, який на цьому й впаде.
func TestSlowViewerDroppedOnlyAfterStreak(t *testing.T) {
	quietNDJSON(t, nil)

	ns := &nodeSession{nodeID: "streak"}
	vl := stalledLeg(t, ns)

	// Кожен КЛЮЧОВИЙ пакет знімає drop-to-IDR і тут же впирається в ту саму
	// повну чергу — тобто це і є «переповнення поспіль».
	for i := 1; i < viewerOverflowStreakMax; i++ {
		forwardToViewers(ns, agentGen1, keyPacket(i))
		ns.mu.Lock()
		_, alive := ns.viewers[vl.pc]
		ns.mu.Unlock()
		if !alive {
			t.Fatalf("ногу відірвано на %d-му переповненні, want не раніше %d-го", i, viewerOverflowStreakMax)
		}
	}

	forwardToViewers(ns, agentGen1, keyPacket(viewerOverflowStreakMax))

	ns.mu.Lock()
	_, alive := ns.viewers[vl.pc]
	ns.mu.Unlock()
	if alive {
		t.Fatalf("нога пережила %d переповнень поспіль — безнадійний глядач тримає ноду вічно", viewerOverflowStreakMax)
	}
}

// ── H-18 ────────────────────────────────────────────────────────────────────

// sdpWithVideo — мінімальний answer-SDP із однією відеосекцією.
func sdpWithVideo(mline string, attrs ...string) string {
	s := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\n" +
		"a=fmtp:111 minptime=10;useinbandfec=1\r\n" + mline + "\r\n"
	for _, a := range attrs {
		s += a + "\r\n"
	}
	return s
}

// TestVideoCodecMismatch — H-18: неузгоджений кодек ловиться ДО того, як хаб
// віддасть 200 OK. Без цієї перевірки все виглядало здоровим — ICE піднімався,
// RTP летів — а людина нескінченно дивилась у сірий екран.
//
// Прибери виклик videoCodecMismatch у гілці leg == "viewer" — і випадки нижче
// перестануть відрізнятись від здорових.
func TestVideoCodecMismatch(t *testing.T) {
	const videoM = "m=video 9 UDP/TLS/RTP/SAVPF 102"

	cases := []struct {
		name     string
		sdp      string
		wantBad  bool
		wantCode string
	}{
		{
			name:    "наш власний профіль — усе гаразд",
			sdp:     sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 "+h264FmtpLine),
			wantBad: false,
		},
		{
			// Рівно той випадок, на якому перша редакція перевірки завалила
			// власні ж тести: level-asymmetry-allowed=1 дозволяє різні рівні.
			name:    "той самий профіль, ІНШИЙ рівень — це норма",
			sdp:     sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 packetization-mode=1;profile-level-id=4d002a"),
			wantBad: false,
		},
		{
			name:     "інший профіль (baseline замість main)",
			sdp:      sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 packetization-mode=1;profile-level-id=42e01f"),
			wantBad:  true,
			wantCode: "h264_profile_mismatch",
		},
		{
			name:    "H.264 без profile-level-id",
			sdp:     sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 packetization-mode=1"),
			wantBad: true,
		},
		{
			name:    "у відео лишився лише VP8 — форвардити нічим",
			sdp:     sdpWithVideo("m=video 9 UDP/TLS/RTP/SAVPF 96", "a=rtpmap:96 VP8/90000"),
			wantBad: true,
		},
		{
			name:    "відеодоріжку відхилено (m=video 0)",
			sdp:     sdpWithVideo("m=video 0 UDP/TLS/RTP/SAVPF 102", "a=rtpmap:102 H264/90000", "a=fmtp:102 "+h264FmtpLine),
			wantBad: true,
		},
		{
			name:    "відеосекції немає взагалі",
			sdp:     "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\n",
			wantBad: true,
		},
		{
			// Opus у секції звуку має власний fmtp і власний PT — він не сміє
			// ані зіпсувати перевірку, ані підмінити собою H.264.
			name:    "H.264 у відео + Opus у звуці",
			sdp:     sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 "+h264FmtpLine),
			wantBad: false,
		},
		{
			// РЕАЛЬНИЙ випадок 05.09: наш енкодер віддає SPS 4D402A (Main із
			// constraint_set1), Chrome оголошує 4d001f без прапорців. Прапорці
			// ГЛЯДАЧА (0x00) — підмножина наших (0x40), тобто наш потік
			// строго простіший за те, що глядач готовий декодувати. Приймаємо.
			// Зворотне порівняння відхиляло рівно того глядача, заради якого
			// все це й робилось.
			name:    "глядач 4d001f проти нашого 4d40xx — приймаємо",
			sdp:     sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 packetization-mode=1;profile-level-id=4d001f"),
			wantBad: false,
		},
		{
			// Асиметрія навмисна: глядач, який пообіцяв БІЛЬШЕ констрейнтів,
			// ніж наш потік, може не впоратись із нашим менш обмеженим
			// потоком. Тут чесніше відмовити, ніж віддати кашу.
			name:     "глядач 4d40xx проти менш обмеженого потоку — 415",
			sdp:      sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 packetization-mode=1;profile-level-id=4d401f"),
			wantBad:  true,
			wantCode: "h264_profile_mismatch",
		},
		{
			// Негативний бік тієї ж правки: послаблення НЕ сміє пропустити
			// чужий profile_idc. High тепер саме такий — хаб на Main.
			name:     "High 640c1f — інший profile_idc, і далі 415",
			sdp:      sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 packetization-mode=1;profile-level-id=640c1f"),
			wantBad:  true,
			wantCode: "h264_profile_mismatch",
		},
		{
			name:     "High 4:4:4 f4001f — інший декодер, 415",
			sdp:      sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 packetization-mode=1;profile-level-id=f4001f"),
			wantBad:  true,
			wantCode: "h264_profile_mismatch",
		},
		{
			name:     "сміття замість profile-level-id",
			sdp:      sdpWithVideo(videoM, "a=rtpmap:102 H264/90000", "a=fmtp:102 packetization-mode=1;profile-level-id=zzzz1f"),
			wantBad:  true,
			wantCode: "h264_profile_mismatch",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := videoCodecMismatch(c.sdp, wantedProfileLevelID)
			if c.wantBad && why == nil {
				t.Fatalf("неузгодженість не помічено — глядач отримав би 200 OK і сірий екран")
			}
			if !c.wantBad && why != nil {
				t.Fatalf("здоровий answer відхилено: %s", why.Detail)
			}
			if c.wantBad && c.wantCode != "" && why.Error != c.wantCode {
				t.Fatalf("код причини %q, а фронт чекає %q", why.Error, c.wantCode)
			}
		})
	}
}

// TestFmtpParam — розбір fmtp має бути нечутливим до регістру й пробілів:
// SDP від чужих реалізацій пишеться як завгодно.
func TestFmtpParam(t *testing.T) {
	cases := []struct{ fmtp, name, want string }{
		{"packetization-mode=1;profile-level-id=64002A", "profile-level-id", "64002a"},
		{" packetization-mode=1 ; Profile-Level-Id = 64002a ", "profile-level-id", "64002a"},
		{"packetization-mode=1", "profile-level-id", ""},
		{"", "profile-level-id", ""},
	}
	for _, c := range cases {
		if got := fmtpParam(c.fmtp, c.name); got != c.want {
			t.Errorf("fmtpParam(%q, %q) = %q, want %q", c.fmtp, c.name, got, c.want)
		}
	}
}
