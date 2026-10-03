// Запис сесії на диск — на ХАБІ, БЕЗ перекодування, у MKV (mkv.go).
//
// 🔴 ПРАПОРЕЦЬ OO_SCREEN_RECORD=1, ТИПОВО ВИМКНЕНО — рівно як OO_SCREEN_AUDIO у
// audio.go. Без нього startRecording() віддає nil, усі методи нижче nil-безпечні
// й вироджуються в один порівняльний перехід, файл не створюється, каталог не
// чіпається. Прод працює — ламати його заради запису не можна.
//
// ЩО САМЕ ПИШЕТЬСЯ. Ті самі H.264 access units, які хаб уже форвардить, і ті
// самі кадри μ-law, які він уже віддає в аудіо-доріжку. Жодного декодування й
// жодного перекодування: RTP розбирається до NAL-одиниць, NAL-и складаються в
// AU, AU лягає в SimpleBlock. Ціна кадру — одне копіювання payload.
//
// ЗАПИС НЕ СТОЇТЬ НА ШЛЯХУ ГЛЯДАЧА. Точка дотику з прод-шляхом одна:
// rec.offer(pkt) у read loop агентської ноги, і це НЕБЛОКУЮЧИЙ send у чергу.
// Черга повна (диск гальмує) => пакет ВИКИДАЄТЬСЯ із ЗАПИСУ і рахується в
// dropped; форвардинг цього навіть не помічає. Розбір RTP, мультиплексування і
// сам write(2) робить окрема горутина.
//
// ЖИТТЄВИЙ ЦИКЛ = ЖИТТЯ READ LOOP АГЕНТСЬКОЇ НОГИ. Рекордер створюється в
// OnTrack і закривається defer-ом ТІЄЇ Ж горутини, що читає RTP. Цим одним
// defer-ом накриваються геть усі шляхи смерті: агент розірвав звʼязок (ReadRTP
// віддає помилку), агента витіснив новий (generation розійшлась), ноду закрив
// runtime-revoke (closeNode -> agentPC.Close() -> ReadRTP помилка). Окремого
// «а якщо ще отак» не існує, бо іншого виходу з тієї горутини немає.
//
// А ЯКЩО ПРОЦЕС УБИЛИ. Тоді defer не виконається — і саме тому коректність
// файлу НЕ покладена на Close(): кластери пишуться цілими шматками (див.
// mkv.go), тож файл читабельний на будь-якому байті, до якого дійшов запис.
// Close() лише дописує останній неповний кластер (до 1 с) і закриває дескриптор.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"github.com/organicoils/oo-screen/internal/h264"
)

// recordEnabled — прапорець. Змінна, а не os.Getenv на місці: тести перемикають
// її напряму (той самий прийом, що audioEnabled в audio.go).
var recordEnabled = os.Getenv("OO_SCREEN_RECORD") == "1"

// recordDir — куди складати записи. Дефолт відносний: хаб на проді запускається
// зі свого каталогу, і "recordings" поруч із бінарем — це те, що адміністратор
// знайде без документації. Каталог створюється при першому записі.
var recordDir = envOr("OO_SCREEN_RECORD_DIR", "recordings")

// recordQueueDepth — глибина черги до писаря. ~830 пакетів/с на 8 Мбіт/с, тож
// 2048 ≈ 2.5 с запасу: звичайне «диск задумався» переживається без втрат.
// ponytail: стеля — черга рахує ПАКЕТИ, як і viewerQueueDepth у fanout.go.
const recordQueueDepth = 2048

// recItem — одиниця роботи писаря: або RTP-пакет відео, або готовий кадр μ-law.
type recItem struct {
	pkt *rtp.Packet // відео (nil для звуку)
	aud []byte      // кадр μ-law (nil для відео)
	dur time.Duration
	at  time.Time // момент приходу — ним прив'язуються обидві доріжки до t0
}

type recorder struct {
	nodeID string
	dir    string

	ch   chan recItem
	done chan struct{}
	fin  chan struct{}
	once sync.Once

	dropped atomic.Uint64
	// audioOwner — слот годувальника аудіо-доріжки. Глядачів у ноди N, тон у
	// audio.go грає в кожного свій, а доріжка в файлі ОДНА: без цього замка N
	// глядачів написали б у неї N копій того самого звуку.
	audioOwner atomic.Bool

	// --- нижче все належить ВИКЛЮЧНО горутині run(); замків не треба ---
	f    *os.File
	path string
	mkv  *mkvWriter
	dp   codecs.H264Packet // збирач FU-A/STAP-A з pion/rtp (див. коментар у handleVideo)

	t0     time.Time
	haveT0 bool

	// Поточний AU: NAL-и того самого RTP-timestamp накопичуються, доки
	// timestamp не зміниться.
	curTS  uint32
	curAU  []byte
	curPTS time.Duration

	// Прив'язка відео: RTP-timestamp першого пакета і його зсув від t0.
	vRTP0 uint32
	vBase time.Duration
	haveV bool

	// Прив'язка звуку: PTS наступного кадру звуку (перший — від t0, далі
	// накопиченням тривалостей).
	aPTS  time.Duration
	haveA bool

	sps, pps []byte
	started  bool
	vFrames  int
	aFrames  int

	// lastPTS — мітка останнього ЗАПИСАНОГО кадру. RTP не гарантує порядку, і
	// пакет, що спізнився, дає мітку, меншу за попередню. Такий кадр ламає
	// монотонність у файлі («non monotonically increasing dts»), а плеєрам це
	// або гикавка, або відмова. Спізнілий кадр нікому не потрібен — він уже
	// показаний.
	lastPTS     time.Duration
	haveLast    bool
	droppedLate int
}

// startRecording піднімає писаря сесії. nil (і жодного сліду на диску), поки
// OO_SCREEN_RECORD не заданий — це і є «без прапорця нічого не змінилось».
func startRecording(nodeID string) *recorder {
	if !recordEnabled {
		return nil
	}
	// Запобіжник місця (recordprune.go). Прибирає застаріле й відмовляється
	// починати, коли на диску тісно. Саме ТУТ, а не у фоновому таймері: у
	// момент старту сесії ми ще можемо чесно сказати «не пишу», а посеред
	// запису вибір уже між зіпсованим файлом і забитим диском.
	if !recordingsFit(recordDir) {
		return nil
	}
	r := &recorder{
		nodeID: nodeID,
		dir:    recordDir,
		ch:     make(chan recItem, recordQueueDepth),
		done:   make(chan struct{}),
		fin:    make(chan struct{}),
	}
	go r.run()
	return r
}

// offer — єдина точка дотику із шляхом форвардингу. Неблокуюча за побудовою.
func (r *recorder) offer(pkt *rtp.Packet) {
	if r == nil {
		return
	}
	r.push(recItem{pkt: pkt, at: time.Now()})
}

// offerAudio приймає готовий кадр μ-law. Копіюємо: кадр прийшов із чужого
// буфера (черга глядача в audio.go), а писар дістанеться до нього пізніше.
func (r *recorder) offerAudio(frame []byte, dur time.Duration) {
	if r == nil || len(frame) == 0 || dur <= 0 {
		return
	}
	r.push(recItem{aud: append([]byte(nil), frame...), dur: dur, at: time.Now()})
}

// push — неблокуючий send. Канал НІКОЛИ не закривається (у нього пишуть кілька
// горутин), зупинку сигналить done; після закриття done send у ще живий буфер
// нікому не шкодить — писар його просто не забере.
func (r *recorder) push(it recItem) {
	select {
	case <-r.done:
	case r.ch <- it:
	default:
		r.dropped.Add(1)
	}
}

// claimAudio віддає слот годувальника аудіо-доріжки рівно одному викликачу.
func (r *recorder) claimAudio() bool {
	return r != nil && r.audioOwner.CompareAndSwap(false, true)
}

func (r *recorder) releaseAudio() {
	if r != nil {
		r.audioOwner.Store(false)
	}
}

// Close зупиняє писаря і ЧЕКАЄ, поки файл дописано й закрито. Ідемпотентний.
func (r *recorder) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() { close(r.done) })
	<-r.fin
}

func (r *recorder) run() {
	defer close(r.fin)
	for {
		select {
		case it := <-r.ch:
			r.handle(it)
		case <-r.done:
			// Дочищаємо чергу: те, що вже прийнято, має потрапити у файл —
			// інакше кінець запису втрачав би до 2.5 с картинки на кожному
			// штатному закритті.
			for drain := true; drain; {
				select {
				case it := <-r.ch:
					r.handle(it)
				default:
					drain = false
				}
			}
			r.finish()
			return
		}
	}
}

func (r *recorder) handle(it recItem) {
	if !r.haveT0 {
		r.t0, r.haveT0 = it.at, true
	}
	if it.pkt != nil {
		r.handleVideo(it)
		return
	}
	r.handleAudio(it)
}

// handleVideo збирає AU з RTP і віддає його у файл, коли AU закінчився.
//
// 🔴 ПАСТКА RTP->ФАЙЛ. У файл не можна класти сирі RTP-payload-и: H.264 їде
// фрагментами FU-A і пачками STAP-A. Збирач тут НЕ пишеться наново — його вже
// має pion, який у нас і так у go.mod: codecs.H264Packet.Unmarshal сам тримає
// буфер FU-A, віддає порожньо на середніх фрагментах і повний NAL зі стартовим
// кодом на End-біті, а STAP-A розкладає на кілька NAL-ів. Тобто конкатенація
// його виходів — це вже валідний Annex-B.
//
// Другу половину (Annex-B -> NAL-и, SPS -> розміри/профіль) робить наш
// internal/h264: SplitNALs і ParseSPS, ті самі, якими користується encode-probe.
//
// Межа AU — зміна RTP-timestamp, а не marker-біт: marker може загубитись разом
// із пакетом, timestamp — ні.
func (r *recorder) handleVideo(it recItem) {
	pkt := it.pkt
	if !r.haveV {
		r.vRTP0, r.vBase, r.haveV = pkt.Timestamp, it.at.Sub(r.t0), true
		r.curTS, r.curPTS = pkt.Timestamp, r.vBase
	}
	if pkt.Timestamp != r.curTS {
		r.flushAU()
		r.curTS = pkt.Timestamp
		r.curPTS = r.videoPTS(pkt.Timestamp)
	}
	nals, err := r.dp.Unmarshal(pkt.Payload)
	if err != nil {
		// Втрачений/битий фрагмент: AU вийде неповним — рівно те саме, що
		// побачить глядач. Запис не має права бути чистішим за потік. Але
		// позначаємо його рваним, щоб файл не ПОЧАВСЯ з такого кадру.
		return
	}
	r.curAU = append(r.curAU, nals...)
}

// videoPTS — PTS кадру: зсув першого пакета від початку запису плюс дельта за
// годинником кодера (90 кГц). Саме дельта RTP, а не час приходу: змінний fps
// має лягти у файл таким, яким його зробив кодер, без джитера мережі. Різниця
// береться зі знаком у int32 — 32-бітний RTP-timestamp обертається кожні ~13 год.
func (r *recorder) videoPTS(ts uint32) time.Duration {
	return r.vBase + time.Duration(int32(ts-r.vRTP0))*time.Second/90000
}

// flushAU переводить зібраний Annex-B AU в AVCC (довжина+NAL, як вимагає
// V_MPEG4/ISO/AVC) і кладе в кластер. Файл народжується тут же — на ПЕРШОМУ
// ключовому AU, у якому є SPS і PPS: раніше нема з чого зібрати CodecPrivate, а
// кадри до першого IDR все одно не декодуються.
func (r *recorder) flushAU() {
	au := r.curAU
	r.curAU = nil
	if len(au) == 0 {
		return
	}
	nals := h264.SplitNALs(au)
	if len(nals) == 0 {
		return
	}

	key := false
	startsFrame := false
	var avcc bytes.Buffer
	for _, n := range nals {
		switch n[0] & 0x1F {
		case h264.NALSPS:
			if r.sps == nil {
				r.sps = append([]byte(nil), n...)
			}
		case h264.NALPPS:
			if r.pps == nil {
				r.pps = append([]byte(nil), n...)
			}
		case h264.NALIDR:
			key = true
			// Чи цей зріз починає КАДР. first_mb_in_slice — перше поле
			// slice_header, кодоване ue(v); нуль записується одним бітом «1»,
			// тобто старший біт першого байта після NAL-заголовка.
			//
			// Саме цього бракувало файлу з прода: коли ми підключаємось
			// посеред кадру, у AU лишається ХВІСТ IDR — NAL-тип каже
			// «ключовий», а зрізи починаються з середини, і декодер відмовляє
			// («A non-intra slice in an IDR NAL unit»). Ознака за вмістом, а
			// не за нумерацією RTP: власна черга писаря теж відкидає пакети
			// під сплеском, і рахувати ті відкидання втратами означало б
			// не почати запис ніколи.
			if len(n) > 1 && n[1]&0x80 != 0 {
				startsFrame = true
			}
		}
		avcc.Write(binary.BigEndian.AppendUint32(nil, uint32(len(n))))
		avcc.Write(n)
	}

	if !r.started {
		if !key || !startsFrame || len(r.sps) < 4 || len(r.pps) == 0 || !r.open() {
			return
		}
	}
	// Мітки у файлі мусять лише зростати. Пакет, що прийшов не по порядку,
	// дав би крок назад — і файл стає «non monotonically increasing dts»,
	// на чому суворі плеєри спотикаються. Спізнілий кадр просто зайвий.
	if r.haveLast && r.curPTS <= r.lastPTS {
		r.droppedLate++
		return
	}
	r.mkv.block(mkvVideoTrack, r.curPTS.Milliseconds(), key, avcc.Bytes())
	r.lastPTS, r.haveLast = r.curPTS, true
	r.vFrames++
}

func (r *recorder) handleAudio(it recItem) {
	// Немає файлу або в ньому не оголошено аудіо-доріжку — звуку нема куди
	// класти. Мовчки пропускаємо: «доріжки немає» не привід падати.
	if !r.started || !r.mkv.hasAudio {
		return
	}
	if !r.haveA {
		r.aPTS, r.haveA = it.at.Sub(r.t0), true
	}
	r.mkv.block(mkvAudioTrack, r.aPTS.Milliseconds(), true, pcmBlock(it.aud))
	r.aPTS += it.dur
	r.aFrames++
}

// open створює файл сесії й пише заголовок. false = писати нікуди (помилка вже
// в журналі); писар після цього далі крутиться вхолосту, нічого не ламаючи.
func (r *recorder) open() bool {
	sps, err := h264.ParseSPS(r.sps)
	if err != nil || sps.Width <= 0 || sps.Height <= 0 {
		log.Printf("record: SPS ноди %s не розібрався (%v) — сесія не пишеться", r.nodeID, err)
		r.started = true // не пробуємо на кожному наступному кадрі
		r.mkv = newMKVWriter(discardWriter{})
		return false
	}
	// SEC: запис — це кадри чужого екрана (паролі, листування). Каталог і
	// файл — лише для власника процесу хаба, а не 0755/0644 для всіх на VPS.
	if err := os.MkdirAll(r.dir, recordDirPerm); err != nil {
		log.Printf("record: каталог %s не створився: %v", r.dir, err)
		r.started = true
		r.mkv = newMKVWriter(discardWriter{})
		return false
	}
	name := fmt.Sprintf("%s-%s.mkv", safeNodeID(r.nodeID), time.Now().UTC().Format("20060102-150405"))
	// O_EXCL: дві сесії однієї ноди в ту саму секунду не обнуляють запис
	// одна одній (os.Create робив O_TRUNC), і підкладений симлінк не відкриваємо.
	f, err := os.OpenFile(filepath.Join(r.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, recordFilePerm)
	if err != nil {
		log.Printf("record: файл %s не створився: %v", name, err)
		r.started = true
		r.mkv = newMKVWriter(discardWriter{})
		return false
	}
	r.f, r.path = f, f.Name()
	r.mkv = newMKVWriter(f)
	if err := r.mkv.writeHeader(sps.Width, sps.Height, avcC(r.sps, r.pps), audioEnabled); err != nil {
		log.Printf("record: заголовок %s не записався: %v", r.path, err)
	}
	r.started = true
	log.Printf("record: пишу сесію [node=%s] -> %s (%dx%d, звук=%v)",
		r.nodeID, r.path, sps.Width, sps.Height, audioEnabled)
	return true
}

func (r *recorder) finish() {
	r.flushAU()
	if r.f == nil {
		if d := r.dropped.Load(); d > 0 {
			log.Printf("record: сесія [node=%s] не записана (викинуто %d пакетів)", r.nodeID, d)
		}
		return
	}
	if err := r.mkv.Close(); err != nil {
		log.Printf("record: останній кластер %s не дописався: %v", r.path, err)
	}
	if err := r.f.Close(); err != nil {
		log.Printf("record: файл %s не закрився: %v", r.path, err)
	}
	log.Printf("record: сесію закрито [node=%s] -> %s (%d кадрів відео, %d кадрів звуку, викинуто %d пакетів)",
		r.nodeID, r.path, r.vFrames, r.aFrames, r.dropped.Load())
}

// discardWriter — куди пише мʼюксер, якщо файл відкрити не вдалось. Дешевше за
// перевірку на nil у кожному block().
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// avcC — AVCDecoderConfigurationRecord для CodecPrivate доріжки V_MPEG4/ISO/AVC:
// один SPS, один PPS, довжина NAL — 4 байти (0xFF: старші 6 біт зарезервовані,
// молодші 2 = lengthSizeMinusOne). Викликати лише коли len(sps) >= 4.
func avcC(sps, pps []byte) []byte {
	b := []byte{1, sps[1], sps[2], sps[3], 0xFF, 0xE1}
	b = binary.BigEndian.AppendUint16(b, uint16(len(sps)))
	b = append(b, sps...)
	b = append(b, 1)
	b = binary.BigEndian.AppendUint16(b, uint16(len(pps)))
	return append(b, pps...)
}

// safeNodeID — node_id приходить ВІД АГЕНТА, тобто ззовні, а ми робимо з нього
// імʼя файлу. Лишаємо тільки [A-Za-z0-9._-]: інакше "../../etc/passwd" як node
// став би шляхом, а не назвою.
// recordDirPerm / recordFilePerm — права на записи сесій (SEC-аудит).
const (
	recordDirPerm  = 0o700
	recordFilePerm = 0o600
)

func safeNodeID(id string) string {
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id) && i < 64; i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "node"
	}
	return string(out)
}

// recordIdleClose — скільки чекаємо після ОСТАННЬОГО глядача, перш ніж закрити
// файл. Перепідключення після обриву (оновили сторінку, мигнула мережа) лишається
// в тому самому файлі, а сеанс наступного дня — вже в новому.
var recordIdleClose = 30 * time.Second

// scheduleRecordClose закриває поточний файл ноди, якщо за recordIdleClose так і
// не зʼявився жоден глядач. Close() чекає на писаря, тому поза таймерною горутиною.
// ponytail: між перевіркою лічильника і Swap новий глядач може встигнути відкрити
// файл — тоді він закриється, і наступний пакет відкриє ще один. Короткий зайвий
// файл, не втрата запису.
func scheduleRecordClose(ns *nodeSession) {
	if !recordEnabled {
		return
	}
	time.AfterFunc(recordIdleClose, func() {
		if ns.viewerCount.Load() != 0 {
			return
		}
		if cur := ns.rec.Swap(nil); cur != nil {
			cur.Close()
		}
	})
}
