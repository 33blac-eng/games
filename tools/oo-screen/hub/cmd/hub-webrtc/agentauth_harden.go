// agentauth_harden.go — S1, хвиля 8: безпечні налаштування перед увімкненням
// strict на проді і відкликання ОДНІЄЇ ноди без ротації master.
//
// Що тут (усе — лише додаткові відмови/попередження; без нових env поведінка
// хаба та сама, крім WARNING у журналі старту):
//
//   - СЛАБКИЙ MASTER. Токен ноди = HMAC(master, node_id), тож master — єдине,
//     що треба вгадати, щоб виковувати токени будь-якої ноди. Коротший за
//     minAgentMasterLen (32 символи; `oo-node-token -gen-secret` дає 64 hex):
//     у strict хаб не стартує, інакше — WARNING на старті.
//   - ПРАВА ФАЙЛУ MASTER. OO_SCREEN_AGENT_SECRET_FILE, який може читати група
//     чи всі (mode & 0o077 != 0): у strict хаб не стартує (виправлення —
//     `chmod 600`), інакше — WARNING. На Windows біти прав нічого не кажуть —
//     перевірка пропускається.
//   - MASTER У ENV у strict — WARNING: видно в /proc/<pid>/environ і юніт-файлі;
//     на проді — файлом.
//   - _PREV == поточний master — WARNING (ротація, якої нема; прибрати _PREV).
//   - ВІДКЛИКАННЯ НОДИ. OO_SCREEN_AGENT_REVOKED_FILE — список node_id (по одному
//     на рядок, # — коментар), агентам яких хаб відмовляє незалежно від
//     токена (токен ноди поточного чи старого master, спільний легасі-токен),
//     на /offer/agent, P2P-полі агента і для потоків додаткових моніторів
//     (`<node>#m<i>` авторизуються базовою нодою). Для вкраденого/списаного
//     ПК: master і токени решти парку не чіпаються. Токен ноди детермінований
//     (той самий node_id = той самий токен), тож повернути ПК у парк = новий
//     node_id (або прибрати рядок, якщо ПК повернуто довіреним).
//     Файл перечитується на зміну (mtime/розмір) не частіше revokedRecheck —
//     без перезапуску хаба. Fail closed на старті: файл задано, але він не
//     читається або має недопустимий node_id — хаб не стартує. Під час роботи
//     файл зник/зіпсувався — лишається останній добрий список (ERROR у
//     журнал), тобто вже відкликані ноди назад не пускаються.
//
// Уже живу агентську ногу відкликання не рве (лише нове підключення); для
// негайного обриву є runtime-revoke ERP kind="node".
package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// minAgentMasterLen — найкоротший master, який хаб вважає годящим (символів).
const minAgentMasterLen = 32

// revokedRecheck — як часто дивитися, чи змінився файл відкликань.
const revokedRecheck = 2 * time.Second

// agentAuthWarnings — нефатальні зауваги до конфігурації (журнал старту).
func agentAuthWarnings() []string {
	var w []string
	m := agentMaster()
	if m != "" && m != token && len(m) < minAgentMasterLen && !agentAuthStrict() {
		w = append(w, fmt.Sprintf("master коротший за %d символів — згенеруй новий (oo-node-token -gen-secret); у strict такий хаб не стартує", minAgentMasterLen))
	}
	if p := os.Getenv("OO_SCREEN_AGENT_SECRET_FILE"); p != "" && os.Getenv("OO_SCREEN_AGENT_SECRET") == "" && !agentAuthStrict() {
		if err := secretFileModeError(p); err != nil {
			w = append(w, err.Error())
		}
	}
	if agentAuthStrict() && os.Getenv("OO_SCREEN_AGENT_SECRET") != "" {
		w = append(w, "master задано env OO_SCREEN_AGENT_SECRET — він видно в /proc/<pid>/environ і юніт-файлі; на проді давай файлом OO_SCREEN_AGENT_SECRET_FILE (chmod 600)")
	}
	if prev := agentMasterPrev(); prev != "" && prev == m {
		w = append(w, "OO_SCREEN_AGENT_SECRET_PREV == поточний master — ротації нема, прибери _PREV")
	}
	if legacyAgentTokenAllowed() && agentAuthStrict() {
		w = append(w, "OO_SCREEN_LEGACY_AGENT_TOKEN=1 у strict — аварійний відкат увімкнено: спільний токен приймається; прибери після відновлення")
	}
	return w
}

// agentAuthHardenError — фатальні (для strict) вади master і файлу
// відкликань. Кличе agentAuthConfigError.
func agentAuthHardenError() error {
	if agentAuthStrict() {
		m := agentMaster()
		if m != "" && m != token && len(m) < minAgentMasterLen {
			return fmt.Errorf("OO_SCREEN_AGENT_AUTH=strict, але master коротший за %d символів — згенеруй новий (oo-node-token -gen-secret) і перевипусти токени нод", minAgentMasterLen)
		}
		if p := os.Getenv("OO_SCREEN_AGENT_SECRET_FILE"); p != "" && os.Getenv("OO_SCREEN_AGENT_SECRET") == "" {
			if err := secretFileModeError(p); err != nil {
				return fmt.Errorf("OO_SCREEN_AGENT_AUTH=strict: %w", err)
			}
		}
	}
	return revoked.init(os.Getenv("OO_SCREEN_AGENT_REVOKED_FILE"))
}

// secretFileModeError — файл master читний не лише власнику.
func secretFileModeError(p string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil // нечитність ловить agentAuthConfigError
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("файл master %s має права %#o — його може читати не лише власник; виконай chmod 600", p, perm)
	}
	return nil
}

// errRevokedFile — файл відкликань задано, але він непридатний.
var errRevokedFile = errors.New("OO_SCREEN_AGENT_REVOKED_FILE")

// revokedNodes — список відкликаних node_id з файлу, перечитується на зміну.
type revokedNodes struct {
	mu      sync.Mutex
	path    string
	set     map[string]struct{}
	mod     time.Time
	size    int64
	checked time.Time
	broken  bool // останнє перечитування не вдалося (ERROR уже в журналі)
	warned  map[string]struct{}
}

var revoked revokedNodes

// parseRevoked — node_id з файлу; недопустимий рядок = помилка з номером.
func parseRevoked(path string) (map[string]struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errRevokedFile, err)
	}
	defer f.Close()
	set := map[string]struct{}{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		n := strings.TrimSpace(sc.Text())
		if n == "" || strings.HasPrefix(n, "#") {
			continue
		}
		if !validAgentNodeID(n) {
			return nil, fmt.Errorf("%w: %s:%d: недопустимий node_id %q", errRevokedFile, path, line, n)
		}
		set[n] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errRevokedFile, path, err)
	}
	return set, nil
}

// init — перше читання на старті (помилка = хаб не стартує). path "" — вимкнено.
func (r *revokedNodes) init(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path, r.set, r.broken, r.checked = path, nil, false, time.Time{}
	r.warned = map[string]struct{}{}
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %v", errRevokedFile, err)
	}
	set, err := parseRevoked(path)
	if err != nil {
		return err
	}
	r.set, r.mod, r.size, r.checked = set, st.ModTime(), st.Size(), time.Now()
	return nil
}

// refreshLocked — перечитати, якщо файл змінився; помилка — лишається
// останній добрий список.
func (r *revokedNodes) refreshLocked(now time.Time) {
	if now.Sub(r.checked) < revokedRecheck {
		return
	}
	r.checked = now
	st, err := os.Stat(r.path)
	if err == nil && st.ModTime().Equal(r.mod) && st.Size() == r.size && !r.broken {
		return
	}
	var set map[string]struct{}
	if err == nil {
		set, err = parseRevoked(r.path)
	}
	if err != nil {
		if !r.broken {
			log.Printf("ERROR agent-auth: %v — лишається попередній список відкликаних нод (%d)", err, len(r.set))
		}
		r.broken = true
		return
	}
	if r.broken || len(set) != len(r.set) {
		log.Printf("agent-auth: список відкликаних нод перечитано: %d", len(set))
	}
	r.set, r.mod, r.size, r.broken = set, st.ModTime(), st.Size(), false
}

// has — чи відкликано node. Нода "" (T1) у списку бути не може.
func (r *revokedNodes) has(node string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path == "" {
		return false
	}
	r.refreshLocked(now)
	_, ok := r.set[node]
	if ok {
		if _, seen := r.warned[node]; !seen {
			r.warned[node] = struct{}{}
			log.Printf("offer/agent [node=%s]: нода у OO_SCREEN_AGENT_REVOKED_FILE — агенту відмовлено", node)
		}
	}
	return ok
}

// count — скільки нод відкликано (для журналу старту).
func (r *revokedNodes) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.set)
}

// agentNodeRevoked — кличе agentAuthorized до перевірки токена.
func agentNodeRevoked(node string) bool { return revoked.has(node, time.Now()) }
