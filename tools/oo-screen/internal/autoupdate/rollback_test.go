package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
)

func swapTo(t *testing.T, f *fixture) {
	t.Helper()
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
}

// After a rollback the restored old binary must not re-install the same
// signed version (no update/rollback cycle), but a newer one is allowed.
func TestRollbackDeniesVersion(t *testing.T) {
	f := newFixture(t, "1.1.0", 100)
	ctx := context.Background()
	swapTo(t, f)
	if res, _ := f.u.Startup(ctx, func(context.Context) error { return errors.New("broken") }); res != RolledBack {
		t.Fatal(res)
	}
	if _, _, err := f.u.Check(ctx); !errors.Is(err, ErrDenied) {
		t.Fatalf("re-update after rollback: %v", err)
	}
	if read(t, f.u.ExePath) != "OLD-BINARY" || exists(f.u.ExePath+".new") {
		t.Fatal("bad version staged again")
	}
	// A fixed, newer release is still picked up.
	f.payload = []byte("NEW-BINARY-1.1.1")
	sum := sha256.Sum256(f.payload)
	f.env, _ = Sign(f.priv, manifest("1.1.1", 100, Artifact{OS: "windows", Arch: "amd64",
		URL: f.srv.URL + "/agent.exe", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(f.payload))}))
	if _, _, err := f.u.Check(ctx); err != nil {
		t.Fatalf("newer after deny: %v", err)
	}
}

func TestCrashLoopRollbackDenies(t *testing.T) {
	f := newFixture(t, "1.1.0", 100)
	f.u.MaxStarts = 1
	swapTo(t, f)
	func() {
		defer func() { _ = recover() }()
		_, _ = f.u.Startup(context.Background(), func(context.Context) error { panic("crash") })
	}()
	if res, _ := f.u.Startup(context.Background(), nil); res != RolledBack {
		t.Fatal(res)
	}
	if !f.u.Denied("1.1.0") {
		t.Fatal("crash-looping version not denied")
	}
}

// .old missing: Startup must not report RolledBack (the caller would relaunch
// the same bad binary with the guard disarmed).
func TestRollbackWithoutOld(t *testing.T) {
	f := newFixture(t, "1.1.0", 100)
	swapTo(t, f)
	if err := os.Remove(f.u.ExePath + ".old"); err != nil {
		t.Fatal(err)
	}
	res, err := f.u.Startup(context.Background(), func(context.Context) error { return errors.New("broken") })
	if res != RollbackFailed || !errors.Is(err, ErrRollbackImpossible) {
		t.Fatalf("%v %v", res, err)
	}
	if !f.u.Denied("1.1.0") || read(t, f.u.ExePath) != "NEW-BINARY-1.1.0" {
		t.Fatal("state after impossible rollback")
	}
}

// Inconclusive health (hub down) neither rolls back nor counts the start.
func TestInconclusiveHealthKeepsMarker(t *testing.T) {
	f := newFixture(t, "1.1.0", 100)
	f.u.MaxStarts = 1
	swapTo(t, f)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		res, err := f.u.Startup(ctx, func(context.Context) error { return fmt.Errorf("hub down: %w", ErrInconclusive) })
		if res != Inconclusive || err == nil {
			t.Fatalf("%d: %v %v", i, res, err)
		}
	}
	if read(t, f.u.ExePath) != "NEW-BINARY-1.1.0" || !exists(f.u.ExePath+".update.json") {
		t.Fatal("inconclusive changed state")
	}
	if res, err := f.u.Startup(ctx, func(context.Context) error { return nil }); res != Committed || err != nil {
		t.Fatalf("%v %v", res, err)
	}
	if f.u.Denied("1.1.0") {
		t.Fatal("good version denied")
	}
}
