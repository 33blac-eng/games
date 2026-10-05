package main

// S6: автооновлення агента (internal/autoupdate). ТИПОВО ВИМКНЕНО: потрібні
// і прапорець -auto-update-url, і публічний ключ, зашитий при збірці
// (-ldflags "-X main.updatePubKey=<hex>"). Без ключа агент маніфестам не вірить
// взагалі — ключ не можна підмінити ні прапорцем, ні env.
//
// Перевірку «чи здоровий новий бінарь» (autoupdate.Startup) агент робить на
// КОЖНОМУ старті незалежно від прапорця: маркер з'являється лише після swap,
// тож без оновлення це один os.Stat.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/organicoils/oo-screen/internal/autoupdate"
	"github.com/organicoils/oo-screen/internal/rollout"
)

var (
	// updatePubKey — ed25519 публічний ключ (hex/base64), пінований при збірці.
	updatePubKey string
	// agentVersion — semver для порівняння з маніфестом (build.sh: AGENT_VERSION).
	agentVersion = "0.0.0"
)

// relaunchAfterExit: після swap агент гасить ctx, main відпускає м'ютекс
// одного екземпляра і лише тоді (defer) запускає новий бінарь з тими ж args.
var relaunchAfterExit atomic.Bool

// startExe — шлях бінаря, знятий ДО будь-якого swap/rollback. os.Executable()
// на Linux читає /proc/self/exe, який іде за перейменуванням: після Swap
// запущений файл уже <exe>.old, після Rollback — <exe>.bad. Тому і
// перезапуск, і Updater.ExePath беруть цей шлях, а не os.Executable() пізніше.
var startExe, startExeErr = os.Executable()

// agentConnected закривається, коли агент уперше підключився до хаба
// (капчер/енкодер ініціалізовані, транспорт пройшов рукостискання).
var (
	agentConnected     = make(chan struct{})
	agentConnectedOnce sync.Once
)

func markAgentConnected() { agentConnectedOnce.Do(func() { close(agentConnected) }) }

func relaunchSelf() {
	exe, err := startExe, startExeErr
	if err != nil {
		log.Printf("oo-agent: autoupdate: перезапуск неможливий: %v", err)
		return
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	if err := cmd.Start(); err != nil {
		log.Printf("oo-agent: autoupdate: перезапуск %s: %v", exe, err)
		return
	}
	_ = cmd.Process.Release()
}

// hubHostPort дістає host:port з -hub (URL для webrtc, host:port для wt).
func hubHostPort(hub string) string {
	if strings.Contains(hub, "://") {
		u, err := url.Parse(hub)
		if err != nil {
			return ""
		}
		if u.Port() != "" {
			return u.Host
		}
		if u.Scheme == "https" {
			return net.JoinHostPort(u.Hostname(), "443")
		}
		return net.JoinHostPort(u.Hostname(), "80")
	}
	return hub
}

// sessionHealth: новий бінарь здоровий, якщо в межах вікна САМ агент
// підключився до хаба (connected закрито після ініціалізації капчера/енкодера
// і рукостискання транспорту). Якщо не встиг — дивимось, чи хаб узагалі
// досяжний по TCP: недосяжний хаб/мережа = ErrInconclusive (не відкат, старт
// не рахується), досяжний хаб без сесії = зламаний бінарь -> відкат.
// Краш при старті ловить лічильник стартів у маркері (MaxStarts).
func sessionHealth(hub string, connected <-chan struct{}) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-connected:
			return nil
		case <-ctx.Done():
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			// агент гаситься (Ctrl+C/logoff) до кінця вікна — це не вирок
			return errors.Join(autoupdate.ErrInconclusive, ctx.Err())
		}
		addr := hubHostPort(hub)
		if addr == "" {
			return errors.New("health: невідома адреса хаба")
		}
		d := net.Dialer{Timeout: 5 * time.Second}
		c, err := d.DialContext(context.Background(), "tcp", addr)
		if err != nil {
			return errors.Join(autoupdate.ErrInconclusive, err)
		}
		_ = c.Close()
		return errors.New("health: хаб досяжний, але агент не підключився за вікно")
	}
}

// rolloutID — ключ кошика викатки й node у звіті здоров'я. Mesh-нода має
// -node; legacy T1-агент (без -node) інакше мав би порожній ключ: усі такі
// агенти падали б в один кошик Bucket("", v), а звіти з node="" хаб
// відкидає (400) — відкати T1-флоту не доходили б до halt-гейта. Тому для
// них беремо стабільне "host:<hostname>"; "" лише якщо й hostname невідомий.
func rolloutID(node string) string {
	if node != "" {
		return node
	}
	return hostRolloutID(os.Hostname)
}

func hostRolloutID(hostname func() (string, error)) string {
	h, err := hostname()
	if err != nil || h == "" {
		return ""
	}
	return "host:" + strings.ToLower(h)
}

// newUpdater збирає Updater; nil, якщо оновлення неможливе (нема ключа/exe).
func newUpdater(manifestURL, node string) (*autoupdate.Updater, error) {
	if startExeErr != nil {
		return nil, startExeErr
	}
	u := &autoupdate.Updater{ExePath: startExe, NodeID: rolloutID(node), CurrentVersion: agentVersion,
		ManifestURL: manifestURL, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	if manifestURL != "" {
		pk, err := autoupdate.ParsePublicKey(updatePubKey)
		if err != nil {
			return nil, err
		}
		u.PubKey = pk
	}
	return u, nil
}

// autoUpdateStartup: якщо щойно відбувся swap — перевірити здоров'я.
// Викликається ПІСЛЯ м'ютекса одного екземпляра (два Startup не йдуть разом)
// у горутині: агент працює під час вікна, а health чекає саме на його
// підключення. Відкат: relaunchAfterExit + stop(); main перезапускає вже
// відновлений старий бінарь після release м'ютекса.
func autoUpdateStartup(ctx context.Context, hub string, window time.Duration, stop func(), reportURL, node string) {
	u, err := newUpdater("", "")
	if err != nil {
		return
	}
	u.CleanupStale()
	hctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	res, err := u.Startup(hctx, sessionHealth(hub, agentConnected))
	if r := startupVerdict(res); r != "" && reportURL != "" {
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		if rerr := postHealthReport(rctx, http.DefaultClient, reportURL, os.Getenv("OO_ROLLOUT_REPORT_TOKEN"),
			rollout.Report{Node: rolloutID(node), Version: agentVersion, Result: r, At: time.Now().UTC()}); rerr != nil {
			log.Printf("oo-agent: autoupdate: звіт викатки: %v", rerr)
		}
		rcancel()
	}
	switch res {
	case autoupdate.Committed:
		log.Printf("oo-agent: autoupdate: версія %s здорова — оновлення прийнято", agentVersion)
	case autoupdate.RolledBack:
		log.Printf("oo-agent: autoupdate: ВІДКАТ на попередній бінарь, версію %s заборонено (%v)", agentVersion, err)
		relaunchAfterExit.Store(true)
		stop()
	case autoupdate.RollbackFailed:
		log.Printf("oo-agent: autoupdate: УВАГА: версія %s нездорова, але відкат неможливий — працюю на ній, версію заборонено: %v", agentVersion, err)
	case autoupdate.Inconclusive:
		log.Printf("oo-agent: autoupdate: здоров'я версії %s не визначено, рішення на наступному старті: %v", agentVersion, err)
	default:
		if err != nil {
			log.Printf("oo-agent: autoupdate: маркер: %v", err)
		}
	}
}

// startupVerdict: результат Startup -> вердикт для O4-викатки ("" = не було
// оновлення, звітувати нічого).
func startupVerdict(res autoupdate.StartupResult) string {
	switch res {
	case autoupdate.Committed:
		return rollout.ResultOK
	case autoupdate.RolledBack, autoupdate.RollbackFailed:
		return rollout.ResultFail
	case autoupdate.Inconclusive:
		return rollout.ResultInconclusive
	}
	return ""
}

// postHealthReport: best-effort POST вердикту на oo-rollout serve.
func postHealthReport(ctx context.Context, c *http.Client, url, token string, r rollout.Report) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// runAutoUpdate — фоновий цикл перевірки маніфесту. Після успішного swap
// ставить relaunchAfterExit і гасить агент через stop.
func runAutoUpdate(ctx context.Context, u *autoupdate.Updater, every time.Duration, stop func()) {
	first := time.Minute
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(every)
		m, a, err := u.Check(ctx)
		if err != nil {
			if !errors.Is(err, autoupdate.ErrNotNewer) && !errors.Is(err, autoupdate.ErrNotInRollout) &&
				!errors.Is(err, autoupdate.ErrUpdatePending) && !errors.Is(err, autoupdate.ErrDenied) {
				log.Printf("oo-agent: autoupdate: check: %v", err)
			}
			continue
		}
		if err := u.Stage(ctx, a); err != nil {
			log.Printf("oo-agent: autoupdate: download %s: %v", m.Version, err)
			continue
		}
		if err := u.Swap(m.Version); err != nil {
			log.Printf("oo-agent: autoupdate: swap %s: %v", m.Version, err)
			continue
		}
		log.Printf("oo-agent: autoupdate: %s -> %s встановлено, перезапуск", agentVersion, m.Version)
		relaunchAfterExit.Store(true)
		stop()
		return
	}
}
