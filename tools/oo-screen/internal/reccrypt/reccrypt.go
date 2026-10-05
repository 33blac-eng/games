// Package reccrypt — шифрування записів сесій на диску (S5), AES-256-GCM.
//
// Формат файла (.mkv.enc) — потоковий, придатний до читання на будь-якому
// цілому записі, як і сам MKV (mkv.go пише цілими кластерами):
//
//	header  = magic "OOSCREC1" (8) | nonce prefix (4, crypto/rand) | key id (8)
//	record* = len (4, BE, довжина ciphertext+tag) | GCM.Seal(...)
//
// nonce запису = prefix(4) | counter(8, BE), counter від 0. AAD = header |
// counter: переставити, продублювати чи пересадити запис з іншого файла не
// можна — розшифровка впаде. Обрізаний ХВІСТ (процес убили посеред write)
// виявляється як ErrTruncated; усе до нього розшифровується.
//
// key id = перші 8 байтів SHA-256(key) — лише щоб сказати «не той ключ»
// замість безликого «authentication failed». Ключ він не розкриває.
package reccrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Magic — перші байти зашифрованого запису.
const Magic = "OOSCREC1"

// Ext — суфікс зашифрованих файлів.
const Ext = ".mkv.enc"

// HeaderSize — розмір заголовка файла.
const HeaderSize = 8 + 4 + 8

// MaxRecord — стеля одного запису (захист читача від битого len).
const MaxRecord = 64 << 20

var (
	ErrTruncated = errors.New("reccrypt: файл обрізано посеред запису")
	ErrWrongKey  = errors.New("reccrypt: файл зашифровано іншим ключем")
	ErrNotEnc    = errors.New("reccrypt: це не зашифрований запис oo-screen")
)

// KeyID — відбиток ключа.
func KeyID(key []byte) [8]byte {
	s := sha256.Sum256(key)
	var id [8]byte
	copy(id[:], s[:8])
	return id
}

// ParseKey приймає 32 байти як 64 hex-символи або base64 (std/raw), з
// пробілами/переводами рядка по краях. Інше — помилка (а не «якось обріжемо»).
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("reccrypt: ключ має бути 32 байти (64 hex або base64)")
}

// LoadKey: file має пріоритет над value; обидва порожні — (nil, nil) =
// шифрування вимкнене. Файл ключа з правами ширшими за 0600 — помилка.
func LoadKey(value, file string) ([]byte, error) {
	if file != "" {
		fi, err := os.Stat(file)
		if err != nil {
			return nil, err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("reccrypt: файл ключа %s доступний не лише власнику (%v)", file, fi.Mode().Perm())
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if len(b) == 32 {
			return b, nil // сирі байти
		}
		return ParseKey(string(b))
	}
	if value == "" {
		return nil, nil
	}
	return ParseKey(value)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("reccrypt: потрібен 32-байтовий ключ (AES-256)")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// Writer шифрує кожен Write окремим записом.
type Writer struct {
	w      io.Writer
	aead   cipher.AEAD
	header [HeaderSize]byte
	ctr    uint64
	buf    []byte
}

// NewWriter пише заголовок і повертає Writer.
func NewWriter(w io.Writer, key []byte) (*Writer, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	ew := &Writer{w: w, aead: aead}
	copy(ew.header[:8], Magic)
	if _, err := rand.Read(ew.header[8:12]); err != nil {
		return nil, err
	}
	id := KeyID(key)
	copy(ew.header[12:], id[:])
	if _, err := w.Write(ew.header[:]); err != nil {
		return nil, err
	}
	return ew, nil
}

func nonceAAD(header []byte, ctr uint64) (nonce [12]byte, aad []byte) {
	copy(nonce[:4], header[8:12])
	binary.BigEndian.PutUint64(nonce[4:], ctr)
	aad = make([]byte, 0, HeaderSize+8)
	aad = append(aad, header...)
	aad = binary.BigEndian.AppendUint64(aad, ctr)
	return
}

// Write — один запис. Повертає len(p) при успіху (контракт io.Writer).
func (e *Writer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(p)+e.aead.Overhead() > MaxRecord {
		return 0, errors.New("reccrypt: запис завеликий")
	}
	nonce, aad := nonceAAD(e.header[:], e.ctr)
	e.buf = binary.BigEndian.AppendUint32(e.buf[:0], uint32(len(p)+e.aead.Overhead()))
	e.buf = e.aead.Seal(e.buf, nonce[:], p, aad)
	if _, err := e.w.Write(e.buf); err != nil {
		return 0, err
	}
	e.ctr++
	return len(p), nil
}

// Decrypt розшифровує потік r у w. Повертає кількість розшифрованих байтів і
// ErrTruncated, якщо хвіст обрізано (все до нього вже в w).
func Decrypt(dst io.Writer, r io.Reader, key []byte) (int64, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return 0, err
	}
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, ErrNotEnc
	}
	if string(hdr[:8]) != Magic {
		return 0, ErrNotEnc
	}
	if id := KeyID(key); string(id[:]) != string(hdr[12:]) {
		return 0, ErrWrongKey
	}
	var n int64
	var lenb [4]byte
	var buf, out []byte
	for ctr := uint64(0); ; ctr++ {
		k, err := io.ReadFull(r, lenb[:])
		if err == io.EOF {
			return n, nil
		}
		if err != nil || k != 4 {
			return n, ErrTruncated
		}
		l := binary.BigEndian.Uint32(lenb[:])
		if l < uint32(aead.Overhead()) || l > MaxRecord {
			return n, fmt.Errorf("reccrypt: битий запис #%d (len=%d)", ctr, l)
		}
		if cap(buf) < int(l) {
			buf = make([]byte, l)
		}
		buf = buf[:l]
		if _, err := io.ReadFull(r, buf); err != nil {
			return n, ErrTruncated
		}
		nonce, aad := nonceAAD(hdr[:], ctr)
		out, err = aead.Open(out[:0], nonce[:], buf, aad)
		if err != nil {
			return n, fmt.Errorf("reccrypt: запис #%d не пройшов автентифікацію: %w", ctr, err)
		}
		w, err := dst.Write(out)
		n += int64(w)
		if err != nil {
			return n, err
		}
	}
}
