// Package hub — реле кадрів агент -> глядач з політикою
// drop-oldest-until-next-IDR при переповненні буфера. Використовується
// hub-wt (кандидат B, WebTransport).
package hub

import (
	"log"
	"sync"

	"github.com/organicoils/oo-screen/internal/envelope"
)

// RelayBuf — обмежений буфер кадрів між агентом і одним глядачем.
// Переповнення: викидаємо найстаріші не-keyframe кадри, поки не звільниться
// місце (drop-oldest-until-next-IDR) — глядач завжди отримає базу з IDR.
type RelayBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	q      []*envelope.Frame
	maxLen int
	closed bool
	drops  int
}

func NewRelayBuf(maxLen int) *RelayBuf {
	r := &RelayBuf{maxLen: maxLen}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// Push додає кадр агента до черги; при переповненні викидає ПРЕФІКС черги
// до наступного keyframe (drop-until-IDR), а не поодинокі дельти —
// поодинокий дроп ізольованої дельти лишає залежні пізніші P-кадри без
// їхньої бази, і черга ніколи не починається з декодабельного GOP.
func (r *RelayBuf) Push(f *envelope.Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.q = append(r.q, f)
	for len(r.q) > r.maxLen {
		// шукаємо наступний keyframe, ПОЧИНАЮЧИ З ІНДЕКСУ 1: якщо він є,
		// весь префікс [0:idx) — старий незавершений GOP, що однаково
		// недекодабельний без своєї бази, тож викидаємо його цілком і
		// черга починається рівно з IDR. Якщо keyframe у хвості нема,
		// лишаємо тільки найновіший кадр (як TrimToLatestKeyframe) —
		// глядач все одно чекатиме наступний IDR.
		idx := -1
		for i := 1; i < len(r.q); i++ {
			if r.q[i].Keyframe() {
				idx = i
				break
			}
		}
		if idx < 0 {
			idx = len(r.q) - 1 // немає IDR у хвості — лишаємо лише найновіший кадр
		}
		if idx == 0 {
			break // черга вже починається з keyframe в межах maxLen
		}
		r.drops += idx
		log.Printf("hub-wt: relay overflow, dropped prefix of %d frames up to seq=%d (total drops=%d)",
			idx, r.q[idx].FrameSeq, r.drops)
		r.q = r.q[idx:]
	}
	r.cond.Signal()
}

// Pop блокується, доки не з'явиться кадр або буфер не закриється.
func (r *RelayBuf) Pop() (*envelope.Frame, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.q) == 0 && !r.closed {
		r.cond.Wait()
	}
	if len(r.q) == 0 {
		return nil, false
	}
	f := r.q[0]
	r.q = r.q[1:]
	return f, true
}

// TrimToLatestKeyframe зрізає чергу до ОСТАННЬОГО keyframe включно —
// кличеться при під'єднанні глядача, щоб він стартував зі свіжого IDR,
// а не жував бэклог (заміряно: без цього a2r p50 468мс замість 4мс).
// Якщо keyframe у черзі нема — черга чиститься повністю (глядач дочекається
// наступного IDR, корпус має IDR/2с).
func (r *RelayBuf) TrimToLatestKeyframe() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.q) - 1; i >= 0; i-- {
		if r.q[i].Keyframe() {
			if i > 0 {
				r.drops += i
				log.Printf("hub-wt: viewer join, trimmed %d backlog frames to latest IDR seq=%d", i, r.q[i].FrameSeq)
			}
			r.q = r.q[i:]
			return
		}
	}
	if n := len(r.q); n > 0 {
		r.drops += n
		log.Printf("hub-wt: viewer join, no IDR in backlog, cleared %d frames", n)
		r.q = nil
	}
}

// Empty повідомляє, чи буфер порожній (немає жодного кадру, отже — і
// keyframe). Кличеться після TrimToLatestKeyframe при приєднанні глядача,
// щоб зрозуміти, чи треба напряму просити агента про свіжий IDR.
func (r *RelayBuf) Empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.q) == 0
}

func (r *RelayBuf) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cond.Broadcast()
}
