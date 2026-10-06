//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Агент живе добами: лог мусить ротуватись НА ХОДУ, а не лише на старті.
// Прибери перевірку розміру з rotatingLog.Write — файл росте без стелі.
func TestLogRotatesWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oo-agent.log")
	l, err := openRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := strings.Repeat("x", 39) + "\n" // 40 байт
	for i := 0; i < 7; i++ {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	cur, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Size() > 100 {
		t.Fatalf("поточний лог %d байт при стелі 100 — ротації на ходу нема", cur.Size())
	}
	old, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("відкладеної генерації .1 нема: %v", err)
	}
	// 7 рядків по 40 при стелі 100: ротація перед 3-м, 5-м і 7-м — у .1 два
	// рядки, у поточному один. Жодного рядка, розрізаного між файлами.
	if old.Size() != 80 || cur.Size() != 40 {
		t.Fatalf(".1=%d поточний=%d, want 80/40", old.Size(), cur.Size())
	}
}

// Ротація, що не вдалась (поточний файл тримає інший процес без
// FILE_SHARE_DELETE), не сміє стерти вже відкладений .1 — саме там історія,
// потрібна, щоб розібратись. І не повторюється на кожному рядку: лог пишеться
// далі в той самий файл.
func TestLogFailedRotationKeepsOldGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oo-agent.log")
	if err := os.WriteFile(path+".1", []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := openRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	hold, err := os.Open(path) // Go на Windows відкриває без FILE_SHARE_DELETE
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()

	line := strings.Repeat("x", 39) + "\n"
	for i := 0; i < 7; i++ {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if b, err := os.ReadFile(path + ".1"); err != nil || string(b) != "OLD" {
		t.Fatalf(".1 = %q, %v — відкладена генерація зникла на невдалій ротації", b, err)
	}
	if st, _ := os.Stat(path); st == nil || st.Size() != 7*40 {
		t.Fatalf("поточний лог не дописувався далі: %+v", st)
	}
	if l.retryAt.IsZero() {
		t.Error("невдала ротація не відклала повтор — кожен рядок пробуватиме знову")
	}
}
