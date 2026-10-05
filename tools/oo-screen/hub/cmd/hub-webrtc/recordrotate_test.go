package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/h264"

	"github.com/pion/webrtc/v4"
)

// 15.09.2026: файл запису закривається після ОСТАННЬОГО глядача, а не разом з
// агентською ногою. Без цього сеанси різних днів зливались в один файл.
func TestRecordClosesAfterLastViewerLeaves(t *testing.T) {
	// Налаштування — у знімку ноди, а не в глобалах: таймер закриття читає
	// лише ns.recCfg.
	ns := &nodeSession{nodeID: "rot", recCfg: recordCfg{on: true, dir: t.TempDir(), idle: 20 * time.Millisecond}}
	vl := silentViewer(t, ns)
	ns.viewerCount.Store(1)
	rec := startRecording(ns.nodeID, ns.recCfg)
	if rec == nil {
		t.Fatal("startRecording з прапорцем віддав nil")
	}
	ns.rec.Store(rec)

	if !removeViewer(ns, vl) {
		t.Fatal("removeViewer не знайшов ногу")
	}
	if ns.viewerCount.Load() != 0 {
		t.Fatalf("viewerCount=%d після останнього глядача", ns.viewerCount.Load())
	}
	// Мутант «нульова затримка замість idle» ловиться тут: чекаємо половину
	// idle (де файл ще МАЄ бути відкритий) і лише тоді перевіряємо.
	time.Sleep(ns.recCfg.idle / 2)
	if ns.rec.Load() == nil {
		t.Fatal("файл запису закрито до спливу idle")
	}
	deadline := time.Now().Add(eventGuard)
	for ns.rec.Load() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ns.rec.Load() != nil {
		t.Fatal("файл запису не закрито після останнього глядача")
	}
}

// Глядач повернувся до спливу паузи — той самий файл лишається.
func TestRecordSurvivesQuickReconnect(t *testing.T) {
	// Налаштування — у знімку ноди, а не в глобалах: таймер закриття читає
	// лише ns.recCfg.
	ns := &nodeSession{nodeID: "rec2", recCfg: recordCfg{on: true, dir: t.TempDir(), idle: time.Second}}
	vl := silentViewer(t, ns)
	ns.viewerCount.Store(1)
	rec := startRecording(ns.nodeID, ns.recCfg)
	ns.rec.Store(rec)
	removeViewer(ns, vl)
	ns.mu.Lock()
	ns.viewers[&webrtc.PeerConnection{}] = vl
	ns.viewerCount.Store(1)
	ns.mu.Unlock()
	// idle = 1 с, а не 40 мс: вікно між removeViewer і поверненням глядача —
	// кілька інструкцій, але під -race/GOMAXPROCS=1 планувальник міг відкласти
	// їх на 40+ мс, і таймер закривав файл «законно» — хибний червоний.
	// Сон тут — лише щоб таймер устиг спрацювати (перевірка відсутності події).
	time.Sleep(ns.recCfg.idle + 500*time.Millisecond)
	if ns.rec.Load() != rec {
		t.Fatal("швидке перепідключення розірвало файл запису")
	}
	rec.Close()
}

// Інший SPS посеред сесії (select_output на монітор іншої роздільності чи
// рівня) = новий файл: заголовок старого (розмір, avcC) уже бреше. Прибери
// перевірку зміни SPS у flushAU — файл лишиться один.
func TestRecordRotatesOnSPSChange(t *testing.T) {
	dir := withRecordFlag(t, true)
	aus := corpusAUs(t)
	key := aus[0].Data

	var sps []byte
	for _, n := range h264.SplitNALs(key) {
		if n[0]&0x1F == h264.NALSPS {
			sps = n
		}
	}
	if len(sps) < 4 {
		t.Fatal("у першому AU корпусу немає SPS")
	}
	other := append([]byte(nil), sps...)
	other[3]++ // level_idc: SPS розбирається, але avcC уже інший
	key2 := bytes.Replace(key, sps, other, 1)

	rec := startRecording("node-sps", currentRecordCfg())
	var seq uint16
	var ts uint32
	for _, au := range [][]byte{key, key, key2, key2} {
		for _, p := range packetizeAU(au, ts, &seq) {
			rec.offer(p)
		}
		ts += 9000
	}
	rec.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.mkv"))
	if len(files) != 2 {
		t.Fatalf("файлів запису %d, want 2 (новий на зміні SPS): %v", len(files), files)
	}
	for _, f := range files {
		blob, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s mkvSummary
		s.blocks = map[byte]int{}
		ebmlWalk(t, blob, &s, "")
		if got := s.blocks[mkvVideoTrack]; got != 2 {
			t.Errorf("%s: кадрів %d, want 2", filepath.Base(f), got)
		}
	}
}

// TestRecordRotatedFileStartsAtZero — файл, відкритий ротацією на зміні SPS,
// має починатись з мітки ~0, а не з «часу від початку сесії»: інакше плеєр
// показує порожній старт і хибну тривалість. Між файлами тут 3 с потоку.
func TestRecordRotatedFileStartsAtZero(t *testing.T) {
	dir := withRecordFlag(t, true)
	aus := corpusAUs(t)
	key := aus[0].Data

	var sps []byte
	for _, n := range h264.SplitNALs(key) {
		if n[0]&0x1F == h264.NALSPS {
			sps = n
		}
	}
	other := append([]byte(nil), sps...)
	other[3]++
	key2 := bytes.Replace(key, sps, other, 1)

	rec := startRecording("node-sps0", currentRecordCfg())
	var seq uint16
	var ts uint32
	for _, au := range [][]byte{key, key, key, key2, key2} {
		for _, p := range packetizeAU(au, ts, &seq) {
			rec.offer(p)
		}
		ts += 90000 // 1 с на кадр
	}
	rec.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.mkv"))
	if len(files) != 2 {
		t.Fatalf("файлів запису %d, want 2: %v", len(files), files)
	}
	for _, f := range files {
		blob, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s mkvSummary
		s.blocks = map[byte]int{}
		ebmlWalk(t, blob, &s, "")
		if len(s.clusterTS) == 0 {
			t.Fatalf("%s: жодного кластера", filepath.Base(f))
		}
		if s.clusterTS[0] != 0 {
			t.Errorf("%s: перший кластер з мітки %d мс, want 0", filepath.Base(f), s.clusterTS[0])
		}
	}
}

// Нода живе за налаштуваннями, з якими зʼявилась у реєстрі, а не за
// глобалами на момент спрацювання таймера. Саме так тест, що повертає глобали
// в Cleanup, і доживаюча pion-горутина ноги попереднього тесту
// (dropViewer → scheduleRecordClose) перестали ділити змінну (R3-G6 ⚪3):
// читати глобали горутині ноди більше нема чого. Поверни в scheduleRecordClose
// читання recordEnabled/recordIdleClose — файл тут не закриється.
func TestRecordCloseUsesSessionSnapshot(t *testing.T) {
	withRecordFlag(t, true)
	prev := recordIdleClose
	recordIdleClose = 20 * time.Millisecond
	ns := newRegistry().getOrCreate("snap")      // знімок налаштувань — тут
	recordEnabled, recordIdleClose = false, prev // «наступний тест» переписав глобали

	vl := silentViewer(t, ns)
	ns.viewerCount.Store(1)
	fin := make(chan struct{})
	close(fin) // писаря нема: Close не чекає на нього
	ns.rec.Store(&recorder{nodeID: "snap", done: make(chan struct{}), fin: fin})
	removeViewer(ns, vl)

	deadline := time.Now().Add(eventGuard)
	for ns.rec.Load() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ns.rec.Load() != nil {
		t.Fatal("файл не закрито: таймер закриття читав глобали, а не знімок ноди")
	}
}
