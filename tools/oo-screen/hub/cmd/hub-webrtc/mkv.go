// Мінімальний Matroska-мʼюксер на дві доріжки (H.264 + лінійний PCM) — рівно
// EBML, скільки треба для запису сесії (record.go).
//
// ЧОМУ СВІЙ, А НЕ БІБЛІОТЕКА. Готові варіанти в Go — це at-wat/ebml-go
// (рефлексія + теги структур, ~4 тис. рядків, тягне за собою власну модель
// документа) або обгортки над libav (cgo). Нам потрібні РІВНО чотири речі:
// заголовок EBML, два TrackEntry, Cluster і SimpleBlock. Це 200 рядків
// прямолінійного запису байтів без жодної нової залежності в go.mod — дешевше
// і за розміром, і за ризиком, ніж нова стороння модель документа заради
// чотирьох елементів. Ціна: ми НЕ вміємо читати MKV (не треба), не пишемо Cues
// і SeekHead (див. нижче) і не підтримуємо lacing.
//
// 🔴 ГОЛОВНЕ РІШЕННЯ ФОРМАТУ — ФАЙЛ КОРЕКТНИЙ НА КОЖНОМУ КЛАСТЕРІ.
// Segment пишеться з НЕВІДОМИМ розміром (0x01FF..FF, штатний режим Matroska для
// живих потоків), а кожен Cluster збирається В ПАМʼЯТІ й лягає на диск одним
// шматком уже з відомим розміром. Наслідок: між двома записами на диску файл
// завжди у консистентному стані — жодного розміру, який треба «дописати
// потім», жодного Seek назад. Обрив агента, kill -9 хабу, зникнення живлення —
// плеєр однаково дочитує файл до останнього ЦІЛОГО кластера. Саме це, а не
// акуратний Close(), і є гарантія «файл відкриється».
//
// ponytail: без Cues/SeekHead/Duration — перемотка в плеєрі йде скануванням, а
// тривалість рахується з останнього кластера. Додати варто тоді (і тільки
// тоді), коли записи почнуть дивитись перемоткою, а не з початку; це вимагатиме
// патчити розміри назад по файлу, тобто ЗЛАМАЄ гарантію абзацом вище — тому
// правильний апгрейд не «дописати Cues», а «дописати Cues окремим проходом
// після закриття сесії».
package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"time"

	"github.com/organicoils/oo-screen/internal/opusenc"
	"github.com/organicoils/oo-screen/internal/pcmu"
)

// Ідентифікатори елементів EBML/Matroska. Пишуться як є: канонічне подання ID
// уже містить власну довжину в старших бітах, окремо кодувати її не треба.
const (
	idEBML            = 0x1A45DFA3
	idEBMLVersion     = 0x4286
	idEBMLReadVersion = 0x42F7
	idEBMLMaxIDLength = 0x42F2
	idEBMLMaxSizeLen  = 0x42F3
	idDocType         = 0x4282
	idDocTypeVersion  = 0x4287
	idDocTypeReadVer  = 0x4285

	idSegment        = 0x18538067
	idInfo           = 0x1549A966
	idTimestampScale = 0x2AD7B1
	idMuxingApp      = 0x4D80
	idWritingApp     = 0x5741

	idTracks      = 0x1654AE6B
	idTrackEntry  = 0xAE
	idTrackNumber = 0xD7
	idTrackUID    = 0x73C5
	idTrackType   = 0x83
	idFlagLacing  = 0x9C
	idCodecID     = 0x86
	idCodecPriv   = 0x63A2

	idVideo       = 0xE0
	idPixelWidth  = 0xB0
	idPixelHeight = 0xBA

	idAudio      = 0xE1
	idSampleFreq = 0xB5
	idChannels   = 0x9F
	idBitDepth   = 0x6264

	idCluster      = 0x1F43B675
	idClusterTS    = 0xE7
	idSimpleBlock  = 0xA3
	trackTypeVideo = 1
	trackTypeAudio = 2
	mkvVideoTrack  = 1
	mkvAudioTrack  = 2
	codecIDH264    = "V_MPEG4/ISO/AVC"
	// Звук у файлі — ЛІНІЙНИЙ PCM, хоч по мережі їде μ-law: розтиснути G.711
	// назад у 16 біт точно й у пʼять рядків (pcmu.Decode), а от A_MS/ACM з
	// WAVEFORMATEX у CodecPrivate читають не всі плеєри. Заразом зникають
	// CodecDelay/SeekPreRoll/OpusHead — у PCM їх просто немає.
	codecIDPCM = "A_PCM/INT/LIT"
	// Opus (F1) — пакети як є, без перекодування; OpusHead у CodecPrivate.
	codecIDOpus    = "A_OPUS"
	idCodecDelay   = 0x56AA
	idSeekPreRoll  = 0x56BB
	audioBitDepth  = 16
	segmentUnknown = 0x01FFFFFFFFFFFFFF // «розмір невідомий» (8-байтний vint)
)

const (
	// Межі кластера. minClusterMS не дає плодити кластер на кожен кадр, коли
	// кодер шле суцільні ключові; maxClusterMS тримає відносний timestamp
	// блока в int16 (стеля 32767 мс) із запасом.
	minClusterMS = 1000
	maxClusterMS = 30000
)

// putSize дописує довжину як EBML-vint найменшої ширини. Значення «усі одиниці»
// зарезервоване під «невідомо», тому межа саме (1<<(7*w))-1, а не 1<<(7*w).
func putSize(b *bytes.Buffer, n uint64) {
	for w := 1; w <= 8; w++ {
		if n < uint64(1)<<(7*uint(w))-1 {
			var tmp [8]byte
			binary.BigEndian.PutUint64(tmp[:], n)
			tmp[8-w] |= 1 << (8 - uint(w))
			b.Write(tmp[8-w:])
			return
		}
	}
	// Недосяжно: 8 байтів vint покривають ~2^56 байтів на елемент.
	b.Write([]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE})
}

// putID дописує ID елемента без провідних нулів.
func putID(b *bytes.Buffer, id uint32) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], id)
	i := 0
	for i < 3 && tmp[i] == 0 {
		i++
	}
	b.Write(tmp[i:])
}

// elem — елемент із заданим тілом.
func elem(id uint32, payload []byte) []byte {
	var b bytes.Buffer
	putID(&b, id)
	putSize(&b, uint64(len(payload)))
	b.Write(payload)
	return b.Bytes()
}

// elemUint — беззнакове ціле мінімальної ширини (0 пишеться одним нульовим байтом).
func elemUint(id uint32, v uint64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	i := 0
	for i < 7 && tmp[i] == 0 {
		i++
	}
	return elem(id, tmp[i:])
}

func elemStr(id uint32, s string) []byte { return elem(id, []byte(s)) }

func elemFloat(id uint32, f float64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], math.Float64bits(f))
	return elem(id, tmp[:])
}

// mkvWriter — потоковий запис Matroska. Кластер живе в памʼяті до flush().
type mkvWriter struct {
	w        io.Writer
	err      error
	hasAudio bool

	cl   bytes.Buffer
	clTS int64 // timestamp поточного кластера, мс; -1 = кластера ще немає
}

func newMKVWriter(w io.Writer) *mkvWriter {
	return &mkvWriter{w: w, clTS: -1}
}

func (m *mkvWriter) write(b []byte) {
	if m.err != nil {
		return
	}
	_, m.err = m.w.Write(b)
}

// writeHeader пише EBML-заголовок, Info і Tracks. avcC — готовий
// AVCDecoderConfigurationRecord (record.go); audio=true додає другу доріжку.
//
// DefaultDuration НЕ пишеться взагалі, і це свідомо. Завдання вимагає
// «DefaultDuration=0» у сенсі «постійної тривалості кадру немає» — а нуль
// специфікація забороняє (мінімум 1). Канонічний спосіб сказати «fps змінний» —
// відсутність елемента: тоді тривалість кожного кадру виводиться з його ж
// timestamp, а це рівно те, що нам треба на діапазоні 1..60 к/с.
func (m *mkvWriter) writeHeader(width, height int, avcC []byte, audio bool) error {
	m.hasAudio = audio

	var hdr bytes.Buffer
	hdr.Write(elemUint(idEBMLVersion, 1))
	hdr.Write(elemUint(idEBMLReadVersion, 1))
	hdr.Write(elemUint(idEBMLMaxIDLength, 4))
	hdr.Write(elemUint(idEBMLMaxSizeLen, 8))
	hdr.Write(elemStr(idDocType, "matroska"))
	// DocType 4 лишається: понижувати його заради самого лише A_PCM/INT/LIT
	// сенсу немає, а плеєри читають 4 як читали.
	hdr.Write(elemUint(idDocTypeVersion, 4))
	hdr.Write(elemUint(idDocTypeReadVer, 2))
	m.write(elem(idEBML, hdr.Bytes()))

	// Segment з невідомим розміром: ID + 8-байтний vint з усіх одиниць.
	var seg bytes.Buffer
	putID(&seg, idSegment)
	var unknown [8]byte
	binary.BigEndian.PutUint64(unknown[:], segmentUnknown)
	seg.Write(unknown[:])
	m.write(seg.Bytes())

	var info bytes.Buffer
	info.Write(elemUint(idTimestampScale, 1000000)) // 1 мс на тік
	info.Write(elemStr(idMuxingApp, "oo-screen"))
	info.Write(elemStr(idWritingApp, "oo-screen hub"))
	m.write(elem(idInfo, info.Bytes()))

	var vid bytes.Buffer
	vid.Write(elemUint(idPixelWidth, uint64(width)))
	vid.Write(elemUint(idPixelHeight, uint64(height)))

	var vt bytes.Buffer
	vt.Write(elemUint(idTrackNumber, mkvVideoTrack))
	vt.Write(elemUint(idTrackUID, mkvVideoTrack))
	vt.Write(elemUint(idTrackType, trackTypeVideo))
	vt.Write(elemUint(idFlagLacing, 0))
	vt.Write(elemStr(idCodecID, codecIDH264))
	vt.Write(elem(idCodecPriv, avcC))
	vt.Write(elem(idVideo, vid.Bytes()))

	tracks := elem(idTrackEntry, vt.Bytes())
	if audio {
		var au bytes.Buffer
		var at bytes.Buffer
		at.Write(elemUint(idTrackNumber, mkvAudioTrack))
		at.Write(elemUint(idTrackUID, mkvAudioTrack))
		at.Write(elemUint(idTrackType, trackTypeAudio))
		at.Write(elemUint(idFlagLacing, 0))
		if hubAudioCodec() == opusenc.CodecOpus {
			au.Write(elemFloat(idSampleFreq, opusenc.Rate))
			au.Write(elemUint(idChannels, opusenc.Channels))
			at.Write(elemStr(idCodecID, codecIDOpus))
			// pre-skip 312 — lookahead libopus/gopus на 48 кГц; точне значення
			// агента хабу невідоме, а помилка в кілька мс тут нешкідлива.
			at.Write(elem(idCodecPriv, opusenc.OpusHead(312)))
			at.Write(elemUint(idCodecDelay, uint64(312*time.Second/opusenc.Rate)))
			at.Write(elemUint(idSeekPreRoll, uint64(80*time.Millisecond)))
		} else {
			au.Write(elemFloat(idSampleFreq, pcmu.Rate))
			au.Write(elemUint(idChannels, 1))
			au.Write(elemUint(idBitDepth, audioBitDepth))
			at.Write(elemStr(idCodecID, codecIDPCM))
		}
		at.Write(elem(idAudio, au.Bytes()))
		tracks = append(tracks, elem(idTrackEntry, at.Bytes())...)
	}
	m.write(elem(idTracks, tracks))
	return m.err
}

// pcmBlock розтискає кадр μ-law у лінійний 16-бітний PCM (little-endian) —
// рівно те, що оголошує доріжка A_PCM/INT/LIT.
func pcmBlock(ulaw []byte) []byte {
	out := make([]byte, 0, len(ulaw)*2)
	for _, b := range ulaw {
		out = binary.LittleEndian.AppendUint16(out, uint16(pcmu.Decode(b)))
	}
	return out
}

// audioBlock — тіло аудіо-блока у файлі: μ-law розтискається в PCM, Opus
// лягає як є (A_OPUS).
func audioBlock(frame []byte) []byte {
	if hubAudioCodec() == opusenc.CodecOpus {
		return frame
	}
	return pcmBlock(frame)
}

// block кладе кадр у поточний кластер, відкриваючи новий за потреби. ts — мс від
// початку запису; key — ключовий кадр (для звуку завжди true).
func (m *mkvWriter) block(track uint8, ts int64, key bool, data []byte) {
	if m.err != nil || len(data) == 0 {
		return
	}
	rel := ts - m.clTS
	if m.clTS < 0 || rel >= maxClusterMS || rel < -maxClusterMS ||
		(key && track == mkvVideoTrack && rel >= minClusterMS) {
		m.flush()
		m.clTS = ts
		rel = 0
	}

	var b bytes.Buffer
	b.WriteByte(0x80 | track) // vint номера доріжки (1..127)
	b.WriteByte(byte(rel >> 8))
	b.WriteByte(byte(rel))
	var flags byte
	if key {
		flags = 0x80
	}
	b.WriteByte(flags)
	b.Write(data)
	m.cl.Write(elem(idSimpleBlock, b.Bytes()))
}

// flush дописує поточний кластер на диск ОДНИМ цілим елементом — саме тому файл
// лишається читабельним, навіть якщо процес помер одразу після цього запису.
func (m *mkvWriter) flush() {
	if m.cl.Len() == 0 {
		return
	}
	var body bytes.Buffer
	body.Write(elemUint(idClusterTS, uint64(max(m.clTS, 0))))
	body.Write(m.cl.Bytes())
	m.cl.Reset()
	m.write(elem(idCluster, body.Bytes()))
}

// Close дописує останній кластер. Ідемпотентний: другий виклик нічого не пише.
func (m *mkvWriter) Close() error {
	m.flush()
	m.clTS = -1
	return m.err
}
