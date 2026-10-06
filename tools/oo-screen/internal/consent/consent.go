// Package consent — S3: згода користувача ПК на сесію і індикатор
// «за вами дивляться».
//
// Рішення приймає ЛИШЕ агент на самому ПК. Хаб/глядач можуть тільки
// ПОПРОСИТИ (сигнал resume = «зʼявився глядач»), але жоден байт від них не
// відчиняє гейт: Gate пропускає resume до кадрового циклу тільки після
// Decide() (локальний діалог або політика адміна з прапорця агента), і поки
// згоди нема — Allowed()==false, тож агент відкидає ввід сам. Тобто обійти
// згоду з боку глядача нема чим: у протоколі просто нема поля «згода є».
package consent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Policy — як агент вирішує, чи пускати глядача.
type Policy int

const (
	// Off — стара поведінка (без запиту, без індикатора). Дефолт.
	Off Policy = iota
	// AlwaysAsk — кожна сесія лише після «Так» користувача ПК.
	AlwaysAsk
	// AskIfUserPresent — питати, якщо користувач за ПК (сесія не заблокована);
	// на заблокованому/порожньому ПК — пускати без запиту (обслуговування).
	AskIfUserPresent
	// Unattended — адмін дозволив без запиту; індикатор усе одно видно.
	Unattended
)

// ParsePolicy розбирає значення прапорця/env.
func ParsePolicy(s string) (Policy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off":
		return Off, nil
	case "always-ask", "always":
		return AlwaysAsk, nil
	case "ask-if-user-logged-in", "ask-if-present":
		return AskIfUserPresent, nil
	case "unattended-allowed-by-admin", "unattended":
		return Unattended, nil
	}
	return Off, fmt.Errorf("consent: unknown policy %q (off|always-ask|ask-if-user-logged-in|unattended-allowed-by-admin)", s)
}

func (p Policy) String() string {
	return [...]string{"off", "always-ask", "ask-if-user-logged-in", "unattended-allowed-by-admin"}[p]
}

// UI — платформна частина: діалог і індикатор.
type UI interface {
	// Ask показує діалог і блокує до відповіді/ctx. false на таймаут/помилку.
	Ask(ctx context.Context, text string) bool
	// ShowIndicator показує постійний індикатор; onEnd — кнопка «Завершити».
	ShowIndicator(onEnd func())
	// HideIndicator ховає його. Обидва ідемпотентні.
	HideIndicator()
}

// Config — налаштування Gate.
type Config struct {
	Policy      Policy
	UI          UI
	UserPresent func() bool   // nil = вважаємо присутнім (безпечніше: питаємо)
	Timeout     time.Duration // на відповідь; дефолт 30с, мовчання = відмова
	Cooldown    time.Duration // після відмови/«Завершити» resume ігнорується; дефолт 60с
	Text        string
	Logf        func(string, ...any)
	Now         func() time.Time
}

// Gate — стан згоди однієї агентської сесії.
type Gate struct {
	cfg Config

	mu        sync.Mutex
	granted   bool
	pending   context.CancelFunc
	gen       uint64
	blockedTo time.Time
	inner     func(bool)
	// want — чи є зараз глядач, що чекає (останній сигнал hub був resume).
	// Pause під час відкритого діалогу НЕ скасовує діалог (на Windows
	// MessageBoxTimeoutW не закрити ззовні — скасування лише плодило б нові
	// вікна на кожному циклі join/leave), а лише скидає want: відповідь «Так»
	// без глядача відкидається, повторний resume перевикористовує той самий
	// діалог. Отже в будь-який момент на екрані щонайбільше ОДИН запит.
	want bool
}

// New. Policy==Off → nil (Wrap/Allowed для nil — прозорі).
func New(cfg Config) *Gate {
	if cfg.Policy == Off {
		return nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 60 * time.Second
	}
	if cfg.Text == "" {
		cfg.Text = "Адміністратор хоче підключитися до вашого екрана (перегляд і, можливо, керування).\n\nДозволити?"
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Gate{cfg: cfg}
}

// Required — чи стартувати агенту в паузі (згоди ще нема).
func (g *Gate) Required() bool { return g != nil }

// Allowed — чи можна зараз приймати ввід / віддавати кадри.
func (g *Gate) Allowed() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.granted
}

// Wrap ставить Gate між сигналом хаба і кадровим гейтом агента.
func (g *Gate) Wrap(inner func(bool)) func(bool) {
	if g == nil || inner == nil {
		return inner
	}
	g.mu.Lock()
	g.inner = inner
	g.mu.Unlock()
	return g.onSignal
}

func (g *Gate) onSignal(resume bool) {
	if !resume {
		g.mu.Lock()
		if g.pending != nil {
			g.want = false
			if g.inner != nil {
				g.inner(false)
			}
			g.mu.Unlock()
			g.cfg.Logf("consent: viewer left while prompt open (prompt kept, answer will be ignored unless a viewer returns)")
			return
		}
		g.mu.Unlock()
		g.revoke("no viewers", 0)
		return
	}
	g.mu.Lock()
	g.want = true
	if g.granted {
		// Під локом: inner(true) не може обігнати конкурентний revoke.
		g.inner(true) // ідемпотентно; згода вже є
		g.mu.Unlock()
		return
	}
	if g.pending != nil {
		g.mu.Unlock()
		return // діалог уже на екрані — не плодимо другий
	}
	if g.cfg.Now().Before(g.blockedTo) {
		g.mu.Unlock()
		g.cfg.Logf("consent: resume ignored (cooldown after deny/end)")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.Timeout)
	g.pending = cancel
	g.gen++
	gen := g.gen
	g.mu.Unlock()
	go g.decide(ctx, cancel, gen)
}

func (g *Gate) decide(ctx context.Context, cancel context.CancelFunc, gen uint64) {
	defer cancel()
	ok, how := false, ""
	switch {
	case g.cfg.Policy == Unattended:
		ok, how = true, "unattended policy"
	case g.cfg.Policy == AskIfUserPresent && g.cfg.UserPresent != nil && !g.cfg.UserPresent():
		ok, how = true, "no user present"
	default:
		ok = g.cfg.UI.Ask(ctx, g.cfg.Text)
		how = "user answer"
		if ctx.Err() != nil && !ok {
			how = "timeout/cancelled"
		}
	}
	g.mu.Lock()
	if g.gen != gen || g.pending == nil {
		g.mu.Unlock() // глядач пішов, поки думали — рішення застаріле
		return
	}
	if !ok {
		g.pending = nil
		g.blockedTo = g.cfg.Now().Add(g.cfg.Cooldown)
		g.mu.Unlock()
		g.cfg.Logf("consent: DENIED (%s)", how)
		return
	}
	if !g.want {
		g.pending = nil
		g.mu.Unlock()
		g.cfg.Logf("consent: answer ignored — viewer left before it")
		return
	}
	// pending лишається виставленим, доки індикатор не показано: повторний
	// resume не запускає другий decide, а revoke (gen++) робить це рішення
	// застарілим.
	g.mu.Unlock()
	// Індикатор — ДО згоди: Allowed() (ввід) і кадри вмикаються лише коли
	// користувач уже бачить, що за ним дивляться.
	g.cfg.UI.ShowIndicator(func() { g.End() })
	g.mu.Lock()
	live := g.gen == gen && g.pending != nil && g.want // revoke чи pause міг встигнути між локами
	if g.gen == gen {
		g.pending = nil // рішення завершене (успішно чи ні)
	}
	if live {
		g.granted = true
		g.inner(true)
	}
	g.mu.Unlock()
	if !live {
		g.cfg.UI.HideIndicator()
		return
	}
	g.cfg.Logf("consent: granted (%s, policy=%s)", how, g.cfg.Policy)
}

// End — кнопка «Завершити сесію» на ПК: зупиняє кадри/ввід і на Cooldown
// ігнорує повторні resume.
func (g *Gate) End() {
	if g == nil {
		return
	}
	g.revoke("ended by user", g.cfg.Cooldown)
}

// ViewerJoin — hub повідомив, що підключився ЩЕ ОДИН глядач (сигнал
// "viewer-join", шлеться на КОЖНУ нову ногу, а не лише на перехід 0->1).
// Згода, дана для попереднього глядача, на нового не поширюється: при
// AlwaysAsk/AskIfUserPresent гейт закривається (кадри й ввід стоп) і
// користувача ПК питають знову. Unattended — лише лог (індикатор уже видно).
// Якщо згоди ще нема — нічого: відкритий/наступний запит покриває і цього.
func (g *Gate) ViewerJoin() {
	if g == nil {
		return
	}
	g.mu.Lock()
	granted := g.granted
	g.mu.Unlock()
	if !granted {
		return
	}
	if g.cfg.Policy == Unattended {
		g.cfg.Logf("consent: another viewer joined (unattended policy, no prompt)")
		return
	}
	g.revoke("another viewer joined — re-asking", 0)
	g.onSignal(true)
}

// Reset — обрив звʼязку з хабом: згода не переживає реконект (на тому боці
// може бути вже інший глядач).
func (g *Gate) Reset() {
	if g == nil {
		return
	}
	g.revoke("hub reconnect", 0)
}

func (g *Gate) revoke(why string, block time.Duration) {
	g.mu.Lock()
	was := g.granted
	g.granted = false
	g.want = false
	g.gen++
	if g.pending != nil {
		g.pending()
		g.pending = nil
	}
	if block > 0 {
		g.blockedTo = g.cfg.Now().Add(block)
	}
	if g.inner != nil {
		g.inner(false) // під локом: впорядковано з inner(true)
	}
	g.mu.Unlock()
	g.cfg.UI.HideIndicator()
	if was {
		g.cfg.Logf("consent: session stopped (%s)", why)
	}
}
