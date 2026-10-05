package reccrypt

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func key(t testing.TB) []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTripAndTamper(t *testing.T) {
	k := key(t)
	var file bytes.Buffer
	w, err := NewWriter(&file, k)
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	for i := 0; i < 50; i++ {
		chunk := bytes.Repeat([]byte{byte(i)}, 1000+i*37)
		plain.Write(chunk)
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	enc := file.Bytes()
	if bytes.Contains(enc, bytes.Repeat([]byte{7}, 64)) {
		t.Fatal("відкритий текст видно у файлі")
	}
	var out bytes.Buffer
	if _, err := Decrypt(&out, bytes.NewReader(enc), k); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), plain.Bytes()) {
		t.Fatal("round-trip не збігся")
	}

	// обрізаний хвіст: усе до нього читається, помилка — ErrTruncated
	out.Reset()
	_, err = Decrypt(&out, bytes.NewReader(enc[:len(enc)-100]), k)
	if !errors.Is(err, ErrTruncated) || out.Len() == 0 || !bytes.HasPrefix(plain.Bytes(), out.Bytes()) {
		t.Fatalf("truncated: err=%v got=%d", err, out.Len())
	}

	// зіпсований байт посередині
	bad := append([]byte(nil), enc...)
	bad[len(bad)/2] ^= 1
	if _, err := Decrypt(&bytes.Buffer{}, bytes.NewReader(bad), k); err == nil {
		t.Fatal("зіпсований файл розшифрувався")
	}
	// інший ключ
	if _, err := Decrypt(&bytes.Buffer{}, bytes.NewReader(enc), key(t)); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong key: %v", err)
	}
	// не наш файл
	if _, err := Decrypt(&bytes.Buffer{}, bytes.NewReader([]byte("\x1aE\xdf\xa3 matroska....")), k); !errors.Is(err, ErrNotEnc) {
		t.Fatalf("plain mkv: %v", err)
	}
}

func TestRecordSwapRejected(t *testing.T) {
	k := key(t)
	var f bytes.Buffer
	w, _ := NewWriter(&f, k)
	w.Write([]byte("AAAA"))
	w.Write([]byte("BBBB"))
	enc := f.Bytes()
	recLen := (len(enc) - HeaderSize) / 2
	sw := append([]byte(nil), enc[:HeaderSize]...)
	sw = append(sw, enc[HeaderSize+recLen:]...)
	sw = append(sw, enc[HeaderSize:HeaderSize+recLen]...)
	if _, err := Decrypt(&bytes.Buffer{}, bytes.NewReader(sw), k); err == nil {
		t.Fatal("переставлені записи розшифрувались")
	}
}

func TestLoadKey(t *testing.T) {
	k := key(t)
	if got, err := LoadKey(hex.EncodeToString(k), ""); err != nil || !bytes.Equal(got, k) {
		t.Fatal("hex", err)
	}
	if got, err := LoadKey("", ""); got != nil || err != nil {
		t.Fatal("порожньо = вимкнено")
	}
	if _, err := LoadKey("short", ""); err == nil {
		t.Fatal("короткий ключ прийнято")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	os.WriteFile(p, k, 0o600)
	if got, err := LoadKey("ignored", p); err != nil || !bytes.Equal(got, k) {
		t.Fatal("file raw", err)
	}
	os.Chmod(p, 0o644)
	if _, err := LoadKey("", p); err == nil {
		t.Fatal("файл ключа 0644 прийнято")
	}
}

// BenchmarkWrite — ціна шифрування на типовому кластері (~1 с відео при
// 2 Мбіт/с ≈ 256 КБ).
func BenchmarkWrite256K(b *testing.B) {
	k := key(b)
	w, _ := NewWriter(discard{}, k)
	p := make([]byte, 256<<10)
	b.SetBytes(int64(len(p)))
	for b.Loop() {
		w.Write(p)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
