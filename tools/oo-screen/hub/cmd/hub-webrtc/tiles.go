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
	"log"
	"os"
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

// tilesCache — повідомлення поточного епізоду ноди: invalidate (якщо був) +
// тайли тієї ж епохи. Захищений власним mu, не ns.mu.
type tilesCache struct {
	mu      sync.Mutex
	epoch   uint32
	have    bool
	msgs    [][]byte
	bytes   int
	dropped atomic.Uint64 // відкинуто на вході від агента (невалідні/завеликі)
}

// remember кладе валідне повідомлення в кеш.
func (c *tilesCache) remember(m *tiles.Msg, raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.have || m.Epoch != c.epoch || m.Type == tiles.TypeInvalidate {
		c.epoch, c.have, c.msgs, c.bytes = m.Epoch, true, nil, 0
	}
	if m.Type == tiles.TypeInvalidate {
		c.msgs = append(c.msgs, raw)
		c.bytes = len(raw)
		return
	}
	if c.bytes+len(raw) > tilesCacheBytes {
		return
	}
	c.msgs = append(c.msgs, raw)
	c.bytes += len(raw)
}

func (c *tilesCache) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.msgs...)
}

// reset — агентська нога пішла: кеш недійсний. Повертає synthetic invalidate
// для глядачів (епоха +1 від останньої, щоб плеєр точно все стер).
func (c *tilesCache) reset() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	had := c.have
	c.epoch++
	c.have, c.msgs, c.bytes = false, nil, 0
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
	ns.tiles.remember(m, raw)
	broadcastTiles(ns, raw, m.Type == tiles.TypeInvalidate)
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
			atomic.AddUint64(&vl.tilesDropped, 1)
		default:
		}
	}
	atomic.AddUint64(&vl.tilesDropped, 1)
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
		atomic.AddUint64(&vl.tilesDropped, 1)
		return
	}
	for {
		select {
		case <-vl.tilesOut:
			atomic.AddUint64(&vl.tilesDropped, 1)
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
	dc.OnOpen(func() {
		q := make(chan []byte, tilesQueueDepth)
		ns.mu.Lock()
		if _, ok := ns.viewers[vl.pc]; !ok {
			ns.mu.Unlock()
			return
		}
		vl.tilesOut = q
		// Знімок кешу — під ns.mu разом із появою черги: усе, що прийде
		// після, піде вже чергою (можливий дубль тайла — нешкідливий).
		snap := ns.tiles.snapshot()
		ns.mu.Unlock()
		log.Printf("tiles: viewer channel open [node=%s], replay %d", ns.nodeID, len(snap))
		go tilesPump(vl, dc, q, snap)
	})
}

// tilesPump шле спершу знімок кешу, далі чергу ноги — до закриття ноги.
// Переповнений SCTP-буфер глядача: чекаємо (черга тим часом переповнюється й
// сама викидає тайли на вході), а не росте пам'ять.
func tilesPump(vl *viewerLeg, dc *webrtc.DataChannel, q chan []byte, snap [][]byte) {
	send := func(raw []byte) bool {
		for dc.BufferedAmount() > tilesMaxBuffered {
			select {
			case <-vl.done:
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
