package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/hub"
)

const hardMaster = "0123456789abcdef0123456789abcdef0123456789abcdef" // 48 символів

func clearHardenEnv(t *testing.T) {
	clearAgentAuthEnv(t)
	t.Setenv("OO_SCREEN_AGENT_REVOKED_FILE", "")
	t.Cleanup(func() { _ = revoked.init("") })
}

func hasWarning(sub string) bool {
	for _, w := range agentAuthWarnings() {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// S1 (хвиля 8): короткий master — у strict хаб не стартує, без strict — WARNING.
func TestHardenWeakMaster(t *testing.T) {
	clearHardenEnv(t)
	t.Setenv("OO_SCREEN_AGENT_SECRET", "short-master-123")
	if err := agentAuthConfigError(); err != nil {
		t.Fatalf("без strict короткий master — лише попередження: %v", err)
	}
	if !hasWarning("коротший") {
		t.Fatalf("нема WARNING про короткий master: %v", agentAuthWarnings())
	}
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	if err := agentAuthConfigError(); err == nil || !strings.Contains(err.Error(), "коротший") {
		t.Fatalf("strict з коротким master стартує: %v", err)
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET", hardMaster)
	if err := agentAuthConfigError(); err != nil {
		t.Fatalf("strict з довгим master: %v", err)
	}
	// Master в env у strict — працює, але з порадою перейти на файл.
	if !hasWarning("/proc") {
		t.Fatalf("нема WARNING про master в env: %v", agentAuthWarnings())
	}
	// _PREV == master — безглузда ротація.
	t.Setenv("OO_SCREEN_AGENT_SECRET_PREV", hardMaster)
	if !hasWarning("_PREV") {
		t.Fatalf("нема WARNING про _PREV == master: %v", agentAuthWarnings())
	}
}

// S1 (хвиля 8): файл master, читний групі/всім, — у strict хаб не стартує.
func TestHardenSecretFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("біти прав на Windows не перевіряються")
	}
	clearHardenEnv(t)
	p := filepath.Join(t.TempDir(), "master")
	if err := os.WriteFile(p, []byte(hardMaster+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET_FILE", p)
	if err := agentAuthConfigError(); err != nil {
		t.Fatalf("без strict — лише попередження: %v", err)
	}
	if !hasWarning("chmod 600") {
		t.Fatalf("нема WARNING про права: %v", agentAuthWarnings())
	}
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	if err := agentAuthConfigError(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("strict з 0644: %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agentAuthConfigError(); err != nil {
		t.Fatalf("strict з 0600: %v", err)
	}
	if hasWarning("chmod") || hasWarning("/proc") {
		t.Fatalf("зайві попередження для правильного файлу: %v", agentAuthWarnings())
	}
}

// S1 (хвиля 8): відкликання ОДНІЄЇ ноди без ротації master — жодним токеном
// (поточний master, старий master, спільний легасі), включно з потоками
// додаткових моніторів; решта парку живе; файл перечитується без перезапуску.
func TestHardenRevokedNodes(t *testing.T) {
	clearHardenEnv(t)
	const prev = "fedcba9876543210fedcba9876543210fedcba98"
	t.Setenv("OO_SCREEN_AGENT_SECRET", hardMaster)
	t.Setenv("OO_SCREEN_AGENT_SECRET_PREV", prev)
	p := filepath.Join(t.TempDir(), "revoked")
	if err := os.WriteFile(p, []byte("# вкрадений ноутбук\nPC-007\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OO_SCREEN_AGENT_REVOKED_FILE", p)
	if err := agentAuthConfigError(); err != nil {
		t.Fatalf("config: %v", err)
	}
	if !strings.Contains(agentAuthModeSummary(), "відкликано нод: 1") {
		t.Fatalf("summary: %s", agentAuthModeSummary())
	}
	if agentAuthorized("PC-007", hub.NodeToken(hardMaster, "PC-007")) {
		t.Fatal("відкликана нода пройшла токеном поточного master")
	}
	if agentAuthorized("PC-007", hub.NodeToken(prev, "PC-007")) {
		t.Fatal("відкликана нода пройшла токеном старого master")
	}
	if agentAuthorized("PC-007", token) {
		t.Fatal("відкликана нода пройшла спільним легасі-токеном")
	}
	prevMM := multimonEnabled
	multimonEnabled = true
	t.Cleanup(func() { multimonEnabled = prevMM })
	if agentAuthNode("PC-007#m1") != "PC-007" || agentAuthorized(agentAuthNode("PC-007#m1"), hub.NodeToken(hardMaster, "PC-007")) {
		t.Fatal("потік додаткового монітора відкликаної ноди пройшов")
	}
	if !agentAuthorized("PC-008", hub.NodeToken(hardMaster, "PC-008")) {
		t.Fatal("сусідню ноду відхилено")
	}
	if !agentAuthorized("", token) {
		t.Fatal("T1-режим (порожній node) зачеплено")
	}

	// Перечитування на зміну файлу (mtime/розмір), без перезапуску.
	later := time.Now().Add(time.Minute)
	if err := os.WriteFile(p, []byte("PC-008\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, later, later)
	if !revoked.has("PC-008", time.Now().Add(revokedRecheck)) || revoked.has("PC-007", time.Now().Add(revokedRecheck)) {
		t.Fatal("зміну файлу не підхоплено")
	}
	// Зіпсований файл під час роботи — лишається останній добрий список.
	if err := os.WriteFile(p, []byte("ok\nbad\x01id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later = later.Add(time.Minute)
	_ = os.Chtimes(p, later, later)
	if !revoked.has("PC-008", time.Now().Add(2*revokedRecheck)) {
		t.Fatal("зіпсований файл зняв відкликання (fail open)")
	}
	// Файл зник — те саме.
	_ = os.Remove(p)
	if !revoked.has("PC-008", time.Now().Add(3*revokedRecheck)) {
		t.Fatal("видалений файл зняв відкликання (fail open)")
	}
}

// S1 (хвиля 8): файл відкликань задано, але він непридатний — хаб не стартує.
func TestHardenRevokedFileFailClosedAtStart(t *testing.T) {
	clearHardenEnv(t)
	t.Setenv("OO_SCREEN_AGENT_REVOKED_FILE", filepath.Join(t.TempDir(), "nope"))
	if err := agentAuthConfigError(); err == nil || !strings.Contains(err.Error(), "OO_SCREEN_AGENT_REVOKED_FILE") {
		t.Fatalf("нечитний файл відкликань: %v", err)
	}
	p := filepath.Join(t.TempDir(), "revoked")
	if err := os.WriteFile(p, []byte("PC-1\nbad\tid\x7f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OO_SCREEN_AGENT_REVOKED_FILE", p)
	if err := agentAuthConfigError(); err == nil || !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("недопустимий node_id має назвати рядок: %v", err)
	}
}
