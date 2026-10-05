// B7: один писар на спільний сокет ICE UDP mux.
//
// Чому. З H-06 усі ноги процесу (агенти і КОЖЕН глядач) ходять через ОДИН
// UDP-сокет. Кожна нога глядача має свій pump (fanout.go), і всі вони пишуть
// у той самий fd одночасно. Go серіалізує записи в fd власним fdMutex, тож 16
// писарів не йдуть паралельно, а стоять у черзі на мʼютексі, паркуються й
// будяться (futex + планувальник) — і разом виходять ПОВІЛЬНІШЕ, ніж один
// писар. Замір на цьому боксі (5280 пакетів по 1216 Б на 16 адрес, 2 ядра):
// 16 писарів в один сокет — 7–8 мкс/пакет, один писар — 3,2–3,6 мкс/пакет,
// один писар з UDP GSO — 1,1 мкс/пакет. IDR-пачка 1×16 — це тисячі пакетів,
// і саме час її проштовхування через сокет і був хвостом p99 у 43 мс.
//
// Що робить egressConn. Обгортка над *net.UDPConn, яку бачить pion: читання —
// напряму, запис — копія в чергу і повернення. Єдина горутина-писар забирає
// з черги все, що встигло накопичитись (до egressBatch), групує за адресою
// призначення (порядок у межах адреси зберігається — для SRTP і NACK важливий
// саме він) і шле серії однакових за розміром пакетів однією GSO-відправкою
// (Linux, UDP_SEGMENT; останній сегмент може бути коротшим). Де GSO немає
// (Windows, старе ядро, драйвер відмовив) — звичайний sendto по пакету, але
// все одно з ОДНІЄЇ горутини, без бійки за fdMutex.
//
// Чого НЕ змінює. Помилка запису більше не повертається в pion синхронно:
// для UDP це й так була помилка «десь колись» (ICMP, повний буфер), pion на
// неї лише логує. Тут вона рахується й логується з обмеженням частоти.
package main

import (
	"errors"
	"log"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// egressQueue — пакетів у черзі писаря. 4096 x ~1,2 КБ ≈ 5 МБ: більше
	// за IDR-пачку на 16 глядачів. Переповнення = блокування pump-а, тобто
	// той самий зворотний тиск, що й повний буфер сокета раніше.
	egressQueue = 4096
	// egressBatch — скільки пакетів писар забирає за один прохід.
	egressBatch = 256
	// gsoMaxSegs/gsoMaxBytes — межі однієї GSO-відправки (ядро: 64 сегменти,
	// датаграма ≤ 64 КБ).
	gsoMaxSegs  = 64
	gsoMaxBytes = 65000
	// egressArena — байти арени для GSO-копій однієї пачки (egressBatch
	// пакетів по ≤ 1500 Б уміщаються цілком).
	egressArena = egressBatch * 1500
)

type egressPkt struct {
	buf *[]byte
	to  netip.AddrPort
}

type egressConn struct {
	c     *net.UDPConn
	q     chan egressPkt
	done  chan struct{}
	once  sync.Once
	exit  chan struct{}
	pool  sync.Pool
	gso   atomic.Bool
	errs  atomic.Uint64
	full  atomic.Uint64 // скільки разів черга була повна (писар блокувався)
	lastE atomic.Int64  // unix-нс останнього логу помилки
	mmsg  atomic.Bool   // R4: sendmmsg доступний (Linux)

	// Стан писаря (лише горутина writer): перевикористовуються між пачками.
	msgs []egressMsg
	ord  []int
	sent []bool
	mm   mmsgState
}

func newEgressConn(c *net.UDPConn) *egressConn {
	e := &egressConn{
		c:    c,
		q:    make(chan egressPkt, egressQueue),
		done: make(chan struct{}),
		exit: make(chan struct{}),
	}
	e.pool.New = func() any { b := make([]byte, 0, 1500); return &b }
	e.gso.Store(gsoSupported(c))
	e.mmsg.Store(e.mm.init(c))
	registerEgress(e)
	go e.writer()
	return e
}

// --- net.PacketConn + ice.AddrPortReaderWriter ---

func (e *egressConn) ReadFrom(b []byte) (int, net.Addr, error) { return e.c.ReadFrom(b) }
func (e *egressConn) ReadFromAddrPort(b []byte) (int, netip.AddrPort, error) {
	return e.c.ReadFromUDPAddrPort(b)
}

func (e *egressConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return e.c.WriteTo(b, addr)
	}
	return e.WriteToAddrPort(b, ua.AddrPort())
}

func (e *egressConn) WriteToAddrPort(b []byte, to netip.AddrPort) (int, error) {
	bp := e.pool.Get().(*[]byte)
	*bp = append((*bp)[:0], b...)
	select {
	case <-e.done:
		e.pool.Put(bp)
		return 0, net.ErrClosed
	default:
	}
	select {
	case e.q <- egressPkt{bp, to}:
		return len(b), nil
	default:
		e.full.Add(1)
	}
	select {
	case e.q <- egressPkt{bp, to}:
		return len(b), nil
	case <-e.done:
		e.pool.Put(bp)
		return 0, net.ErrClosed
	}
}

func (e *egressConn) Close() error {
	var err error
	e.once.Do(func() {
		unregisterEgress(e)
		close(e.done)
		<-e.exit
		err = e.c.Close()
	})
	return err
}

func (e *egressConn) LocalAddr() net.Addr               { return e.c.LocalAddr() }
func (e *egressConn) SetReadDeadline(t time.Time) error { return e.c.SetReadDeadline(t) }

// SetDeadline/SetWriteDeadline НЕ чіпають дедлайн запису сокета. pion
// (UDPMuxDefault.abortWrite, при закритті будь-якої muxed-ноги) ставить на
// спільний сокет SetWriteDeadline(now), щоб розблокувати свої записи. Але
// пише в сокет лише наш writer, а записи pion блокуються на черзі, не на fd:
// дедлайн бив по writer-у (i/o timeout на чужих пакетах) і, гірше, вимикав
// GSO назавжди — у логах 10×20 це траплялось на кожному прогоні.
func (e *egressConn) SetDeadline(t time.Time) error      { return e.c.SetReadDeadline(t) }
func (e *egressConn) SetWriteDeadline(t time.Time) error { return nil }

// --- писар ---

func (e *egressConn) writer() {
	defer close(e.exit)
	batch := make([]egressPkt, 0, egressBatch)
	var scratch []byte
	for {
		select {
		case <-e.done:
			return
		case p := <-e.q:
			batch = append(batch[:0], p)
		}
	drain:
		for len(batch) < egressBatch {
			select {
			case p := <-e.q:
				batch = append(batch, p)
			default:
				break drain
			}
		}
		scratch = e.flush(batch, scratch)
		for i := range batch {
			e.pool.Put(batch[i].buf)
			batch[i] = egressPkt{}
		}
	}
}

// egressMsg — одна датаграма для ядра: або поодинокий пакет, або GSO-серія
// (seg > 0) однакових пакетів на одну адресу. pkts — номери пакетів пачки
// (у e.ord[i0:i1]): для fanoutLat і для відкату «по пакету».
type egressMsg struct {
	b      []byte
	seg    int
	to     netip.AddrPort
	i0, i1 int
}

// flush шле пачку: по адресах у порядку першої появи, у межах адреси —
// у порядку черги. Повертає scratch для повторного використання.
//
// R4: датаграми всієї пачки збираються в e.msgs і йдуть в ядро ОДНИМ
// sendmmsg (Linux) замість sendto на кожну. На сотнях глядачів у пачці
// майже немає двох пакетів на одну адресу, тож GSO сам по собі не рятує, і
// syscall-и були ~45 % CPU хаба (pprof, 10×20, див. bench/RESULTS-hub.md, «R4 (хвиля 5)»).
func (e *egressConn) flush(batch []egressPkt, scratch []byte) []byte {
	if cap(scratch) < egressArena {
		scratch = make([]byte, 0, egressArena)
	}
	scratch = scratch[:0]
	e.msgs = e.msgs[:0]
	e.ord = e.ord[:0]
	if cap(e.sent) < len(batch) {
		e.sent = make([]bool, egressBatch)
	}
	sent := e.sent[:len(batch)]
	clear(sent)
	idx := make([]int, 0, len(batch))
	for i := range batch {
		if sent[i] {
			continue
		}
		to := batch[i].to
		idx = idx[:0]
		for j := i; j < len(batch); j++ {
			if !sent[j] && batch[j].to == to {
				idx = append(idx, j)
				sent[j] = true
			}
		}
		scratch = e.sendTo(batch, idx, to, scratch)
	}
	e.sendMsgs(batch)
	return scratch
}

// sendTo розкладає пакети batch[idx...] на одну адресу в датаграми e.msgs:
// серії однакового розміру — GSO-датаграмою (копія в арену scratch), решту —
// по одній (без копії, буфер із черги живий до кінця flush).
func (e *egressConn) sendTo(batch []egressPkt, idx []int, to netip.AddrPort, scratch []byte) []byte {
	for k := 0; k < len(idx); {
		seg := len(*batch[idx[k]].buf)
		end := k + 1
		total := seg
		if e.gso.Load() {
			for end < len(idx) && end-k < gsoMaxSegs {
				n := len(*batch[idx[end]].buf)
				if n > seg || total+n > gsoMaxBytes {
					break
				}
				total += n
				end++
				if n < seg { // коротший сегмент можливий лише останнім
					break
				}
			}
		}
		i0 := len(e.ord)
		e.ord = append(e.ord, idx[k:end]...)
		if end-k == 1 {
			e.msgs = append(e.msgs, egressMsg{b: *batch[idx[k]].buf, to: to, i0: i0, i1: len(e.ord)})
			k = end
			continue
		}
		if len(scratch)+total > cap(scratch) {
			// Арена скінчилась (пакети > 1500 Б): віддаємо зібране і
			// починаємо арену заново — старі зрізи вже в ядрі.
			e.sendMsgs(batch)
			e.msgs = e.msgs[:0]
			scratch = scratch[:0]
		}
		start := len(scratch)
		for _, j := range idx[k:end] {
			scratch = append(scratch, *batch[j].buf...)
		}
		e.msgs = append(e.msgs, egressMsg{b: scratch[start:len(scratch):len(scratch)], seg: seg, to: to, i0: i0, i1: len(e.ord)})
		k = end
	}
	return scratch
}

var (
	errNoMmsg          = errors.New("sendmmsg вимкнено")
	errMmsgUnsupported = errors.New("sendmmsg недоступний")
)

// sendMulti — скільки датаграм із початку msgs ядро прийняло одним викликом.
func (e *egressConn) sendMulti(msgs []egressMsg) (int, error) {
	if !e.mmsg.Load() {
		return 0, errNoMmsg
	}
	return e.mm.send(msgs)
}

// sendMsgs віддає e.msgs ядру: пачкою (sendmmsg), де вміємо, інакше по одній.
func (e *egressConn) sendMsgs(batch []egressPkt) {
	msgs := e.msgs
	for len(msgs) > 0 {
		n, err := e.sendMulti(msgs)
		if n > 0 {
			if fanoutLatOn {
				now := time.Now().UnixNano()
				for _, m := range msgs[:n] {
					for _, j := range e.ord[m.i0:m.i1] {
						fanoutLatSent(*batch[j].buf, now)
					}
				}
			}
			msgs = msgs[n:]
			continue
		}
		if errors.Is(err, errMmsgUnsupported) && e.mmsg.Swap(false) {
			log.Printf("egress: sendmmsg вимкнено (%v) — далі по датаграмі", err)
		}
		// Перша датаграма — окремо (тут і відкат GSO, і облік помилки).
		e.sendOne(batch, msgs[0])
		msgs = msgs[1:]
	}
}

func (e *egressConn) sendOne(batch []egressPkt, m egressMsg) {
	if m.seg == 0 {
		e.write(m.b, m.to)
		return
	}
	err := writeGSO(e.c, m.b, m.seg, m.to)
	if err == nil {
		if fanoutLatOn {
			now := time.Now().UnixNano()
			for _, j := range e.ord[m.i0:m.i1] {
				fanoutLatSent(*batch[j].buf, now)
			}
		}
		return
	}
	// Ядро/драйвер не вміє — вимикаємо GSO назавжди і шлемо ту
	// саму серію по пакету: жоден пакет не губиться через спробу.
	// Тайм-аут — не «не вміє»: GSO не чіпаємо.
	if !errors.Is(err, os.ErrDeadlineExceeded) && e.gso.Swap(false) {
		log.Printf("egress: UDP GSO вимкнено (%v) — далі sendto по пакету", err)
	}
	for _, j := range e.ord[m.i0:m.i1] {
		e.write(*batch[j].buf, m.to)
	}
}

func (e *egressConn) write(b []byte, to netip.AddrPort) {
	if _, err := e.c.WriteToUDPAddrPort(b, to); err != nil {
		e.noteErr(err)
		return
	}
	fanoutLatSent(b, time.Now().UnixNano())
}

func (e *egressConn) noteErr(err error) {
	if errors.Is(err, net.ErrClosed) {
		return
	}
	n := e.errs.Add(1)
	now := time.Now().UnixNano()
	if last := e.lastE.Load(); now-last > int64(10*time.Second) && e.lastE.CompareAndSwap(last, now) {
		log.Printf("egress: помилка запису UDP (усього %d): %v", n, err)
	}
}
