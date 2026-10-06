package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// mkRec кладе у теку файл запису заданого віку й розміру.
func mkRec(t *testing.T, dir, name string, age time.Duration, size int64, now time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatalf("не створив %s: %v", name, err)
	}
	mod := now.Add(-age)
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatalf("не проставив час %s: %v", name, err)
	}
	return p
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Записи, старші за recordMaxAge, зникають; свіжі лишаються.
func TestPruneByAge(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	old := mkRec(t, dir, "old.mkv", recordMaxAge+time.Hour, 1024, now)
	fresh := mkRec(t, dir, "fresh.mkv", time.Hour, 1024, now)

	pruneRecordings(dir, now, recordMaxBytes)

	if exists(old) {
		t.Error("запис, старший за граничний вік, лишився")
	}
	if !exists(fresh) {
		t.Error("свіжий запис прибрали — прибирання зачепило зайве")
	}
}

// Рівно на межі віку файл ЛИШАЄТЬСЯ: правило «старші за», а не «старші або
// рівні». Без цієї перевірки помилка на одиницю жила б непоміченою.
func TestPruneAgeBoundaryKeeps(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	edge := mkRec(t, dir, "edge.mkv", recordMaxAge-time.Minute, 1024, now)
	pruneRecordings(dir, now, recordMaxBytes)

	if !exists(edge) {
		t.Error("файл на межі віку прибрали, хоча він ще не застарів")
	}
}

// Той самий закон обсягу, але на дрібних числах: файли справжні, ліміт
// дрібний, переданий параметром.
func TestPruneBySizeSmallLimit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	const limit = 2500 // трохи більше за два файли по 1000

	oldest := mkRec(t, dir, "a.mkv", 3*time.Hour, 1000, now)
	middle := mkRec(t, dir, "b.mkv", 2*time.Hour, 1000, now)
	newest := mkRec(t, dir, "c.mkv", time.Hour, 1000, now)

	pruneRecordings(dir, now, limit)

	if exists(oldest) {
		t.Error("найстаріший запис лишився, хоча архів перевищував стелю")
	}
	if !exists(middle) || !exists(newest) {
		t.Errorf("зрізали зайве: middle=%v newest=%v", exists(middle), exists(newest))
	}
}

// Прибирання не чіпає нічого, крім .mkv: у тій самій теці можуть лежати чужі
// файли, і зжерти їх — гірше, ніж не прибрати свої.
func TestPruneTouchesOnlyMkv(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	foreign := mkRec(t, dir, "important.db", recordMaxAge+time.Hour, 1024, now)
	mine := mkRec(t, dir, "mine.mkv", recordMaxAge+time.Hour, 1024, now)

	pruneRecordings(dir, now, recordMaxBytes)

	if !exists(foreign) {
		t.Fatal("прибирання видалило ЧУЖИЙ файл — це вихід за межі своєї теки")
	}
	if exists(mine) {
		t.Error("свій застарілий .mkv лишився")
	}
}

// Відсутня тека — нормальний стан до першого запису, не помилка й не паніка.
func TestPruneMissingDirIsQuiet(t *testing.T) {
	pruneRecordings(filepath.Join(t.TempDir(), "ще-нема"), time.Now(), recordMaxBytes)
}

// recordingsFit на порожній теці дозволяє писати (на цій машині вільного
// місця свідомо більше за поріг — інакше тест і не мав би сенсу).
func TestRecordingsFitAllowsOnEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if free, ok := diskFreeBytes(dir); ok && free < recordMinFree {
		t.Skipf("на цій машині вільно %.1f ГБ — менше за поріг, перевіряти нічого", float64(free)/(1<<30))
	}
	if !recordingsFit(dir) {
		t.Error("відмовився писати на порожній теці з достатнім місцем")
	}
}

// TestRecordingsFitMeasuresFirstRecordingDir — R6-G6: на першому записі теки
// recordings ще нема. recordingsFit мусить створити її ДО виміру, інакше
// Statfs (diskfree_unix.go) падає і запобіжник місця мовчки пропускається.
// Зніми MkdirAll у recordingsFit — теки після виклику не буде.
func TestRecordingsFitMeasuresFirstRecordingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recordings")
	recordingsFit(dir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("тека першого запису не створена до виміру місця: %v", err)
	}
	if _, ok := diskFreeBytes(dir); !ok && runtime.GOOS == "linux" {
		t.Fatal("на Linux вільне місце теки першого запису не виміряно")
	}
}

func TestPruneRemovesManyOverLimit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	const limit = 3000

	var paths []string
	for i := 0; i < 10; i++ {
		paths = append(paths, mkRec(t, dir, fmt.Sprintf("r%02d.mkv", i), time.Duration(10-i)*time.Hour, 1000, now))
	}

	pruneRecordings(dir, now, limit)

	var left int
	for _, p := range paths {
		if exists(p) {
			left++
		}
	}
	if left != 3 {
		t.Errorf("під стелю 3000 Б (по 1000 Б на файл) мало лишитись 3, лишилось %d", left)
	}
}
