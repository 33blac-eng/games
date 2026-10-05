package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/autoupdate"
	"github.com/organicoils/oo-screen/internal/rollout"
)

type env struct {
	dir, man, st, reps string
	pub                ed25519.PublicKey
	clock              time.Time
	srv                *httptest.Server
}

func setup(t *testing.T) *env {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OO_UPDATE_SIGNING_KEY", base64.StdEncoding.EncodeToString(priv.Seed()))
	d := t.TempDir()
	e := &env{dir: d, man: filepath.Join(d, "manifest.json"), st: filepath.Join(d, "state.json"),
		reps: filepath.Join(d, "reports.jsonl"), pub: pub, clock: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	b, _ := autoupdate.Sign(priv, autoupdate.Manifest{Product: autoupdate.Product, Version: "1.5.0",
		Artifacts: []autoupdate.Artifact{{OS: "windows", Arch: "amd64", URL: "https://x/a.exe", SHA256: "00", Size: 1}}})
	if err := os.WriteFile(e.man, b, 0o644); err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(reportHandler(e.reps, "tok", func() time.Time { return e.clock }))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) percent(t *testing.T) int {
	b, _ := os.ReadFile(e.man)
	m, err := autoupdate.Verify(e.pub, b)
	if err != nil {
		t.Fatal(err)
	}
	return m.RolloutPercent
}

func (e *env) post(t *testing.T, node, res, tok string) int {
	b, _ := json.Marshal(rollout.Report{Node: node, Version: "1.5.0", Result: res, At: time.Unix(0, 0)})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

var planArgs = []string{"-stages", "5,50,100", "-soak", "1h", "-min-reports", "2", "-max-fail-rate", "0.2", "-max-stage-time", "0"}

func (e *env) step(t *testing.T) (int, string) {
	var out bytes.Buffer
	code, err := cmdStep(append([]string{"-manifest", e.man, "-state", e.st, "-reports", e.reps}, planArgs...), e.clock, &out)
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

func TestStagedRolloutEndToEnd(t *testing.T) {
	e := setup(t)
	if err := cmdInit(append([]string{"-manifest", e.man, "-state", e.st}, planArgs...), e.clock); err != nil {
		t.Fatal(err)
	}
	if p := e.percent(t); p != 5 {
		t.Fatalf("canary percent %d", p)
	}
	if err := cmdInit([]string{"-manifest", e.man, "-state", e.st}, e.clock); err == nil {
		t.Fatal("init over existing state without -force")
	}
	e.clock = e.clock.Add(10 * time.Minute)
	if c := e.post(t, "a", rollout.ResultOK, "tok"); c != http.StatusNoContent {
		t.Fatalf("post %d", c)
	}
	e.post(t, "b", rollout.ResultOK, "tok")
	if _, out := e.step(t); !strings.Contains(out, "action=hold") || e.percent(t) != 5 {
		t.Fatalf("before soak: %s", out)
	}
	e.clock = e.clock.Add(time.Hour)
	if _, out := e.step(t); !strings.Contains(out, "action=advance") || e.percent(t) != 50 {
		t.Fatalf("advance: %s", out)
	}
	e.clock = e.clock.Add(time.Minute)
	e.post(t, "c", rollout.ResultFail, "tok")
	code, out := e.step(t)
	if code != exitHalt || !strings.Contains(out, "action=halt") || e.percent(t) != 0 {
		t.Fatalf("halt: code %d %s pct %d", code, out, e.percent(t))
	}
	// halted stays halted
	e.clock = e.clock.Add(24 * time.Hour)
	for i := 0; i < 20; i++ {
		e.post(t, fmt.Sprint("n", i), rollout.ResultOK, "tok")
	}
	if code, _ := e.step(t); code != exitHalt || e.percent(t) != 0 {
		t.Fatal("halt not sticky")
	}
}

func TestReportHandlerRejects(t *testing.T) {
	e := setup(t)
	if c := e.post(t, "a", rollout.ResultOK, "wrong"); c != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", c)
	}
	if c := e.post(t, "a", "great", "tok"); c != http.StatusBadRequest {
		t.Fatalf("bad result: %d", c)
	}
	if c := e.post(t, "", rollout.ResultOK, "tok"); c != http.StatusBadRequest {
		t.Fatalf("empty node: %d", c)
	}
	e.post(t, "a", rollout.ResultOK, "tok")
	r, err := loadReports(e.reps)
	if err != nil || len(r) != 1 || !r[0].At.Equal(e.clock) {
		t.Fatalf("stored %+v %v (server clock must replace client time)", r, err)
	}
}

func TestStepRejectsForeignManifest(t *testing.T) {
	e := setup(t)
	if err := cmdInit([]string{"-manifest", e.man, "-state", e.st}, e.clock); err != nil {
		t.Fatal(err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv("OO_UPDATE_SIGNING_KEY", base64.StdEncoding.EncodeToString(other.Seed()))
	if _, err := cmdStep([]string{"-manifest", e.man, "-state", e.st, "-reports", e.reps}, e.clock, &bytes.Buffer{}); err == nil {
		t.Fatal("manifest signed by another key accepted")
	}
}
