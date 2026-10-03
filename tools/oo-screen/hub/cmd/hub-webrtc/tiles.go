// Текстові тайли (bench/quality/STAGE3-444.md, рекомендація B): агент шле
// lossless PNG-тайли кольорового тексту нерухомого екрана каналом
// "oosc-tiles", хаб пересилає їх УСІМ глядачам ноди тим самим каналом.
//
// 🔴 ПРАПОРЕЦЬ OO_SCREEN_TILES=1, ТИПОВО ВИМКНЕНО. Без нього хаб не бере
// канал ні від агента, ні від глядача — поведінка бітово та сама, що й до
// появи цього файла.
//
// Хаб тут — не транслятор із довірою, а труба з засувками:
//   - кожне повідомлення агента проходить tiles.Decode (магія, версія, межі,
//     розмір ≤ tiles.MaxMessage) — сміття далі не йде;
//   - кожен глядач має обмежену чергу (tilesQueueDepth повідомлень) і стелю
//     буфера SCTP (tilesMaxBuffered): повільний глядач втрачає тайли, а не
//     пам'ять хаба; invalidate не губиться ніколи — він витісняє чергу;
//   - кеш поточного епізоду (≤ tilesCacheBytes) — щоб глядач, який прийшов на
//     вже нерухомий екран, отримав тайли без нового епізоду на агенті.
package main

import (
	"encoding/binary"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/organicoils/oo-screen/internal/tiles"
	"github.com/pion/webrtc/v4"
)

// tilesLabel — мітка каналу на обох ногах.
const tilesLabel = tiles.ChannelLabel

var tilesEnabled = os.Getenv("OO_SCREEN_TILES") == "1"

const (
	// tilesQueueDepth — повідомлень у черзі одного глядача (≤ 64 × 64 КіБ).
	tilesQueueDepth = 64
	// tilesMaxBuffered — понад стільки байтів у SCTP-буфері глядача тайли
	// викидаються (invalidate — ні).
	tilesMaxBuffered = 1 << 20
	// tilesCacheBytes — стеля кешу епізоду на ноду (агент сам ріже на 2 МБ).
	tilesCacheBytes = 3 << 20
)

// tilesCache — дзеркало того, що тримає плеєр ноди: останній invalidate
// поточної епохи + утримані тайли (rect → сире повідомлення та епоха, в якій
// тайл чинний). Тайли переживають епохи: tiles.TypeKeep переводить
// перелічені в нову епоху й викидає решту (як плеєр). Пізній глядач
// отримує ПОВНИЙ набір чинних тайлів, перештампований у поточну епоху
// (tiles.Restamp), а не keep-посилання на тайли, яких у нього немає.
// Захищений власним mu, не ns.mu.
type tilesCache struct {
	mu         sync.Mutex
	epoch      uint32
	have       bool
	inv        []byte // invalidate поточної епохи (nil — епоха почалась без нього)
	keepEpoch  uint32 // епоха останнього TypeKeep
	keepSeen   bool
	srcW, srcH uint16
	held       map[tiles.TileKey]*heldTile
	bytes      int
	dropped    atomic.Uint64 // відкинуто на вході від агента (невалідні/завеликі)
}

type heldTile struct {
	raw   []byte
	epoch uint32
}

func (c *tilesCache) dropWhere(f func(*heldTile) bool) {
	for k, t := range c.held {
		if f(t) {
			c.bytes -= len(t.raw)
			delete(c.held, k)
		}
	}
}

// setEpoch — нова епоха без invalidate (keep/тайл прийшов першим).
func (c *tilesCache) setEpoch(e uint32) {
	if !c.have || e != c.epoch {
		c.epoch, c.have, c.inv = e, true, nil
	}
}

// remember застосовує валідне повідомлення до кешу. Для TypeKeep повертає
// перелічені тайли, перештамповані в епоху keep-а (ремонт для глядачів, що
// губили тайли).
func (c *tilesCache) remember(m *tiles.Msg, raw []byte, keepRects []tiles.Rect) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch m.Type {
	case tiles.TypeInvalidate:
		c.epoch, c.have, c.inv = m.Epoch, true, raw
		return nil
	case tiles.TypeKeep:
		c.setEpoch(m.Epoch)
		c.keepEpoch, c.keepSeen = m.Epoch, true
		want := make(map[tiles.TileKey]bool, len(keepRects))
		order := make([]tiles.TileKey, 0, len(keepRects))
		for _, r := range keepRects {
			k := tiles.TileKey{X: uint16(r.X), Y: uint16(r.Y), W: uint16(r.W), H: uint16(r.H)}
			want[k] = true
			order = append(order, k)
		}
		sameSrc := c.srcW == m.SrcW && c.srcH == m.SrcH
		for k, t := range c.held {
			if !sameSrc || !want[k] {
				c.bytes -= len(t.raw)
				delete(c.held, k)
			}
		}
		c.srcW, c.srcH = m.SrcW, m.SrcH
		var repair [][]byte
		for _, k := range order {
			if t := c.held[k]; t != nil {
				t.epoch = m.Epoch
				repair = append(repair, tiles.Restamp(t.raw, m.Epoch))
			}
		}
		return repair
	}
	// TypeTile
	c.setEpoch(m.Epoch)
	if !c.keepSeen || c.keepEpoch != m.Epoch {
		// Епізод без keep (старий агент): старі тайли недійсні.
		e := m.Epoch
		c.dropWhere(func(t *heldTile) bool { return t.epoch != e })
	}
	if c.srcW != m.SrcW || c.srcH != m.SrcH {
		c.dropWhere(func(*heldTile) bool { return true })
		c.srcW, c.srcH = m.SrcW, m.SrcH
	}
	k := tiles.TileKey{X: m.X, Y: m.Y, W: m.W, H: m.H}
	if c.held == nil {
		c.held = make(map[tiles.TileKey]*heldTile)
	}
	if old := c.held[k]; old != nil {
		c.bytes -= len(old.raw)
		delete(c.held, k)
	}
	if c.bytes+len(raw) > tilesCacheBytes {
		return nil
	}
	c.held[k] = &heldTile{raw: raw, epoch: m.Epoch}
	c.bytes += len(raw)
	return nil
}

// snapshot — повний стан для глядача, що щойно відкрив канал: invalidate
// епохи, порожній keep (новий плеєр стирає будь-яке власне сховище), далі
// всі чинні тайли, перештамповані в поточну епоху, у стабільному порядку.
func (c *tilesCache) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.have {
		return nil
	}
	var out [][]byte
	if c.inv != nil {
		out = append(out, c.inv)
	}
	var cur []tiles.TileKey
	for k, t := range c.held {
		if t.epoch == c.epoch {
			cur = append(cur, k)
		}
	}
	if len(cur) == 0 {
		return out
	}
	if keep, err := tiles.Keep(c.epoch, 0, int(c.srcW), int(c.srcH), nil); err == nil {
		out = append(out, keep)
	}
	sort.Slice(cur, func(i, j int) bool {
		a, b := cur[i], cur[j]
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
	for _, k := range cur {
		t := c.held[k]
		if t.epoch == c.epoch && len(t.raw) >= tiles.HeaderSize &&
			binary.LittleEndian.Uint32(t.raw[4:]) == c.epoch {
			out = append(out, t.raw)
		} else {
			out = append(out, tiles.Restamp(t.raw, c.epoch))
		}
	}
	return out
}

// reset — агентська нога пішла: кеш недійсний. Повертає synthetic invalidate
// для глядачів (епоха +1 від останньої, щоб плеєр точно все стер).
func (c *tilesCache) reset() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	had := c.have
	c.epoch++
	c.have, c.inv, c.held, c.bytes, c.keepSeen, c.srcW, c.srcH = false, nil, nil, 0, false, 0, 0
	if !had {
		return nil
	}
	return tiles.Invalidate(c.epoch, 0)
}

// onAgentTiles — повідомлення від агента. Невалідне — відкидається тут.
func onAgentTiles(ns *nodeSession, data []byte) {
	if len(data) > tiles.MaxMessage {
		ns.tiles.dropped.Add(1)
		return
	}
	m, err := tiles.Decode(data)
	if err != nil {
		if ns.tiles.dropped.Add(1)%100 == 1 {
			log.Printf("tiles: від агента відкинуто [node=%s]: %v", ns.nodeID, err)
		}
		return
	}
	raw := append([]byte(nil), data...) // pion перевикористовує буфер
	m.Payload = nil
	if m.Type == tiles.TypeStill {
		// Анонс keepalive-кадру (tiles.TypeStill) — подія «зараз», не стан:
		// у кеш не йде (новому глядачу старі анонси дали б фальшиві кредити),
		// але й губитись за тайлами не має — інакше плеєр сховає тайли.
		broadcastStill(ns, raw)
		return
	}
	if m.Type == tiles.TypeKeep {
		repair := ns.tiles.remember(m, raw, tiles.KeepRects(raw[tiles.HeaderSize:]))
		broadcastKeep(ns, raw, repair)
		return
	}
	ns.tiles.remember(m, raw, nil)
	broadcastTiles(ns, raw, m.Type == tiles.TypeInvalidate)
}

// broadcastKeep — keep не губиться (як анонс still витісняє найстаріше).
// Глядач, що з минулого keep-а губив тайли (tilesLossy), одразу за keep-ом
// отримує перелічені тайли з кешу: інакше в його сховищі їх нема, а агент
// вважає їх утриманими й більше не шле.
func broadcastKeep(ns *nodeSession, raw []byte, repair [][]byte) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	for _, vl := range ns.viewers {
		if vl.tilesOut == nil {
			continue
		}
		lossy := vl.tilesLossy.Swap(false)
		enqueueStill(vl, raw)
		if lossy {
			for _, t := range repair {
				enqueueTiles(vl, t, false)
			}
		}
	}
}

func tileDropped(vl *viewerLeg) {
	atomic.AddUint64(&vl.tilesDropped, 1)
	vl.tilesLossy.Store(true)
}

// broadcastStill — як broadcastTiles, але повна черга віддає під анонс
// місце найстарішого повідомлення (тайл буде втрачено, анонс — ні).
func broadcastStill(ns *nodeSession, raw []byte) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	for _, vl := range ns.viewers {
		if vl.tilesOut != nil {
			enqueueStill(vl, raw)
		}
	}
}

func enqueueStill(vl *viewerLeg, raw []byte) {
	for i := 0; i < 2; i++ {
		select {
		case vl.tilesOut <- raw:
			return
		default:
		}
		select {
		case <-vl.tilesOut:
			tileDropped(vl)
		default:
		}
	}
	tileDropped(vl)
}

// broadcastTiles кладе повідомлення в черги всіх глядачів з відкритим каналом.
func broadcastTiles(ns *nodeSession, raw []byte, invalidate bool) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	for _, vl := range ns.viewers {
		if vl.tilesOut != nil {
			enqueueTiles(vl, raw, invalidate)
		}
	}
}

// enqueueTiles — без блокування. Повна черга: тайл викидаємо; invalidate
// спершу вичищає чергу (усе в ній однаково застаріле) і йде гарантовано.
func enqueueTiles(vl *viewerLeg, raw []byte, invalidate bool) {
	select {
	case vl.tilesOut <- raw:
		return
	default:
	}
	if !invalidate {
		tileDropped(vl)
		return
	}
	for {
		select {
		case <-vl.tilesOut:
			tileDropped(vl)
			continue
		default:
		}
		select {
		case vl.tilesOut <- raw:
			return
		default:
		}
	}
}

// viewerTilesHandler — обробник каналу тайлів viewer-ноги (лише під прапорцем).
// Глядач нічого не шле цим каналом; усе, що прийде, ігнорується.
func viewerTilesHandler(ns *nodeSession, vl *viewerLeg, dc *webrtc.DataChannel) {
	// Один канал тайлів на viewer-ногу. Без цього нога, відкривши N каналів
	// 'oosc-tiles', запускала б N помп (до ~65k горутин), кожна з яких
	// перезаписувала vl.tilesOut і висіла б до кінця ноги. Зайвий канал
	// закривається; помпа виходить і на закриття свого каналу.
	stop := make(chan struct{})
	var stopOnce sync.Once
	var q chan []byte
	dc.OnClose(func() {
		stopOnce.Do(func() { close(stop) })
		ns.mu.Lock()
		if q != nil && vl.tilesOut == q {
			vl.tilesOut = nil
		}
		ns.mu.Unlock()
	})
	dc.OnOpen(func() {
		ns.mu.Lock()
		if _, ok := ns.viewers[vl.pc]; !ok {
			ns.mu.Unlock()
			return
		}
		if vl.tilesOut != nil {
			ns.mu.Unlock()
			log.Printf("tiles: дубль каналу глядача [node=%s] — канал закрито", ns.nodeID)
			_ = dc.Close()
			return
		}
		q = make(chan []byte, tilesQueueDepth)
		vl.tilesOut = q
		// Знімок кешу — під ns.mu разом із появою черги: усе, що прийде
		// після, піде вже чергою (можливий дубль тайла — нешкідливий).
		snap := ns.tiles.snapshot()
		ns.mu.Unlock()
		log.Printf("tiles: viewer channel open [node=%s], replay %d", ns.nodeID, len(snap))
		go tilesPump(vl, dc, q, snap, stop)
	})
}

// tilesPumps — скільки помп живе зараз (для тестів і діагностики).
var tilesPumps atomic.Int64

// tilesPump шле спершу знімок кешу, далі чергу ноги — до закриття ноги або
// свого каналу (stop).
// Переповнений SCTP-буфер глядача: чекаємо (черга тим часом переповнюється й
// сама викидає тайли на вході), а не росте пам'ять.
func tilesPump(vl *viewerLeg, dc *webrtc.DataChannel, q chan []byte, snap [][]byte, stop <-chan struct{}) {
	tilesPumps.Add(1)
	defer tilesPumps.Add(-1)
	send := func(raw []byte) bool {
		for dc.BufferedAmount() > tilesMaxBuffered {
			select {
			case <-vl.done:
				return false
			case <-stop:
				return false
			case <-time.After(10 * time.Millisecond):
			}
		}
		if st := dc.ReadyState(); st != webrtc.DataChannelStateOpen {
			return false
		}
		if err := dc.Send(raw); err != nil {
			return false
		}
		atomic.AddUint64(&vl.tilesSent, 1)
		return true
	}
	for _, raw := range snap {
		if !send(raw) {
			return
		}
	}
	for {
		select {
		case <-vl.done:
			return
		case <-stop:
			return
		case raw := <-q:
			if !send(raw) {
				return
			}
		}
	}
}

// agentTilesGone — агентська нога ноди впала: стираємо кеш і тайли глядачів.
func agentTilesGone(ns *nodeSession) {
	if inv := ns.tiles.reset(); inv != nil {
		broadcastTiles(ns, inv, true)
	}
}
