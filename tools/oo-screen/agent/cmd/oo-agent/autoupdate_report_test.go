package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/autoupdate"
	"github.com/organicoils/oo-screen/internal/rollout"
)

func TestRolloutIDFallsBackToHostname(t *testing.T) {
	if got := rolloutID("pc-1"); got != "pc-1" {
		t.Fatalf("mesh node: %q", got)
	}
	if got := hostRolloutID(func() (string, error) { return "WS-07", nil }); got != "host:ws-07" {
		t.Fatalf("T1 host: %q", got)
	}
	if got := hostRolloutID(func() (string, error) { return "", context.Canceled }); got != "" {
		t.Fatalf("no hostname: %q", got)
	}
}

func TestStartupVerdict(t *testing.T) {
	cases := map[autoupdate.StartupResult]string{
		autoupdate.NoPending:      "",
		autoupdate.Committed:      rollout.ResultOK,
		autoupdate.RolledBack:     rollout.ResultFail,
		autoupdate.RollbackFailed: rollout.ResultFail,
		autoupdate.Inconclusive:   rollout.ResultInconclusive,
	}
	for res, want := range cases {
		if got := startupVerdict(res); got != want {
			t.Errorf("%v: got %q want %q", res, got, want)
		}
	}
}

func TestPostHealthReport(t *testing.T) {
	var got rollout.Report
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r := rollout.Report{Node: "pc1", Version: "1.2.0", Result: rollout.ResultOK, At: time.Now().UTC()}
	if err := postHealthReport(context.Background(), srv.Client(), srv.URL, "tok", r); err != nil {
		t.Fatal(err)
	}
	if got.Node != "pc1" || got.Result != rollout.ResultOK || auth != "Bearer tok" {
		t.Fatalf("got %+v auth %q", got, auth)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	if err := postHealthReport(context.Background(), bad.Client(), bad.URL, "", r); err == nil {
		t.Fatal("401 not reported as error")
	}
}
