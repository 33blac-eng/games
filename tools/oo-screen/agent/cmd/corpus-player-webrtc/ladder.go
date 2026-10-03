package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/organicoils/oo-screen/internal/control"
	"github.com/organicoils/oo-screen/internal/h264"
)

// offerBitrate — поле "bitrate" в offer агента: стеля контролера хаба. 0 = не слати.
var offerBitrate uint64

type ladderLevel struct {
	bps uint64
	aus []h264.AU
}

// ladder — емуляція енкодера, який виконує накази хаба: bitrate_target обирає
// найвищий щабель <= цілі, а перехід і keyframe_request/PLI починають щабель
// з кадру 0 (IDR). Перемикання між щаблями — лише на IDR, тож потік лишається
// валідним H.264.
type ladder struct {
	mu      sync.Mutex
	levels  []ladderLevel // за зростанням bps
	cur     int
	pos     int
	pending bool // з наступного кадру — кадр 0 поточного щабля

	// aligned — режим «живий енкодер» (OO_CORPUS_LADDER_ALIGNED=1): усі щаблі —
	// той самий контент з однаковою довжиною й GOP, тож кадр k у кожному файлі
	// показує той самий момент. bitrate_target лише запамʼятовує want, а
	// перемикання стається на НАСТУПНОМУ ПРИРОДНОМУ IDR щабля, без додаткового
	// IDR і без стрибка контенту — так поводиться справжній агент після P0
	// (SetBitrate без ForceIDR). Відмінність від MFT: той міняє QP з
	// наступного кадру, тут ціль діє із запізненням до одного GOP (2 с).
	aligned bool
	want    int
}

// loadLadder: "500000=a.h264,8000000=b.h264".
func loadLadder(spec string) (*ladder, error) {
	l := &ladder{}
	for _, part := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, fmt.Errorf("щабель %q: треба bps=шлях", part)
		}
		bps, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			return nil, err
		}
		l.levels = append(l.levels, ladderLevel{bps, loadAUsFrom(v)})
	}
	if len(l.levels) == 0 {
		return nil, fmt.Errorf("порожня драбина")
	}
	sort.Slice(l.levels, func(i, j int) bool { return l.levels[i].bps < l.levels[j].bps })
	l.cur = len(l.levels) - 1
	l.want = l.cur
	return l, nil
}

// setAligned вмикає режим природних IDR; щаблі мусять мати однакову довжину.
func (l *ladder) setAligned() error {
	n := len(l.levels[0].aus)
	for _, lv := range l.levels {
		if len(lv.aus) != n {
			return fmt.Errorf("aligned: щабель %d має %d AU, а не %d", lv.bps, len(lv.aus), n)
		}
	}
	l.aligned = true
	return nil
}

func (l *ladder) pick(bps uint64) int {
	idx := 0
	for i, lv := range l.levels {
		if lv.bps <= bps {
			idx = i
		}
	}
	return idx
}

func (l *ladder) next() []byte { return l.nextAU().Data }

// nextAU — як next, але з прапорцем IDR (для пейсера: кредит на keyframe).
func (l *ladder) nextAU() h264.AU {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending {
		l.pos, l.pending = 0, false
		if l.aligned {
			l.cur = l.want // явний IDR — заодно й нова ціль
		}
	}
	if l.aligned && l.want != l.cur {
		if wa := l.levels[l.want].aus; wa[l.pos%len(wa)].Keyframe {
			l.cur = l.want
		}
	}
	aus := l.levels[l.cur].aus
	au := aus[l.pos%len(aus)]
	l.pos++
	return au
}

// levelBps — бітрейт поточного щабля (ціль «енкодера» для пейсера).
func (l *ladder) levelBps() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.levels[l.cur].bps
}

func (l *ladder) requestIDR() {
	l.mu.Lock()
	l.pending = true
	l.mu.Unlock()
}

func (l *ladder) setTarget(bps uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.pick(bps)
	if l.aligned {
		l.want = n
		log.Printf("ctl: bitrate_target %d -> ladder %d (на природному IDR)", bps, l.levels[n].bps)
		return
	}
	if n != l.cur {
		l.cur, l.pending = n, true
	}
	log.Printf("ctl: bitrate_target %d -> ladder %d", bps, l.levels[l.cur].bps)
}

func (l *ladder) onCtl(data []byte) {
	var m control.Msg
	if json.Unmarshal(data, &m) != nil {
		return // "pause"/"resume" — текстові, ігноруємо: корпус шле завжди
	}
	switch m.Type {
	case control.TypeBitrateTarget:
		l.setTarget(m.BitrateBps)
	case control.TypeKeyframeRequest:
		log.Printf("ctl: keyframe_request -> IDR")
		l.requestIDR()
	}
}
