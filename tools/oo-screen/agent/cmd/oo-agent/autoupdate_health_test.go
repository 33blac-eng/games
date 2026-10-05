package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/autoupdate"
)

func TestSessionHealth(t *testing.T) {
	// Connected -> healthy.
	c := make(chan struct{})
	close(c)
	if err := sessionHealth("127.0.0.1:1", c)(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Hub reachable but agent never connected -> real failure (rollback).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = sessionHealth(ln.Addr().String(), make(chan struct{}))(ctx)
	if err == nil || errors.Is(err, autoupdate.ErrInconclusive) {
		t.Fatalf("reachable, no session: %v", err)
	}
	// Hub unreachable -> inconclusive (no rollback of a possibly good build).
	addr := ln.Addr().String()
	ln.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := sessionHealth(addr, make(chan struct{}))(ctx2); !errors.Is(err, autoupdate.ErrInconclusive) {
		t.Fatalf("unreachable: %v", err)
	}
	// Agent shutting down before the window ends -> inconclusive.
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancel3()
	if err := sessionHealth("127.0.0.1:1", make(chan struct{}))(ctx3); !errors.Is(err, autoupdate.ErrInconclusive) {
		t.Fatalf("canceled: %v", err)
	}
}

// startExe must be the path captured before any rename; os.Executable on
// Linux follows the rename of the running file (a -> a.old).
func TestStartExeSurvivesRename(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/exe semantics")
	}
	if os.Getenv("OO_HELPER_EXE") == "1" {
		before := startExe
		_ = os.Rename(before, before+".old")
		after, _ := os.Executable()
		os.Stdout.WriteString(before + "\n" + after + "\n")
		os.Exit(0)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestStartExeSurvivesRename$")
	cmd.Env = append(os.Environ(), "OO_HELPER_EXE=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(out))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || lines[0] != bin {
		t.Fatalf("startExe = %q, want %q", lines, bin)
	}
	if lines[1] == bin {
		t.Log("os.Executable did not follow rename here; startExe still correct")
	}
}
