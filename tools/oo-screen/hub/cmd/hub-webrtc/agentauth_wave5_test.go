package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/organicoils/oo-screen/hub"
)

func clearAgentAuthEnv(t *testing.T) {
	for _, k := range []string{"OO_SCREEN_AGENT_AUTH", "OO_SCREEN_LEGACY_AGENT_TOKEN", "OO_SCREEN_AGENT_SECRET", "OO_SCREEN_AGENT_SECRET_FILE", "OO_SCREEN_AGENT_SECRET_PREV"} {
		t.Setenv(k, "")
	}
}

// S1: master з файлу (не з env) — токен ноди з нього приймається в strict.
func TestWave5SecretFile(t *testing.T) {
	clearAgentAuthEnv(t)
	p := filepath.Join(t.TempDir(), "master")
	if err := os.WriteFile(p, []byte("file-master-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET_FILE", p)
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	if err := agentAuthConfigError(); err != nil {
		t.Fatalf("валідна конфігурація: %v", err)
	}
	if !agentAuthorized("pc1", hub.NodeToken("file-master-0123456789", "pc1")) {
		t.Fatal("токен від master-файлу відхилено")
	}
	if agentAuthorized("pc1", token) {
		t.Fatal("легасі прийнято в strict")
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET_FILE", filepath.Join(t.TempDir(), "nope"))
	if err := agentAuthConfigError(); err != errSecretFileUnreadable {
		t.Fatalf("нечитний файл: %v", err)
	}
}

// S1: strict без окремого master — хаб не стартує (а не тихо рубає весь парк);
// аварійний відкат OO_SCREEN_LEGACY_AGENT_TOKEN=1 це знімає.
func TestWave5StrictWithoutMasterIsConfigError(t *testing.T) {
	clearAgentAuthEnv(t)
	if err := agentAuthConfigError(); err != nil {
		t.Fatalf("легасі-режим за замовчуванням: %v", err)
	}
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	if err := agentAuthConfigError(); err != errStrictNoMaster {
		t.Fatalf("strict без master: %v", err)
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET", token)
	if err := agentAuthConfigError(); err != errStrictNoMaster {
		t.Fatalf("strict з master==T1: %v", err)
	}
	t.Setenv("OO_SCREEN_LEGACY_AGENT_TOKEN", "1")
	if err := agentAuthConfigError(); err != nil {
		t.Fatalf("аварійний відкат: %v", err)
	}
	if !strings.Contains(agentAuthModeSummary(), "аварійний") {
		t.Fatalf("summary: %s", agentAuthModeSummary())
	}
}

// S1: ротація master — токени старого master приймаються лише поки задано
// _PREV; спільний T1 як _PREV не обходить strict.
func TestWave5MasterRotation(t *testing.T) {
	clearAgentAuthEnv(t)
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	t.Setenv("OO_SCREEN_AGENT_SECRET", "new-master-0123456789")
	oldTok := hub.NodeToken("old-master-0123456789", "pc1")
	if agentAuthorized("pc1", oldTok) {
		t.Fatal("старий токен без _PREV прийнято")
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET_PREV", "old-master-0123456789")
	if !agentAuthorized("pc1", oldTok) {
		t.Fatal("старий токен під час ротації відхилено")
	}
	if agentAuthorized("pc2", oldTok) {
		t.Fatal("старий токен pc1 прийнято для pc2")
	}
	if !agentAuthorized("pc1", hub.NodeToken("new-master-0123456789", "pc1")) {
		t.Fatal("новий токен відхилено")
	}
	if !strings.Contains(agentAuthModeSummary(), "ротація") {
		t.Fatal("summary не згадує ротацію")
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET_PREV", token)
	if agentAuthorized("pc1", hub.NodeToken(token, "pc1")) {
		t.Fatal("T1 як _PREV обійшов strict")
	}
	if agentAuthorized("pc1", token) {
		t.Fatal("легасі прийнято в strict")
	}
}

// Fix: master-файл читається один раз; якщо згодом його видалено/спорожнено,
// токени нод від справжнього master далі валідні, а спільний T1-токен НЕ стає
// master (інакше в легасі будь-хто з T1 виковував би токени нод).
func TestWave5SecretFileFailClosed(t *testing.T) {
	clearAgentAuthEnv(t)
	p := filepath.Join(t.TempDir(), "master")
	if err := os.WriteFile(p, []byte("cached-master-0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OO_SCREEN_AGENT_SECRET_FILE", p)
	if err := agentAuthConfigError(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if !agentAuthorized("pc1", hub.NodeToken("cached-master-0123456789", "pc1")) {
		t.Fatal("після видалення файлу токен справжнього master відхилено")
	}
	if agentAuthorized("pc1", hub.NodeToken(token, "pc1")) {
		t.Fatal("токен ноди, підписаний T1, прийнято")
	}
	// Файл, що ні разу не прочитався: fail closed, а не T1.
	t.Setenv("OO_SCREEN_AGENT_SECRET_FILE", filepath.Join(t.TempDir(), "never"))
	if agentMaster() != "" {
		t.Fatal("нечитний файл: master не порожній")
	}
	if agentAuthorized("pc2", hub.NodeToken(token, "pc2")) {
		t.Fatal("нечитний файл: токен ноди від T1 прийнято")
	}
}
