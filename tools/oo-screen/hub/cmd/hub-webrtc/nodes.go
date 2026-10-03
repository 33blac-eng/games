package main

import (
	"crypto/subtle"
	"net/http"
	"sort"
)

// readyNodes — знімок node_id, у яких ПРЯМО ЗАРАЗ є живий publisher.
//
// Рахується тим самим ns.hasAgent(), яким authorizeViewer вирішує 404 «no
// publisher for node». Це навмисно: якби перелік для консолі мав власне джерело
// правди, кнопка «OO» брехала б рівно там, де це найдорожче — людина тисне,
// а хаб відмовляє.
//
// Лок реєстру тримається лише на копіювання вказівників; лок КОЖНОЇ ноди
// (hasAgent) береться вже поза ним — той самий порядок, що в nodesForUser.
func (r *registry) readyNodes() []string {
	r.mu.Lock()
	snapshot := make([]*nodeSession, 0, len(r.nodes))
	for _, ns := range r.nodes {
		snapshot = append(snapshot, ns)
	}
	r.mu.Unlock()

	out := make([]string, 0, len(snapshot))
	for _, ns := range snapshot {
		// Порожній node — T1/бенч-агент без -node: до нього все одно не
		// підключиться жоден viewer (fail-closed у authorizeViewer), тож у
		// переліку для консолі він був би рядком, який нічого не вмикає.
		if ns.nodeID == "" || !ns.hasAgent() {
			continue
		}
		out = append(out, ns.nodeID)
	}
	sort.Strings(out)
	return out
}

type nodesResp struct {
	Nodes []string `json:"nodes"`
}

// handleNodes — GET /nodes: які ПК зараз готові віддавати ВЛАСНИЙ потік. Лише
// читання, жодної мутації.
//
// СЕКРЕТ. Перелік node_id — це інвентар парку власника, а хаб дивиться у
// відкритий інтернет. Гейт — той самий спільний X-OO-Hub-Key, яким хаб уже
// ходить в ЕРП (ConsumeTicket, SubscribeRevoke): ключ уже налаштований з обох
// боків (OO_SCREEN_HUB_KEY у хаба, remote_access.screen.hub_shared_key в ЕРП),
// і другий механізм тут заводити нема з чого.
//
// Агентський OO_SCREEN_T1_TOKEN на цю роль НЕ годиться: у нього публічний
// передбачуваний дефолт "t1-dev-token", тож хаб, піднятий без env, роздавав би
// інвентар кожному охочому. Порожній hubKey = маршрут вимкнено (403), а не
// «пускати всіх»: ключ, якого нема, не збігається ні з чим.
func handleNodes(w http.ResponseWriter, r *http.Request) {
	corsHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	given := r.Header.Get("X-OO-Hub-Key")
	if hubKey == "" || subtle.ConstantTimeCompare([]byte(given), []byte(hubKey)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, nodesResp{Nodes: reg.readyNodes()})
}
