package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/h264"
	"github.com/pion/rtp"
)

// Два закони цілісності запису, обидва знайдені на ЖИВОМУ файлі з прода
// (ffmpeg на ньому лаявся, а суворий плеєр не стартував):
//
//  1. файл не сміє ПОЧИНАТИСЬ із рваного кадру — того, у якому загубився
//     фрагмент. Він виглядає ключовим, але зрізи неповні;
//  2. мітки часу у файлі мусять лише зростати — RTP не гарантує порядку.

// recordedFile повертає єдиний .mkv із теки запису (або "" — якщо файлу нема).
func recordedFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".mkv" {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// Рваний ключовий кадр не має права стати першим у файлі: писар мусить
// дочекатись наступного цілого.
func TestRecordSkipsTornFirstKeyframe(t *testing.T) {
	dir := withRecordFlag(t, true)
	aus := corpusAUs(t)

	rec := startRecording("node-torn")
	if rec == nil {
		t.Fatal("під прапорцем startRecording віддав nil")
	}

	var seq uint16
	var ts uint32

	// Перший AU — ХВІСТ кадру: NAL-тип каже «ключовий», але зріз починається
	// не з початку (first_mb_in_slice != 0). Саме це прийшло з прода, коли
	// хаб підключився посеред кадру: декодеру нема з чого стартувати, і він
	// каже «A non-intra slice in an IDR NAL unit».
	//
	// Моделюємо за ВМІСТОМ, а не викиданням пакетів: якщо перший зріз кадру
	// вцілів, AU придатний, і писар має повне право його взяти — тест на
	// викидання перевіряв би не той закон.
	tail := tailOnlyIDR(t, aus[0].Data)
	for _, p := range packetizeAU(tail, ts, &seq) {
		rec.offer(p)
	}

	// Другий AU — цілий.
	ts += 9000
	for _, p := range packetizeAU(aus[0].Data, ts, &seq) {
		rec.offer(p)
	}
	ts += 9000
	for _, p := range packetizeAU(aus[len(aus)-1].Data, ts, &seq) {
		rec.offer(p)
	}
	rec.Close()

	path := recordedFile(t, dir)
	if path == "" {
		t.Fatal("файл не створився зовсім — писар мав почати з ЦІЛОГО кадру, а не здатись")
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var s mkvSummary
	s.blocks = map[byte]int{}
	ebmlWalk(t, blob, &s, "")

	// Рваний AU не записаний: у файлі менше кадрів, ніж ми надіслали AU.
	if got := s.blocks[mkvVideoTrack]; got != 2 {
		t.Errorf("кадрів у файлі %d, want 2 (рваний перший мав бути пропущений)", got)
	}
}

// Кадр, що спізнився (мітка менша за вже записану), у файл не потрапляє.
func TestRecordDropsOutOfOrderFrame(t *testing.T) {
	dir := withRecordFlag(t, true)
	aus := corpusAUs(t)

	rec := startRecording("node-order")
	if rec == nil {
		t.Fatal("startRecording віддав nil")
	}

	var seq uint16
	base := uint32(90000)

	// Три кадри: 0 -> +9000 -> і третій зі СТАРОЮ міткою (переупорядкування).
	for _, ts := range []uint32{base, base + 9000} {
		for _, p := range packetizeAU(aus[0].Data, ts, &seq) {
			rec.offer(p)
		}
	}
	for _, p := range packetizeAU(aus[0].Data, base+4500, &seq) { // спізнівся
		rec.offer(p)
	}
	// Ще один свіжий, щоб останній AU точно вилився у файл.
	for _, p := range packetizeAU(aus[0].Data, base+18000, &seq) {
		rec.offer(p)
	}
	rec.Close()

	path := recordedFile(t, dir)
	if path == "" {
		t.Fatal("файл не створився")
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var s mkvSummary
	s.blocks = map[byte]int{}
	ebmlWalk(t, blob, &s, "")

	if got := s.blocks[mkvVideoTrack]; got != 3 {
		t.Errorf("кадрів у файлі %d, want 3 (спізнілий мав бути відкинутий)", got)
	}
	if rec.droppedLate != 1 {
		t.Errorf("лічильник спізнілих = %d, want 1", rec.droppedLate)
	}
}

// Дзеркальна половина: нормальний порядок НЕ страждає. Без цієї перевірки
// «фікс» можна було б зробити, відкинувши геть усе.
func TestRecordKeepsMonotonicFrames(t *testing.T) {
	dir := withRecordFlag(t, true)
	aus := corpusAUs(t)

	rec := startRecording("node-mono")
	if rec == nil {
		t.Fatal("startRecording віддав nil")
	}
	var seq uint16
	ts := uint32(90000)
	const want = 5
	for i := 0; i < want; i++ {
		for _, p := range packetizeAU(aus[i%len(aus)].Data, ts, &seq) {
			rec.offer(p)
		}
		ts += 9000
	}
	// Останній AU виллється лише на зміні мітки або на Close.
	rec.Close()
	time.Sleep(10 * time.Millisecond)

	path := recordedFile(t, dir)
	if path == "" {
		t.Fatal("файл не створився")
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var s mkvSummary
	s.blocks = map[byte]int{}
	ebmlWalk(t, blob, &s, "")

	if got := s.blocks[mkvVideoTrack]; got != want {
		t.Errorf("кадрів %d, want %d — монотонний потік не сміє втрачати кадри", got, want)
	}
	if rec.droppedLate != 0 {
		t.Errorf("на монотонному потоці відкинуто %d кадрів — жодного не мало бути", rec.droppedLate)
	}
}

var _ = rtp.Packet{}

// tailOnlyIDR робить із цілого AU «хвіст кадру»: лишає SPS/PPS (без них файл
// не почнеться з іншої причини) і псує first_mb_in_slice кожного IDR-зрізу,
// щоб жоден не заявляв початок кадру.
func tailOnlyIDR(t *testing.T, au []byte) []byte {
	t.Helper()
	nals := h264.SplitNALs(au)
	if len(nals) == 0 {
		t.Fatal("корпусний AU порожній")
	}
	var out []byte
	touched := false
	for _, n := range nals {
		n = append([]byte(nil), n...)
		if n[0]&0x1F == h264.NALIDR && len(n) > 1 {
			n[1] &^= 0x80 // ue(v) вже не нуль -> зріз не з початку кадру
			touched = true
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	if !touched {
		t.Skip("у корпусному AU немає IDR — псувати нічого")
	}
	return out
}
