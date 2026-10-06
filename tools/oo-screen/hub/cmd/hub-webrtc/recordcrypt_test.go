package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/reccrypt"
)

func withRecordKey(t *testing.T, key []byte, broken bool) {
	t.Helper()
	prev, prevB := recordKey.Load(), recordKeyBroken.Load()
	if key == nil {
		recordKey.Store(nil)
	} else {
		recordKey.Store(&key)
	}
	recordKeyBroken.Store(broken)
	t.Cleanup(func() { recordKey.Store(prev); recordKeyBroken.Store(prevB) })
}

// Без ключа — типово вимкнено (захист від перевернутого дефолту).
func TestRecordEncryptionDefaultOff(t *testing.T) {
	if os.Getenv("OO_SCREEN_RECORD_KEY") == "" && os.Getenv("OO_SCREEN_RECORD_KEY_FILE") == "" {
		if currentRecordKey() != nil || recordKeyBroken.Load() {
			t.Fatal("без змінних оточення шифрування мусить бути вимкнене")
		}
	}
}

func TestRecordEncryptedRoundTrip(t *testing.T) {
	dir := withRecordFlag(t, true)
	key := make([]byte, 32)
	rand.Read(key)
	withRecordKey(t, key, false)
	withAudioFlag(t, true) // ffprobeMKV чекає обидві доріжки

	aus := corpusAUs(t)
	rec := startRecording("pc1", currentRecordCfg())
	if rec == nil {
		t.Fatal("startRecording nil")
	}
	var seq uint16
	var ts uint32
	for _, au := range aus {
		for _, p := range packetizeAU(au.Data, ts, &seq) {
			rec.offer(p)
		}
		ts += 9000
	}
	rec.Close()

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), reccrypt.Ext) {
		t.Fatalf("файли: %v, want один *%s", entries, reccrypt.Ext)
	}
	path := filepath.Join(dir, entries[0].Name())
	enc, _ := os.ReadFile(path)
	if !bytes.HasPrefix(enc, []byte(reccrypt.Magic)) || bytes.Contains(enc, []byte("matroska")) {
		t.Fatal("файл не зашифровано (видно EBML DocType)")
	}
	var plain bytes.Buffer
	if _, err := reccrypt.Decrypt(&plain, bytes.NewReader(enc), key); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var s mkvSummary
	s.blocks = map[byte]int{}
	ebmlWalk(t, plain.Bytes(), &s, "")
	if s.blocks[mkvVideoTrack] != len(aus) || s.width != 1920 {
		t.Fatalf("розшифрований MKV: %d кадрів (want %d), %dx%d", s.blocks[mkvVideoTrack], len(aus), s.width, s.height)
	}
	t.Logf("encrypted %d B -> plain %d B (overhead %.3f%%)", len(enc), plain.Len(),
		100*float64(len(enc)-plain.Len())/float64(plain.Len()))

	dec := filepath.Join(t.TempDir(), "dec.mkv")
	os.WriteFile(dec, plain.Bytes(), 0o600)
	ffprobeMKV(t, dec)

	// Прибирання бачить і .mkv.enc.
	old := time.Now().Add(-recordMaxAge - time.Hour)
	os.Chtimes(path, old, old)
	pruneRecordings(dir, time.Now(), recordMaxBytes)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("старий .mkv.enc не прибрано")
	}
}

// Ключ задано, але битий — НІЧОГО не пишемо (а не відкритий текст).
func TestRecordBrokenKeyRefuses(t *testing.T) {
	dir := withRecordFlag(t, true)
	withRecordKey(t, nil, true)
	if rec := startRecording("pc1", currentRecordCfg()); rec != nil {
		rec.Close()
		t.Fatal("з битим ключем запис стартував")
	}
	if e, _ := os.ReadDir(dir); len(e) != 0 {
		t.Fatalf("на диску щось є: %v", e)
	}
}

func TestRecordRetentionParse(t *testing.T) {
	for in, want := range map[string]time.Duration{"14d": 14 * 24 * time.Hour, "36h": 36 * time.Hour, "0.5d": 12 * time.Hour} {
		if d, ok := parseAge(in); !ok || d != want {
			t.Errorf("parseAge(%q)=%v,%v", in, d, ok)
		}
	}
	for _, bad := range []string{"", "1s", "x", "-3d", "30m"} {
		if _, ok := parseAge(bad); ok {
			t.Errorf("parseAge(%q) прийнято", bad)
		}
	}
	for in, want := range map[string]int64{"8G": 8 << 30, "500M": 500 << 20, "2gb": 2 << 30, "1048576": 1 << 20} {
		if n, ok := parseBytes(in); !ok || n != want {
			t.Errorf("parseBytes(%q)=%v,%v", in, n, ok)
		}
	}
	for _, bad := range []string{"", "1K", "-1G", "G", "99999999999T"} {
		if _, ok := parseBytes(bad); ok {
			t.Errorf("parseBytes(%q) прийнято", bad)
		}
	}
}

func TestRecordMaxAgeVarApplied(t *testing.T) {
	dir := t.TempDir()
	prev := recordMaxAgeVar.Load()
	recordMaxAgeVar.Store(int64(2 * time.Hour))
	t.Cleanup(func() { recordMaxAgeVar.Store(prev) })
	now := time.Now()
	a := mkRec(t, dir, "a.mkv", 3*time.Hour, 10, now)
	b := mkRec(t, dir, "b.mkv.enc", 3*time.Hour, 10, now)
	c := mkRec(t, dir, "c.mkv", time.Hour, 10, now)
	pruneRecordings(dir, now, recordMaxBytes)
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s мав зникнути за віком 2h", p)
		}
	}
	if _, err := os.Stat(c); err != nil {
		t.Fatal("свіжий зник")
	}
}
