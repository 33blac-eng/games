// nodereap.go — R5: прибирання нод, у яких давно немає ні агента, ні глядачів.
//
// Знайдено стиснутим soak-ом (hub/cmd/churnsoak): nodeSession створюється на
// першому offer ноди і досі зникав із реєстру лише через runtime-revoke або
// невдалий agent-offer. Агент, що просто пішов (ПК вимкнули, перевстановили,
// замінили на інший node_id), лишав ноду в реєстрі назавжди — разом із її
// ретрансляторами каналів (relays), станом бітрейту й лічильниками. Памʼяті
// на ноду небагато, але реєстр ріс з кожним новим node_id парку, і на стелі
// OO_SCREEN_MAX_NODES (500) хаб почав би ВІДМОВЛЯТИ новим агентам (503), хоча
// живих нод мав би одиниці.
//
// Жнець раз на nodeReapEvery проходить реєстр і знімає ноду, якщо вона
// простоює (agentPC == nil і немає жодного глядача) щонайменше nodeIdleGrace
// і стільки ж часу до неї не приходив offer (touched). Друга умова закриває
// вікно домовленості: agentPC стає непорожнім лише на Connected, а між
// answer-ом і Connected нода виглядає простою. Глядачам (ticket-режим) це
// нічого не змінює: нода без агента й так віддає 404 «no publisher».
package main

import (
	"context"
	"time"
)

var (
	// nodeIdleGrace — скільки нода має простоювати, щоб її прибрали. Агент,
	// що перепідключається за секунди, ноду не втрачає; 0 вимикає жнеця.
	nodeIdleGrace = envDuration("OO_SCREEN_NODE_IDLE_REAP", 2*time.Minute)
	nodeReapEvery = 30 * time.Second
)

// reaper памʼятає, з якого моменту кожна нода простоює.
type reaper struct {
	idleSince map[*nodeSession]time.Time
}

func newReaper() *reaper { return &reaper{idleSince: map[*nodeSession]time.Time{}} }

func nodeIdle(ns *nodeSession) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.agentPC == nil && len(ns.viewers) == 0
}

// sweep — один прохід; повертає, скільки нод знято.
func (rp *reaper) sweep(r *registry, now time.Time, grace time.Duration) int {
	seen := make(map[*nodeSession]struct{})
	var victims []*nodeSession
	for _, ns := range r.all() {
		seen[ns] = struct{}{}
		if !nodeIdle(ns) {
			delete(rp.idleSince, ns)
			continue
		}
		since, ok := rp.idleSince[ns]
		if !ok {
			rp.idleSince[ns] = now
			continue
		}
		if now.Sub(since) < grace || now.Sub(time.Unix(0, ns.touched.Load())) < grace {
			continue
		}
		victims = append(victims, ns)
	}
	for ns := range rp.idleSince {
		if _, ok := seen[ns]; !ok {
			delete(rp.idleSince, ns)
		}
	}
	n := 0
	for _, ns := range victims {
		// Перевірка й видалення під r.mu: getOrCreate теж бере r.mu, тож offer,
		// що прийшов між проходом і цим місцем, або вже оновив touched, або
		// прийде після видалення і створить нову ноду.
		r.mu.Lock()
		gone := r.nodes[ns.nodeID] == ns && nodeIdle(ns) &&
			now.Sub(time.Unix(0, ns.touched.Load())) >= grace
		if gone {
			delete(r.nodes, ns.nodeID)
		}
		r.mu.Unlock()
		delete(rp.idleSince, ns)
		if gone {
			forgetRelays(ns)
			n++
		}
	}
	return n
}

func reapIdleNodesLoop(ctx context.Context) {
	if nodeIdleGrace <= 0 {
		return
	}
	rp := newReaper()
	t := time.NewTicker(nodeReapEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			_ = rp.sweep(reg, now, nodeIdleGrace)
		}
	}
}
