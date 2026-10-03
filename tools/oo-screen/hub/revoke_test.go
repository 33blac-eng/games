package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSubscribeRevoke_CallsOnRevoke — httptest ERP-стаб віддає один запис
// {"node_id": "n1"} на перший запит і порожній масив на всі наступні;
// перевіряємо, що SubscribeRevoke викликає onRevoke("node", "n1") рівно
// один раз, з правильним X-OO-Hub-Key заголовком.
func TestSubscribeRevoke_CallsOnRevoke(t *testing.T) {
	// atomic, а не голі локальні: обробник httptest біжить у СВОЇЙ горутині на
	// кожен запит, а поллінг з інтервалом 10 мс легко дає два запити внахлест.
	// Голий `served` тут не лише червонив -race — він ще й пускав обидва запити
	// у гілку "перший", і тоді onRevoke кликався двічі при справному коді.
	var served atomic.Int32
	var gotKey atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("X-OO-Hub-Key"))
		w.Header().Set("Content-Type", "application/json")
		if served.CompareAndSwap(0, 1) {
			_ = json.NewEncoder(w).Encode([]revocationEntry{
				{NodeID: "n1", At: time.Now().UnixMilli()},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]revocationEntry{})
	}))
	defer srv.Close()

	var mu sync.Mutex
	var calls []string

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go SubscribeRevoke(ctx, srv.URL, "secret-key", 10*time.Millisecond, func(kind, val string) {
		mu.Lock()
		calls = append(calls, kind+":"+val)
		mu.Unlock()
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("onRevoke calls = %v, want exactly 1", calls)
	}
	if calls[0] != "node:n1" {
		t.Errorf("onRevoke call = %q, want %q", calls[0], "node:n1")
	}
	if k, _ := gotKey.Load().(string); k != "secret-key" {
		t.Errorf("X-OO-Hub-Key = %q, want secret-key", k)
	}
}

// TestSubscribeRevoke_UserKind — заповнене поле user_id -> onRevoke("user", ...).
func TestSubscribeRevoke_UserKind(t *testing.T) {
	var served atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if served.CompareAndSwap(0, 1) {
			_ = json.NewEncoder(w).Encode([]revocationEntry{
				{UserID: "u42", At: time.Now().UnixMilli()},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]revocationEntry{})
	}))
	defer srv.Close()

	var mu sync.Mutex
	var calls []string

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go SubscribeRevoke(ctx, srv.URL, "secret-key", 10*time.Millisecond, func(kind, val string) {
		mu.Lock()
		calls = append(calls, kind+":"+val)
		mu.Unlock()
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "user:u42" {
		t.Fatalf("onRevoke calls = %v, want [user:u42]", calls)
	}
}

// TestSubscribeRevoke_HTTPErrorDoesNotKillLoop — ERP-стаб завжди 500;
// поллінг має продовжувати без паніки/зависання (fail-soft).
func TestSubscribeRevoke_HTTPErrorDoesNotKillLoop(t *testing.T) {
	// Лічильник атомарний: handler живе в горутинах httptest-сервера, а тест
	// читає його зі своєї. Замість сну на «мабуть устигне» — чекаємо події:
	// другий запит ПІСЛЯ 500 і є доказом, що цикл пережив помилку.
	var hits atomic.Int32
	second := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 2 {
			close(second)
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		SubscribeRevoke(ctx, srv.URL, "secret-key", 5*time.Millisecond, func(kind, val string) {})
		close(stopped)
	}()

	select {
	case <-second:
	case <-time.After(10 * time.Second):
		t.Fatalf("expected loop to keep polling despite 500s, got %d hits", hits.Load())
	}
	cancel()
	<-stopped
}

// ── staleGate: «м'яко спочатку, твердо згодом» ──────────────────────────────
//
// Час КЕРОВАНИЙ: staleGate.observe бере now параметром, тому епізод
// недоступності на годину програється миттєво, без жодного time.Sleep. Реальний
// сон тут був би не просто повільним — він робив би тест флакі (планувальник,
// GC), а перевіряємо ми арифметику порога, а не годинник.

// newTestGate — гейт із порогом after і збирачем викликів onRevoke.
func newTestGate(after time.Duration, calls *[]string) *staleGate {
	return &staleGate{
		after:    after,
		url:      "http://erp.test",
		onRevoke: func(kind, val string) { *calls = append(*calls, kind+":"+val) },
	}
}

// t0 — фіксована точка відліку керованого часу.
var t0 = time.Date(2026, 8, 30, 1, 45, 0, 0, time.UTC)

// (а) Коротка недоступність — сесії ЖИВІ. ERP лежить 60 с при порозі 90 с: це
// звичайне вікно викочування, рвати колегам сесії через нього неприпустимо.
func TestStaleGate_ShortOutageKeepsSessions(t *testing.T) {
	var calls []string
	g := newTestGate(90*time.Second, &calls)

	g.observe(t0, true) // звʼязок був
	for s := 2; s <= 60; s += 2 {
		g.observe(t0.Add(time.Duration(s)*time.Second), false)
	}

	if len(calls) != 0 {
		t.Fatalf("коротка недоступність (60с < 90с) обірвала сесії: %v", calls)
	}
}

// (а, друга половина) Курсор since при помилці НЕ рухається, і опитування
// позначається як невдале — це рівно те, чим живиться staleGate.
func TestPollRevocationsOnce_ErrorKeepsCursorAndReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	const since = int64(1756500000000)
	got, ok := pollRevocationsOnce(context.Background(), srv.Client(), srv.URL, "k", since, func(kind, val string) {
		t.Fatalf("onRevoke не мав викликатись на 500, отримав %s:%s", kind, val)
	})
	if got != since {
		t.Errorf("курсор зрушив на помилці: since %d -> %d", since, got)
	}
	if ok {
		t.Error("опитування з 500 позначене як успішне")
	}
}

// (б) Недоступність довша за поріг — обрив РІВНО ОДИН раз, і повторні тіки не
// обривають знову по колу (інакше кожні 2 с ішов би новий залп closeNode).
func TestStaleGate_FiresExactlyOnceAfterThreshold(t *testing.T) {
	var calls []string
	g := newTestGate(90*time.Second, &calls)

	g.observe(t0, true)
	g.observe(t0.Add(89*time.Second), false)
	if len(calls) != 0 {
		t.Fatalf("обірвало ДО порога (89с < 90с): %v", calls)
	}

	g.observe(t0.Add(90*time.Second), false)
	if len(calls) != 1 {
		t.Fatalf("на порозі обривів = %d, треба рівно 1 (%v)", len(calls), calls)
	}
	if calls[0] != RevokeKindStale+":1m30s" {
		t.Errorf("виклик = %q, треба %q", calls[0], RevokeKindStale+":1m30s")
	}

	for s := 92; s <= 600; s += 2 { // ще 8,5 хвилин мовчання
		g.observe(t0.Add(time.Duration(s)*time.Second), false)
	}
	if len(calls) != 1 {
		t.Fatalf("повторні тіки обривають по колу: %d викликів (%v)", len(calls), calls)
	}
}

// (в) Свіжий старт хаба БЕЗ жодного успішного опитування не обриває нікого.
// «Ніколи не було успіху» != «успіх був давно»: у першому випадку застарілого
// дозволу не існує, і обрив тут клав би всіх після кожного перезапуску хаба.
func TestStaleGate_FreshStartNeverFires(t *testing.T) {
	var calls []string
	g := newTestGate(90*time.Second, &calls)

	for s := 2; s <= 3600; s += 2 { // година недоступності з самого старту
		g.observe(t0.Add(time.Duration(s)*time.Second), false)
	}

	if len(calls) != 0 {
		t.Fatalf("свіжий старт без жодного успіху обірвав сесії: %v", calls)
	}
}

// (г) Після відновлення звʼязку все працює далі БЕЗ перезапуску хаба: гейт
// знову терпить коротку недоступність і знову спрацьовує на наступній довгій.
func TestStaleGate_RecoveryRestoresNormalBehaviour(t *testing.T) {
	var calls []string
	g := newTestGate(90*time.Second, &calls)

	g.observe(t0, true)
	g.observe(t0.Add(120*time.Second), false) // епізод 1: обрив
	if len(calls) != 1 {
		t.Fatalf("епізод 1: обривів = %d, треба 1 (%v)", len(calls), calls)
	}

	g.observe(t0.Add(180*time.Second), true) // ERP ожив
	g.observe(t0.Add(240*time.Second), false)
	if len(calls) != 1 {
		t.Fatalf("після відновлення коротка недоступність (60с) обірвала: %v", calls)
	}

	g.observe(t0.Add(275*time.Second), false) // 95с від успіху на 180-й секунді
	if len(calls) != 2 {
		t.Fatalf("епізод 2: обривів = %d, треба 2 (%v)", len(calls), calls)
	}
	if calls[1] != RevokeKindStale+":1m35s" {
		t.Errorf("другий виклик = %q, треба %q", calls[1], RevokeKindStale+":1m35s")
	}
}

// Поріг з оточення: валідний перекриває дефолт, сміття й нуль рубильник НЕ
// вимикають (мовчазний fail-open через друкарську помилку в env неприйнятний).
func TestStaleAfterFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", defaultStaleAfter},
		{"45s", 45 * time.Second},
		{"2m", 2 * time.Minute},
		{"0s", defaultStaleAfter},
		{"-1m", defaultStaleAfter},
		{"хвилинку", defaultStaleAfter},
	} {
		t.Setenv(envStaleAfter, tc.env)
		if got := staleAfterFromEnv(); got != tc.want {
			t.Errorf("%s=%q -> %s, треба %s", envStaleAfter, tc.env, got, tc.want)
		}
	}
}
