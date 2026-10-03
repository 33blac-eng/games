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
	lastE atomic.Int64 // unix-нс останнього логу помилки
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
	case <-e.done:
		e.pool.Put(bp)
		return 0, net.ErrClosed
	}
}

func (e *egressConn) Close() error {
	var err error
	e.once.Do(func() {
		close(e.done)
		<-e.exit
		err = e.c.Close()
	})
	return err
}

func (e *egressConn) LocalAddr() net.Addr                { return e.c.LocalAddr() }
func (e *egressConn) SetDeadline(t time.Time) error      { return e.c.SetDeadline(t) }
func (e *egressConn) SetReadDeadline(t time.Time) error  { return e.c.SetReadDeadline(t) }
func (e *egressConn) SetWriteDeadline(t time.Time) error { return e.c.SetWriteDeadline(t) }

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

// flush шле пачку: по адресах у порядку першої появи, у межах адреси —
// у порядку черги. Повертає scratch для повторного використання.
func (e *egressConn) flush(batch []egressPkt, scratch []byte) []byte {
	sent := make([]bool, len(batch)) // пачка ≤ egressBatch — дешево
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
	return scratch
}

// sendTo шле пакети batch[idx...] на одну адресу: серії однакового розміру —
// GSO-відправкою, решту — по одному.
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
		if end-k == 1 {
			e.write(*batch[idx[k]].buf, to)
			k++
			continue
		}
		scratch = scratch[:0]
		for _, j := range idx[k:end] {
			scratch = append(scratch, *batch[j].buf...)
		}
		if err := writeGSO(e.c, scratch, seg, to); err != nil {
			// Ядро/драйвер не вміє — вимикаємо GSO назавжди і шлемо ту
			// саму серію по пакету: жоден пакет не губиться через спробу.
			if e.gso.Swap(false) {
				log.Printf("egress: UDP GSO вимкнено (%v) — далі sendto по пакету", err)
			}
			for _, j := range idx[k:end] {
				e.write(*batch[j].buf, to)
			}
		}
		k = end
	}
	return scratch
}

func (e *egressConn) write(b []byte, to netip.AddrPort) {
	if _, err := e.c.WriteToUDPAddrPort(b, to); err != nil {
		e.noteErr(err)
	}
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
