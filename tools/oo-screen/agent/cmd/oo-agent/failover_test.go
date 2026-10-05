package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHubSelectorOffByDefault(t *testing.T) {
	called := false
	s := newHubSelector("http://a/offer/agent", "", 0, func(context.Context, string) error { called = true; return nil })
	for i := 0; i < 10; i++ {
		if s.failed(context.Background()) {
			t.Fatal("switched without standby")
		}
	}
	if called || s.current() != "http://a/offer/agent" {
		t.Fatal("health probed or address changed with standby OFF")
	}
}

func TestHubSelectorSwitchesToHealthy(t *testing.T) {
	healthy := map[string]bool{"http://c/offer/agent": true}
	s := newHubSelector("http://a/offer/agent", "http://b/offer/agent, http://c/offer/agent", 2,
		func(_ context.Context, u string) error {
			if healthy[u] {
				return nil
			}
			return errors.New("down")
		})
	ctx := context.Background()
	if s.failed(ctx) {
		t.Fatal("switched before failAfter")
	}
	if !s.failed(ctx) || s.current() != "http://c/offer/agent" {
		t.Fatalf("want c, got %s", s.current())
	}
	healthy = map[string]bool{} // всі мертві — лишаємось
	s.failed(ctx)
	if s.failed(ctx) || s.current() != "http://c/offer/agent" {
		t.Fatal("must stay when nobody is healthy")
	}
	healthy["http://a/offer/agent"] = true // повернення на основний
	s.failed(ctx)
	if !s.failed(ctx) || s.current() != "http://a/offer/agent" {
		t.Fatalf("want a, got %s", s.current())
	}
}

func TestHubSelectorOkResets(t *testing.T) {
	s := newHubSelector("a", "b", 2, func(context.Context, string) error { return nil })
	s.failed(context.Background())
	s.ok()
	if s.failed(context.Background()) {
		t.Fatal("ok() must reset the failure counter")
	}
}

func TestHTTPHealth(t *testing.T) {
	body, code := `{"ok":true}`, 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := httpHealth(ctx, srv.URL+"/offer/agent"); err != nil {
		t.Fatal(err)
	}
	body = `{"ok":false}`
	if httpHealth(ctx, srv.URL+"/offer/agent") == nil {
		t.Fatal("ok=false accepted")
	}
	body, code = `{"ok":true}`, 503
	if httpHealth(ctx, srv.URL+"/offer/agent") == nil {
		t.Fatal("503 accepted")
	}
	if httpHealth(ctx, "::bad") == nil {
		t.Fatal("bad url accepted")
	}
}
