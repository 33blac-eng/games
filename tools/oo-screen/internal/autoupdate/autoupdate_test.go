package autoupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Tests use a freshly generated key; no real signing key is in the repo.
func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func manifest(ver string, pct int, art Artifact) Manifest {
	return Manifest{Product: Product, Version: ver, RolloutPercent: pct, Artifacts: []Artifact{art}}
}

func TestVerify(t *testing.T) {
	pub, priv := testKey(t)
	otherPub, otherPriv := testKey(t)
	_ = otherPub
	env, err := Sign(priv, manifest("1.2.0", 100, Artifact{OS: "windows", Arch: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	if m, err := Verify(pub, env); err != nil || m.Version != "1.2.0" {
		t.Fatalf("valid: %v %v", m, err)
	}
	// Wrong key.
	envOther, _ := Sign(otherPriv, manifest("1.2.0", 100, Artifact{}))
	if _, err := Verify(pub, envOther); !errors.Is(err, ErrBadSig) {
		t.Fatalf("foreign key: %v", err)
	}
	// Tampered body (raise rollout to 100 / change url) with original sig.
	var s Signed
	_ = json.Unmarshal(env, &s)
	raw, _ := base64.StdEncoding.DecodeString(s.Manifest)
	var m Manifest
	_ = json.Unmarshal(raw, &m)
	m.Artifacts[0].URL = "http://evil/agent.exe"
	raw2, _ := json.Marshal(m)
	s.Manifest = base64.StdEncoding.EncodeToString(raw2)
	tampered, _ := json.Marshal(s)
	if _, err := Verify(pub, tampered); !errors.Is(err, ErrBadSig) {
		t.Fatalf("tampered: %v", err)
	}
	// No key pinned.
	if _, err := Verify(nil, env); !errors.Is(err, ErrNoKey) {
		t.Fatalf("nil key: %v", err)
	}
	// Garbage.
	if _, err := Verify(pub, []byte("{")); !errors.Is(err, ErrBadFormat) {
		t.Fatalf("garbage: %v", err)
	}
	// Wrong product is rejected even if signed.
	bad := manifest("9.9.9", 100, Artifact{})
	bad.Product = "other"
	envBad, _ := Sign(priv, bad)
	if _, err := Verify(pub, envBad); !errors.Is(err, ErrBadFormat) {
		t.Fatalf("product: %v", err)
	}
}

func TestParsePublicKey(t *testing.T) {
	pub, _ := testKey(t)
	for _, s := range []string{hex.EncodeToString(pub), base64.StdEncoding.EncodeToString(pub)} {
		k, err := ParsePublicKey(s)
		if err != nil || !k.Equal(pub) {
			t.Fatalf("%q: %v", s, err)
		}
	}
	if _, err := ParsePublicKey(""); !errors.Is(err, ErrNoKey) {
		t.Fatal(err)
	}
	if _, err := ParsePublicKey("abcd"); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{{"1.2.10", "1.2.9", 1}, {"v1.0", "1.0.0", 0}, {"0.9", "1.0", -1}, {"2", "1.99.99", 1}}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s = %d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestRolloutStagedDistribution(t *testing.T) {
	m := &Manifest{Version: "1.3.0"}
	for _, pct := range []int{0, 10, 50, 100} {
		m.RolloutPercent = pct
		in := 0
		const n = 10000
		for i := 0; i < n; i++ {
			if InRollout(fmt.Sprintf("node-%d", i), m) {
				in++
			}
		}
		got := float64(in) * 100 / n
		if pct == 0 && in != 0 || pct == 100 && in != n || got < float64(pct)-2 || got > float64(pct)+2 {
			t.Errorf("pct=%d got %.2f%%", pct, got)
		}
	}
	// Monotonic: a node in at 10% stays in at 50%.
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("n%d", i)
		m.RolloutPercent = 10
		a := InRollout(id, m)
		m.RolloutPercent = 50
		if a && !InRollout(id, m) {
			t.Fatalf("%s dropped out when widening", id)
		}
	}
}

type fixture struct {
	u       *Updater
	priv    ed25519.PrivateKey
	srv     *httptest.Server
	payload []byte
	env     []byte
}

func newFixture(t *testing.T, ver string, pct int) *fixture {
	t.Helper()
	pub, priv := testKey(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "oo-agent.exe")
	if err := os.WriteFile(exe, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{priv: priv, payload: []byte("NEW-BINARY-" + ver)}
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(f.env) })
	mux.HandleFunc("/agent.exe", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(f.payload) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	sum := sha256.Sum256(f.payload)
	f.env, _ = Sign(priv, manifest(ver, pct, Artifact{OS: "windows", Arch: "amd64",
		URL: f.srv.URL + "/agent.exe", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(f.payload))}))
	f.u = &Updater{ExePath: exe, PubKey: pub, NodeID: "pc-1", CurrentVersion: "1.0.0",
		ManifestURL: f.srv.URL + "/manifest", GOOS: "windows", GOARCH: "amd64"}
	return f
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestFullUpdateCommit(t *testing.T) {
	f := newFixture(t, "1.1.0", 100)
	ctx := context.Background()
	m, a, err := f.u.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.u.Stage(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := f.u.Swap(m.Version); err != nil {
		t.Fatal(err)
	}
	if read(t, f.u.ExePath) != "NEW-BINARY-1.1.0" || read(t, f.u.ExePath+".old") != "OLD-BINARY" {
		t.Fatal("swap wrong")
	}
	// Second check while pending is refused.
	if _, _, err := f.u.Check(ctx); !errors.Is(err, ErrUpdatePending) {
		t.Fatalf("pending: %v", err)
	}
	res, err := f.u.Startup(ctx, func(context.Context) error { return nil })
	if err != nil || res != Committed {
		t.Fatalf("startup: %v %v", res, err)
	}
	if exists(f.u.ExePath+".old") || exists(f.u.ExePath+".update.json") {
		t.Fatal("leftovers after commit")
	}
	// No marker -> nothing to do.
	if res, _ := f.u.Startup(ctx, nil); res != NoPending {
		t.Fatal(res)
	}
}

func TestRollbackOnFailedHealth(t *testing.T) {
	f := newFixture(t, "1.1.0", 100)
	ctx := context.Background()
	m, a, _ := f.u.Check(ctx)
	_ = f.u.Stage(ctx, a)
	_ = f.u.Swap(m.Version)
	res, err := f.u.Startup(ctx, func(context.Context) error { return errors.New("hub unreachable") })
	if res != RolledBack || err == nil {
		t.Fatalf("%v %v", res, err)
	}
	if read(t, f.u.ExePath) != "OLD-BINARY" || exists(f.u.ExePath+".update.json") {
		t.Fatal("not rolled back")
	}
}

func TestRollbackOnCrashLoop(t *testing.T) {
	f := newFixture(t, "1.1.0", 100)
	f.u.MaxStarts = 2
	ctx := context.Background()
	m, a, _ := f.u.Check(ctx)
	_ = f.u.Stage(ctx, a)
	_ = f.u.Swap(m.Version)
	// Simulate the new binary crashing inside health twice.
	for i := 0; i < 2; i++ {
		func() {
			defer func() { _ = recover() }()
			_, _ = f.u.Startup(ctx, func(context.Context) error { panic("crash") })
		}()
	}
	res, _ := f.u.Startup(ctx, func(context.Context) error { t.Fatal("health must not run"); return nil })
	if res != RolledBack || read(t, f.u.ExePath) != "OLD-BINARY" {
		t.Fatalf("crash loop: %v", res)
	}
}

func TestCheckRejections(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "1.0.0", 100)
	if _, _, err := f.u.Check(ctx); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("same version: %v", err)
	}
	f = newFixture(t, "0.9.0", 100) // replay of an old signed manifest = downgrade
	if _, _, err := f.u.Check(ctx); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("downgrade: %v", err)
	}
	f = newFixture(t, "1.1.0", 0)
	if _, _, err := f.u.Check(ctx); !errors.Is(err, ErrNotInRollout) {
		t.Fatalf("rollout 0: %v", err)
	}
	f = newFixture(t, "1.1.0", 100)
	f.u.GOARCH = "arm64"
	if _, _, err := f.u.Check(ctx); !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("arch: %v", err)
	}
}

func TestStageRejectsTamperedBinary(t *testing.T) {
	f := newFixture(t, "1.1.0", 100)
	ctx := context.Background()
	_, a, err := f.u.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.payload = []byte("EVIL-BINARY-1.1") // same length, different bytes
	if err := f.u.Stage(ctx, a); !errors.Is(err, ErrDigest) {
		t.Fatalf("tampered: %v", err)
	}
	f.payload = append([]byte("NEW-BINARY-1.1.0"), 'x') // longer than signed size
	if err := f.u.Stage(ctx, a); err == nil {
		t.Fatal("oversize accepted")
	}
	if exists(f.u.ExePath + ".new") {
		t.Fatal("staged a bad binary")
	}
	if err := f.u.Swap("1.1.0"); err == nil || read(t, f.u.ExePath) != "OLD-BINARY" {
		t.Fatal("swap without staged binary")
	}
}
