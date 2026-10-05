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
	"context"
	"errors"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/organicoils/oo-screen/internal/autoupdate"
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

func relaunchSelf() {
	exe, err := os.Executable()
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

// hubReachableHealth: новий бінарь здоровий, якщо в межах вікна він дожив до
// мережі й достукався TCP до хаба. Це навмисно мінімальна перевірка (не
// гарантує, що капчер/енкодер працюють) — краш при старті ловить лічильник
// стартів у маркері (MaxStarts).
func hubReachableHealth(hub string) func(context.Context) error {
	return func(ctx context.Context) error {
		addr := hubHostPort(hub)
		if addr == "" {
			return errors.New("health: невідома адреса хаба")
		}
		var last error
		for {
			d := net.Dialer{Timeout: 5 * time.Second}
			c, err := d.DialContext(ctx, "tcp", addr)
			if err == nil {
				_ = c.Close()
				return nil
			}
			last = err
			select {
			case <-ctx.Done():
				return errors.Join(ctx.Err(), last)
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// newUpdater збирає Updater; nil, якщо оновлення неможливе (нема ключа/exe).
func newUpdater(manifestURL, node string) (*autoupdate.Updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	u := &autoupdate.Updater{ExePath: exe, NodeID: node, CurrentVersion: agentVersion,
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
// true = відкотились, викликач має перезапуститись і вийти.
func autoUpdateStartup(hub string, window time.Duration) bool {
	u, err := newUpdater("", "")
	if err != nil {
		return false
	}
	u.CleanupStale()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	res, err := u.Startup(ctx, hubReachableHealth(hub))
	switch res {
	case autoupdate.Committed:
		log.Printf("oo-agent: autoupdate: версія %s здорова — оновлення прийнято", agentVersion)
	case autoupdate.RolledBack:
		log.Printf("oo-agent: autoupdate: ВІДКАТ на попередній бінарь (%v)", err)
		return true
	default:
		if err != nil {
			log.Printf("oo-agent: autoupdate: маркер: %v", err)
		}
	}
	return false
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
			if !errors.Is(err, autoupdate.ErrNotNewer) && !errors.Is(err, autoupdate.ErrNotInRollout) {
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
