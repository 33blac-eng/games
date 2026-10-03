package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/h264"
	"github.com/organicoils/oo-screen/internal/pcmu"
)

// withRecordFlag ставить прапорець і каталог на час тесту й повертає їх назад —
// той самий прийом, що withAudioFlag в audio_test.go. Каталог порожній: сам факт
// появи в ньому чогось і є доказом у тесті нижче.
func withRecordFlag(t *testing.T, on bool) string {
	t.Helper()
	dir := t.TempDir()
	prevOn, prevDir := recordEnabled, recordDir
	recordEnabled, recordDir = on, dir
	t.Cleanup(func() { recordEnabled, recordDir = prevOn, prevDir })
	return dir
}

// silentViewer — Connected глядач БЕЗ pump-а: черга нікуди не витікає, тож після
// forwardToViewers її можна прочитати й побачити рівно те, що хаб віддав.
func silentViewer(t *testing.T, ns *nodeSession) *viewerLeg {
	t.Helper()
	vl := &viewerLeg{
		pc:   &webrtc.PeerConnection{}, // сентинел-ключ мапи, як у readyNode
		trk:  newViewerTrack(t),
		out:  make(chan *rtp.Packet, 64),
		done: make(chan struct{}),
	}
	ns.mu.Lock()
	ns.viewers = map[*webrtc.PeerConnection]*viewerLeg{vl.pc: vl}
	vl.ready, vl.live = true, true
	ns.mu.Unlock()
	return vl
}

// TestRecordFlagOffChangesNothing — ГОЛОВНИЙ тест хвилі: без OO_SCREEN_RECORD
// хаб поводиться рівно як до появи запису. Це захист робочого проду, а не фічі.
//
// Три половини правди:
//  1. Рекордера не існує — startRecording віддає nil.
//  2. Усі чотири точки дотику з прод-кодом на nil — no-op, а не паніка.
//  3. Форвардинг віддає глядачеві БАЙТ-У-БАЙТ той самий потік, і на диску не
//     зʼявляється нічого.
func TestRecordFlagOffChangesNothing(t *testing.T) {
	// Дефолт перевіряємо ДО підміни: інакше тест доводив би лише те, що
	// перемикач працює, а не те, що ТИПОВО він вимкнений — а прод захищає саме
	// друге. Перевернутий дефолт падає тут.
	if recordEnabled != (os.Getenv("OO_SCREEN_RECORD") == "1") {
		t.Fatalf("recordEnabled=%v при OO_SCREEN_RECORD=%q — типово запис мусить бути ВИМКНЕНИЙ",
			recordEnabled, os.Getenv("OO_SCREEN_RECORD"))
	}

	dir := withRecordFlag(t, false)

	rec := startRecording("n1")
	if rec != nil {
		t.Fatalf("startRecording без прапорця віддав %v, want nil", rec)
	}
	rec.offer(&rtp.Packet{Payload: []byte{1, 2, 3}})
	rec.offerAudio([]byte{4, 5, 6}, 20*time.Millisecond)
	if rec.claimAudio() {
		t.Fatal("nil-рекордер віддав слот аудіо — audioPump почав би годувати ніщо")
	}
	rec.releaseAudio()
	rec.Close() // не має ні впасти, ні зависнути на <-r.fin

	ns := &nodeSession{nodeID: "off"}
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{}
	ns.mu.Unlock()
	ns.rec.Store(startRecording(ns.nodeID))
	if ns.rec.Load() != nil {
		t.Fatal("ns.rec не nil без прапорця — audioPump знайшов би рекордер")
	}
	vl := silentViewer(t, ns)

	const n = 8
	for i := 0; i < n; i++ {
		forwardToViewers(ns, agentGen1, &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(1000 + i), Timestamp: uint32(900000 + i*3000)},
			Payload: []byte{byte(i), 0xAA},
		})
	}
	if got := len(vl.out); got != n {
		t.Fatalf("глядач отримав %d пакетів, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		got := <-vl.out
		if got.SequenceNumber != uint16(i) || got.Timestamp != uint32(i*3000) {
			t.Fatalf("пакет %d: seq=%d ts=%d, want seq=%d ts=%d",
				i, got.SequenceNumber, got.Timestamp, i, i*3000)
		}
		if !bytes.Equal(got.Payload, []byte{byte(i), 0xAA}) {
			t.Fatalf("пакет %d: payload=%v — запис зачепив те, що бачить глядач", i, got.Payload)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("з вимкненим прапорцем у %s зʼявилось %d записів: %v", dir, len(entries), entries)
	}
}

// TestRecordWritesClosedMKVWithBothTracks — під прапорцем зʼявляється MKV із
// ДВОМА доріжками, і він КОРЕКТНО закритий після обриву агента.
//
// «Коректно закритий» перевіряється не оком, а розбором: ebmlWalk дочитує файл
// до останнього байта й падає, щойно якийсь елемент заявив більше, ніж лишилось
// — тобто рівно на обрізаному хвості, який плеєр і не відкриє.
func TestRecordWritesClosedMKVWithBothTracks(t *testing.T) {
	dir := withRecordFlag(t, true)
	withAudioFlag(t, true) // друга доріжка існує лише разом зі своїм прапорцем

	aus := corpusAUs(t)
	tone := toneFrames(t)

	rec := startRecording("../../etc/passwd") // заодно перевірка санітизації імені
	if rec == nil {
		t.Fatal("під прапорцем startRecording віддав nil")
	}

	var seq uint16
	var ts uint32
	for i, au := range aus {
		for _, p := range packetizeAU(au.Data, ts, &seq) {
			rec.offer(p)
		}
		// Два 20-мс кадри звуку на кожен кадр відео.
		rec.offerAudio(tone[(2*i)%len(tone)], 20*time.Millisecond)
		rec.offerAudio(tone[(2*i+1)%len(tone)], 20*time.Millisecond)
		// СВІДОМО нерівний крок: 100..300 мс на кадр — це і є той змінний fps,
		// заради якого обрано MKV, а не MP4.
		ts += 9000 + uint32(i%5)*4500
	}
	rec.Close() // «агент відпав» — той самий шлях, що defer у read loop

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("у %s %d файлів, want 1: %v", dir, len(entries), entries)
	}
	name := entries[0].Name()
	if strings.ContainsAny(name, `/\`) || !strings.HasPrefix(name, ".._.._etc_passwd-") || !strings.HasSuffix(name, ".mkv") {
		t.Fatalf("імʼя файлу %q — node_id потрапив у шлях несанітизованим", name)
	}
	path := filepath.Join(dir, name)

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var s mkvSummary
	s.blocks = map[byte]int{}
	ebmlWalk(t, blob, &s, "")

	if len(s.codecIDs) != 2 || s.codecIDs[0] != codecIDH264 || s.codecIDs[1] != codecIDPCM {
		t.Fatalf("доріжки = %v, want [%s %s]", s.codecIDs, codecIDH264, codecIDPCM)
	}
	if s.width != 1920 || s.height != 1080 {
		t.Fatalf("розмір у файлі %dx%d, want 1920x1080 (SPS корпусу)", s.width, s.height)
	}
	if s.clusters < 2 {
		t.Fatalf("кластерів %d, want >=2 — ротація по ключовому кадру не спрацювала", s.clusters)
	}
	if s.blocks[mkvVideoTrack] != len(aus) {
		t.Fatalf("кадрів відео у файлі %d, want %d", s.blocks[mkvVideoTrack], len(aus))
	}
	// Рівно два кадри звуку не потрапляють у файл, і це не втрата, а межа за
	// побудовою: файл народжується на ПЕРШОМУ ключовому AU, а той закривається
	// лише коли прийде перший пакет наступного кадру. Звук, запропонований до
	// цієї миті (тут — пара кадрів ітерації 0), класти нікуди. У бою це десятки
	// мілісекунд: перший глядач одразу просить keyframe (resetBitrate).
	if want := 2*len(aus) - 2; s.blocks[mkvAudioTrack] != want {
		t.Fatalf("кадрів звуку у файлі %d, want %d", s.blocks[mkvAudioTrack], want)
	}

	ffprobeMKV(t, path)
}

// TestRecordAudioSlotIsExclusive — слот годувальника аудіо дістається рівно
// одному: без цього N глядачів написали б N копій тону в одну доріжку.
func TestRecordAudioSlotIsExclusive(t *testing.T) {
	withRecordFlag(t, true)
	rec := startRecording("slot")
	t.Cleanup(rec.Close)

	if !rec.claimAudio() {
		t.Fatal("перший claimAudio не дав слота")
	}
	if rec.claimAudio() {
		t.Fatal("другий claimAudio теж дав слот — доріжка отримала б дубль звуку")
	}
	rec.releaseAudio()
	if !rec.claimAudio() {
		t.Fatal("після releaseAudio слот не звільнився — нова нога лишилась би без звуку")
	}
}

// --- допоміжне: джерела реального медіа ---

// corpusAUs — реальні AU з bench/corpus: 6 навколо першого IDR і 6 навколо
// другого (кожні 2с = кожні 120 кадрів). Два ключові кадри в наборі потрібні,
// щоб перевірити ще й ротацію кластера.
//
// Особливість саме цієї фікстури: SplitAUs лишає замикальний SEI (тип 6) у
// ХВОСТІ кадру, хоч належить він наступному — межу AU він не відкриває. Через
// це ffmpeg на отриманому файлі каже «Late SEI is not implemented» (і все одно
// декодує всі кадри). Це властивість фікстури, а не запису: у бою межі AU дає
// RTP-timestamp агента, і SEI кадру N+1 приїжджає вже з його timestamp.
func corpusAUs(t *testing.T) []h264.AU {
	t.Helper()
	data, err := os.ReadFile("../../../bench/corpus/corpus-1080p60.h264")
	if err != nil {
		t.Skipf("корпус відсутній: %v", err)
	}
	all := h264.SplitAUs(data)
	if len(all) < 126 {
		t.Fatalf("у корпусі %d AU, треба хоча б 126", len(all))
	}
	out := append([]h264.AU{}, all[0:6]...)
	return append(out, all[120:126]...)
}

// toneFrames — справжні кадри μ-law із того самого запасного тону, який хаб шле
// у браузер (audio.go). Фейкові байти тут не годяться: ffprobe нижче має бачити
// доріжку, яку реально можна відкрити.
func toneFrames(t *testing.T) [][]byte {
	t.Helper()
	var tone audioTone
	out := make([][]byte, 24)
	for i := range out {
		out[i] = tone.next()
		if len(out[i]) != pcmu.FrameSamples {
			t.Fatalf("кадр тону %d байт, want %d", len(out[i]), pcmu.FrameSamples)
		}
	}
	return out
}

// packetizeAU робить із AU те, що робить агент: одиночні NAL-пакети, а великі
// слайси — фрагментами FU-A. Саме FU-A і є пастка, яку має закрити збирач.
func packetizeAU(au []byte, ts uint32, seq *uint16) []*rtp.Packet {
	const mtu = 1100
	var out []*rtp.Packet
	add := func(payload []byte) {
		out = append(out, &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96,
				SequenceNumber: *seq, Timestamp: ts, SSRC: 0xDEADBEEF,
			},
			Payload: payload,
		})
		*seq++
	}
	for _, nal := range h264.SplitNALs(au) {
		if len(nal) <= mtu {
			add(append([]byte(nil), nal...))
			continue
		}
		hdr, body := nal[0], nal[1:]
		for i := 0; i < len(body); i += mtu {
			end := min(i+mtu, len(body))
			fu := []byte{(hdr & 0xE0) | 28, hdr & 0x1F}
			if i == 0 {
				fu[1] |= 0x80 // start
			}
			if end == len(body) {
				fu[1] |= 0x40 // end
			}
			add(append(fu, body[i:end]...))
		}
	}
	if len(out) > 0 {
		out[len(out)-1].Marker = true
	}
	return out
}

// --- допоміжне: розбір готового MKV ---

type mkvSummary struct {
	codecIDs      []string
	clusters      int
	blocks        map[byte]int
	width, height uint64
}

// ebmlWalk обходить документ і ПАДАЄ, щойно елемент заявляє більше байтів, ніж
// лишилось у батька. Саме це й означає «файл коректно закритий»: обрізаний
// хвіст (незакритий запис) тут не пройде.
func ebmlWalk(t *testing.T, b []byte, s *mkvSummary, path string) {
	t.Helper()
	for i := 0; i < len(b); {
		id, iw, ok := readEBMLID(b[i:])
		if !ok {
			t.Fatalf("обрізаний ID у %s/ на зсуві %d (лишилось %d байтів)", path, i, len(b)-i)
		}
		i += iw
		size, sw, unknown, ok := readEBMLSize(b[i:])
		if !ok {
			t.Fatalf("обрізаний розмір елемента %X у %s/ (лишилось %d байтів)", id, path, len(b)-i)
		}
		i += sw

		var body []byte
		if unknown {
			body, i = b[i:], len(b)
		} else {
			if uint64(len(b)-i) < size {
				t.Fatalf("елемент %X у %s/ заявив %d байтів, лишилось %d — файл ОБРІЗАНИЙ (некоректно закритий)",
					id, path, size, len(b)-i)
			}
			body, i = b[i:i+int(size)], i+int(size)
		}

		switch id {
		case idSegment, idTracks, idTrackEntry, idVideo:
			ebmlWalk(t, body, s, fmt.Sprintf("%s/%X", path, id))
		case idCluster:
			s.clusters++
			ebmlWalk(t, body, s, fmt.Sprintf("%s/%X", path, id))
		case idCodecID:
			s.codecIDs = append(s.codecIDs, string(body))
		case idPixelWidth:
			s.width = beUint(body)
		case idPixelHeight:
			s.height = beUint(body)
		case idSimpleBlock:
			if len(body) < 5 {
				t.Fatalf("SimpleBlock у %s/ має %d байтів — кадру в ньому немає", path, len(body))
			}
			s.blocks[body[0]&0x7F]++
		}
	}
}

func readEBMLID(b []byte) (uint32, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	w := 0
	for m := byte(0x80); m != 0 && w == 0; m >>= 1 {
		if b[0]&m != 0 {
			w = 1
			for n := byte(0x80); n > m; n >>= 1 {
				w++
			}
		}
	}
	if w == 0 || w > 4 || len(b) < w {
		return 0, 0, false
	}
	var v uint32
	for _, c := range b[:w] {
		v = v<<8 | uint32(c)
	}
	return v, w, true
}

func readEBMLSize(b []byte) (val uint64, width int, unknown, ok bool) {
	if len(b) == 0 {
		return 0, 0, false, false
	}
	mask := byte(0x80)
	width = 1
	for mask != 0 && b[0]&mask == 0 {
		mask >>= 1
		width++
	}
	if mask == 0 || len(b) < width {
		return 0, 0, false, false
	}
	val = uint64(b[0] &^ mask)
	for _, c := range b[1:width] {
		val = val<<8 | uint64(c)
	}
	return val, width, val == uint64(1)<<(7*uint(width))-1, true
}

func beUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// ffprobeMKV — зовнішній суддя. Якщо ffprobe у системі немає, тест не вигадує
// результат, а прямо каже, що цієї перевірки не було.
func ffprobeMKV(t *testing.T, path string) {
	t.Helper()
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Log("ffprobe у системі немає — зовнішньої перевірки файлу НЕ БУЛО")
		return
	}
	out, err := exec.Command(bin, "-v", "error",
		"-show_entries", "stream=index,codec_name,codec_type,width,height",
		"-of", "default=noprint_wrappers=1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe %s: %v\n%s", path, err, out)
	}
	got := string(out)
	t.Logf("ffprobe:\n%s", got)
	for _, want := range []string{"codec_name=h264", "codec_name=pcm_s16le", "width=1920", "height=1080"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ffprobe не побачив %q:\n%s", want, got)
		}
	}
}

// TestMKVReadableWithoutClose — доказ головного рішення формату: файл читається
// БЕЗ жодного Close(). Тут його навмисно не викликають — це модель kill -9 (або
// зникнення живлення) одразу після того, як черговий кластер ліг на диск.
// Читабельним лишається все до останнього ЦІЛОГО кластера, і саме на цьому, а
// не на акуратному завершенні, тримається вимога «файл мусить відкритися».
func TestMKVReadableWithoutClose(t *testing.T) {
	var buf bytes.Buffer
	m := newMKVWriter(&buf)
	if err := m.writeHeader(640, 480, avcC([]byte{0x67, 0x64, 0x00, 0x2A}, []byte{0x68, 0xEE}), false); err != nil {
		t.Fatalf("writeHeader: %v", err)
	}
	// Ключові кадри на 0 і 4500 мс: другий відкриває новий кластер, а перший
	// на цьому лягає на диск цілим.
	for i := 0; i < 6; i++ {
		m.block(mkvVideoTrack, int64(i)*1500, i%3 == 0, []byte{0, 0, 0, 2, 9, 0xF0})
	}
	// Close() СВІДОМО не кличемо.

	var s mkvSummary
	s.blocks = map[byte]int{}
	ebmlWalk(t, buf.Bytes(), &s, "")

	if s.clusters != 1 {
		t.Fatalf("кластерів у недописаному файлі %d, want 1", s.clusters)
	}
	if s.blocks[mkvVideoTrack] != 3 {
		t.Fatalf("кадрів у недописаному файлі %d, want 3 (усе до ротації)", s.blocks[mkvVideoTrack])
	}
	if s.width != 640 || s.height != 480 {
		t.Fatalf("заголовок недописаного файлу нечитабельний: %dx%d", s.width, s.height)
	}
}
