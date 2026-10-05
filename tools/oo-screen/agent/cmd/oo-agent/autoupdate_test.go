package main

import "testing"

func TestHubHostPort(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:4470/offer/agent": "127.0.0.1:4470",
		"https://hub.example/offer/agent":   "hub.example:443",
		"http://hub.example/x":              "hub.example:80",
		"localhost:4460":                    "localhost:4460",
	} {
		if got := hubHostPort(in); got != want {
			t.Errorf("%s -> %s want %s", in, got, want)
		}
	}
}

// Default build: no pinned key -> an update URL alone must not enable updates.
func TestNewUpdaterRequiresPinnedKey(t *testing.T) {
	if updatePubKey != "" {
		t.Skip("built with a pinned key")
	}
	if _, err := newUpdater("https://example/manifest", "pc"); err == nil {
		t.Fatal("updater created without pinned key")
	}
}
