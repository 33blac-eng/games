package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/organicoils/oo-screen/hub"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSingleNodeFromSecretFile(t *testing.T) {
	sf := writeFile(t, "m", "master-A\r\n")
	var out, errb bytes.Buffer
	if rc := run([]string{"-secret-file", sf, "-node", "PC-1"}, env(nil), &out, &errb); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != hub.NodeToken("master-A", "PC-1") {
		t.Fatalf("token %q", got)
	}
}

func TestEnvSecretFileAndFallbackWarning(t *testing.T) {
	sf := writeFile(t, "m", "master-B")
	var out, errb bytes.Buffer
	if rc := run([]string{"-node", "n"}, env(map[string]string{"OO_SCREEN_AGENT_SECRET_FILE": sf}), &out, &errb); rc != 0 {
		t.Fatal(errb.String())
	}
	if strings.TrimSpace(out.String()) != hub.NodeToken("master-B", "n") {
		t.Fatal("SECRET_FILE не використано")
	}
	out.Reset()
	errb.Reset()
	if rc := run([]string{"-node", "n"}, env(map[string]string{"OO_SCREEN_T1_TOKEN": "t1"}), &out, &errb); rc != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(errb.String(), "WARNING") {
		t.Fatal("нема попередження про T1-фолбек")
	}
	if rc := run([]string{"-node", "n"}, env(nil), &out, &errb); rc != 1 {
		t.Fatalf("без master rc=%d", rc)
	}
}

func TestBatchNodesFile(t *testing.T) {
	sf := writeFile(t, "m", "M")
	nf := writeFile(t, "nodes", "# парк\r\nPC-1\r\n\r\nPC-2\nPC-1\nbad\x01id\n")
	var out, errb bytes.Buffer
	rc := run([]string{"-secret-file", sf, "-nodes-file", nf}, env(nil), &out, &errb)
	if rc != 1 {
		t.Fatalf("битий id мусить дати rc=1, а не %d", rc)
	}
	want := "PC-1\t" + hub.NodeToken("M", "PC-1") + "\nPC-2\t" + hub.NodeToken("M", "PC-2") + "\n"
	if out.String() != want {
		t.Fatalf("вивід:\n%q\nхотіли\n%q", out.String(), want)
	}
	if !strings.Contains(errb.String(), ":6:") {
		t.Fatalf("помилка без номера рядка: %s", errb.String())
	}
}

func TestVerifyAndRotation(t *testing.T) {
	cur := writeFile(t, "cur", "NEW")
	prev := writeFile(t, "prev", "OLD")
	var out, errb bytes.Buffer
	if rc := run([]string{"-secret-file", cur, "-node", "PC-1", "-verify", hub.NodeToken("NEW", "PC-1")}, env(nil), &out, &errb); rc != 0 {
		t.Fatalf("свій токен: rc=%d", rc)
	}
	oldTok := hub.NodeToken("OLD", "PC-1")
	if rc := run([]string{"-secret-file", cur, "-node", "PC-1", "-verify", oldTok}, env(nil), &out, &errb); rc != 1 {
		t.Fatal("старий токен без -prev-secret-file прийнято")
	}
	out.Reset()
	if rc := run([]string{"-secret-file", cur, "-prev-secret-file", prev, "-node", "PC-1", "-verify", oldTok}, env(nil), &out, &errb); rc != 0 || !strings.Contains(out.String(), "СТАРИЙ") {
		t.Fatalf("старий master не розпізнано: rc=%d %s", rc, out.String())
	}
	if rc := run([]string{"-secret-file", cur, "-node", "PC-2", "-verify", hub.NodeToken("NEW", "PC-1")}, env(nil), &out, &errb); rc != 1 {
		t.Fatal("токен чужої ноди прийнято")
	}
}

func TestUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	for _, a := range [][]string{{}, {"-node", "a", "-nodes-file", "x"}, {"-nodes-file", "x", "-verify", "t"}, {"-node", "a\nb"}} {
		if rc := run(a, env(map[string]string{"OO_SCREEN_AGENT_SECRET": "s"}), &out, &errb); rc != 2 {
			t.Errorf("%q: rc=%d, хотіли 2", a, rc)
		}
	}
}

func TestGenSecret(t *testing.T) {
	var a, b, errb bytes.Buffer
	run([]string{"-gen-secret"}, env(nil), &a, &errb)
	run([]string{"-gen-secret"}, env(nil), &b, &errb)
	s := strings.TrimSpace(a.String())
	if len(s) != 64 || s == strings.TrimSpace(b.String()) {
		t.Fatalf("секрет %q", s)
	}
}
