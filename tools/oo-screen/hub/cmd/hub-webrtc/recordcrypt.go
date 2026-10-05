package main

// S5: шифрування записів на диску і настроювана політика зберігання.
//
// 🔴 ТИПОВО ВИМКНЕНО. Без OO_SCREEN_RECORD_KEY / OO_SCREEN_RECORD_KEY_FILE хаб
// пише рівно той самий відкритий .mkv, що й раніше. З ключем — .mkv.enc
// (internal/reccrypt, AES-256-GCM, запис = один кластер MKV), розшифровка —
// `go run ./hub/cmd/oo-rec-decrypt -key-file K in.mkv.enc > out.mkv`.
//
// ЗАДАНО, АЛЕ БИТЕ = НЕ ПИШЕМО. Якщо адмін просив шифрування, а ключ не
// розібрався (короткий, файл 0644, файла нема), відкритий текст на диск НЕ
// лягає: startRecording віддає nil, причина — в журналі на старті й на
// кожній сесії (раз на хвилину).
//
// Зберігання: OO_SCREEN_RECORD_MAX_AGE (Go duration або "Nd", типово 14d) і
// OO_SCREEN_RECORD_MAX_BYTES (байти або з суфіксом K/M/G, типово 8G). Битий
// рядок -> лог і дефолт (дефолт і так безпечний).

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/organicoils/oo-screen/internal/reccrypt"
)

// recordKey — nil = шифрування вимкнене.
var recordKey atomic.Pointer[[]byte]

// recordKeyBroken — ключ задано, але він непридатний: запис заборонено.
var recordKeyBroken atomic.Bool

var recordKeyWarn = rate.NewLimiter(rate.Every(time.Minute), 1)

func init() {
	k, err := reccrypt.LoadKey(os.Getenv("OO_SCREEN_RECORD_KEY"), os.Getenv("OO_SCREEN_RECORD_KEY_FILE"))
	switch {
	case err != nil:
		recordKeyBroken.Store(true)
		log.Printf("record: ключ шифрування непридатний (%v) — запис ВИМКНЕНО, відкритим текстом не пишу", err)
	case k != nil:
		recordKey.Store(&k)
	}
	if v := os.Getenv("OO_SCREEN_RECORD_MAX_AGE"); v != "" {
		if d, ok := parseAge(v); ok {
			recordMaxAgeVar.Store(int64(d))
		} else {
			log.Printf("record: OO_SCREEN_RECORD_MAX_AGE=%q не розібрався — лишаю %v", v, recordMaxAge)
		}
	}
	if v := os.Getenv("OO_SCREEN_RECORD_MAX_BYTES"); v != "" {
		if n, ok := parseBytes(v); ok {
			recordMaxBytesVar = n
		} else {
			log.Printf("record: OO_SCREEN_RECORD_MAX_BYTES=%q не розібрався — лишаю %d", v, recordMaxBytesVar)
		}
	}
}

// recordAllowedByKey — false, якщо шифрування просили, але ключ битий.
func recordAllowedByKey() bool {
	if recordKeyBroken.Load() {
		if recordKeyWarn.Allow() {
			log.Printf("record: сесію НЕ пишу — ключ шифрування заданий, але непридатний")
		}
		return false
	}
	return true
}

// currentRecordKey — ключ або nil.
func currentRecordKey() []byte {
	if p := recordKey.Load(); p != nil {
		return *p
	}
	return nil
}

// parseAge: "14d", "36h", "90m". Мінімум 1 год — захист від "1s", що стер би
// архів одразу.
func parseAge(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, false
		}
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, false
		}
	}
	if d < time.Hour {
		return 0, false
	}
	return d, true
}

// parseBytes: "8G", "500M", "1073741824". Мінімум 1 МБ.
func parseBytes(s string) (int64, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "B")
	mul := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mul, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mul, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mul, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "T"):
		mul, s = 1<<40, strings.TrimSuffix(s, "T")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mul || n*mul < 1<<20 {
		return 0, false
	}
	return n * mul, true
}

// isRecordingFile — що вважається записом для прибирання.
func isRecordingFile(name string) bool {
	return strings.HasSuffix(name, ".mkv") || strings.HasSuffix(name, reccrypt.Ext)
}
