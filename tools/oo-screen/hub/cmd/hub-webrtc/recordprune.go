package main

// Запобіжник місця для запису сесій (record.go).
//
// 🔴 Навіщо: цей самий VPS уже одного разу став колом через диск, забитий під
// 100% — і поклало це не того, хто його забив, а всі служби поруч. Запис
// екрана — саме той тип навантаження, що росте тихо: жодна окрема сесія не
// виглядає великою, а через місяць їх триста.
//
// Три рубежі, від найдешевшого до найгрубішого:
//  1. вік — старші за recordMaxAge зникають;
//  2. обсяг — усе, що понад recordMaxBytesVar, зрізається з найстарішого;
//  3. вільне місце — якщо після (1) і (2) на розділі лишилось менше за
//     recordMinFree, НОВИЙ запис просто не починається.
//
// Порядок навмисний: коли місце кінчається посеред дня, дешевше втратити
// найстаріший запис (його ніхто не дивиться), ніж поточну сесію, заради якої
// адміна щойно покликали. Але якщо різати вже нічого — ми відмовляємось
// писати, а не з'їдаємо залишок диска.

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

const (
	// recordMaxAge — скільки живуть записи. Два тижні: більше за будь-який
	// реальний привід повернутись до сеансу допомоги («що ти там натиснув
	// минулого понеділка»), і менше за строк, на якому архів стає окремою
	// сутністю, яку треба адмініструвати.
	recordMaxAge = 14 * 24 * time.Hour

	// Стеля всього архіву (recordMaxBytesVar нижче) — 8 ГБ проти 21 ГБ вільних на
	// момент увімкнення: беремо помітно менше за вільне, щоб ліміт спрацював
	// РАНІШЕ, ніж диск стане проблемою для сусідніх служб.
	//
	// Порядок величин заміряний не нами: година статичного офісного екрана в
	// H.264 без перекодування — сотні мегабайтів, не гігабайти. 8 ГБ — це
	// приблизно 20 годин запису, тобто тижні реального ужитку при 3-5
	// сеансах на день.

	// recordMinFree — нижче цього новий запис не починається взагалі.
	// Не нуль і не «скільки треба цьому файлу»: 5 ГБ лишаються сусідам
	// (база, логи, оновлення), бо диск спільний.
	recordMinFree uint64 = 5 << 30
)

// recordMaxBytesVar — стеля всього архіву, змінна (не const) з тієї ж
// причини, що й прапорці фіч: тест підміняє її дрібним числом і перевіряє
// закон на справжніх файлах, замість писати на диск вісім гігабайтів.
var recordMaxBytesVar int64 = 8 << 30

// recordMaxAgeVar — фактична межа віку (S5: OO_SCREEN_RECORD_MAX_AGE), типово
// recordMaxAge. Атомарна: читає фонова горутина прибирання.
var recordMaxAgeVar atomic.Int64

func init() { recordMaxAgeVar.Store(int64(recordMaxAge)) }

// recordingsFit прибирає застаріле й зайве, а тоді каже, чи можна починати
// новий запис. false = місця немає; викликач мусить не починати.
//
// Помилки читання теки НЕ фатальні для прибирання, але фатальні для дозволу:
// не зміг подивитись — не пиши. «Не виміряв» не те саме, що «вільно».
func recordingsFit(dir string) bool {
	// H-21: прунінг ходить по диску (stat кожного файлу) — не в OnTrack, де
	// він затримував перший кадр. Місце перевіряємо одразу, чистимо у фоні.
	go pruneRecordings(dir, time.Now())

	free, ok := diskFreeBytes(dir)
	if !ok {
		// Платформа, де ми не вміємо питати про вільне місце (див.
		// diskfree_*.go). Прибирання за віком і обсягом уже відпрацювало —
		// цього досить, щоб архів не ріс безмежно, а решту диска стережуть
		// інші. Пишемо.
		return true
	}
	if free < recordMinFree {
		log.Printf("record: НЕ починаю запис — на розділі %s вільно %.1f ГБ, поріг %.1f ГБ",
			dir, float64(free)/(1<<30), float64(recordMinFree)/(1<<30))
		return false
	}
	return true
}

// pruneRecordings видаляє записи, старші за recordMaxAge, а потім — найстаріші
// з тих, що лишились, поки сумарний обсяг не влізе в recordMaxBytesVar.
//
// now параметром, а не time.Now() всередині: інакше правило про вік неможливо
// перевірити тестом, не чекаючи два тижні.
func pruneRecordings(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Теки ще нема — це нормальний стан до першого запису.
		if !os.IsNotExist(err) {
			log.Printf("record: не можу прочитати %s для прибирання: %v", dir, err)
		}
		return
	}

	type rec struct {
		path string
		mod  time.Time
		size int64
	}
	var files []rec
	for _, e := range entries {
		if e.IsDir() || !isRecordingFile(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, rec{filepath.Join(dir, e.Name()), fi.ModTime(), fi.Size()})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	maxAge := time.Duration(recordMaxAgeVar.Load())
	var total int64
	kept := files[:0]
	for _, f := range files {
		if now.Sub(f.mod) > maxAge {
			if err := os.Remove(f.path); err != nil {
				log.Printf("record: не зміг прибрати старий %s: %v", f.path, err)
				continue
			}
			log.Printf("record: прибрано за віком: %s (%.0f днів)", filepath.Base(f.path), now.Sub(f.mod).Hours()/24)
			continue
		}
		kept = append(kept, f)
		total += f.size
	}

	// Найстаріші йдуть першими: kept уже відсортований за часом.
	for _, f := range kept {
		if total <= recordMaxBytesVar {
			break
		}
		if err := os.Remove(f.path); err != nil {
			log.Printf("record: не зміг прибрати зайвий %s: %v", f.path, err)
			continue
		}
		total -= f.size
		log.Printf("record: прибрано за обсягом: %s (%.0f МБ, лишилось %.1f ГБ)",
			filepath.Base(f.path), float64(f.size)/(1<<20), float64(total)/(1<<30))
	}
}
