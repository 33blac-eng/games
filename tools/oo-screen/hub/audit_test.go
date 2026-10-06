package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuditChainAppendVerifyReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := OpenAuditLog(p)
	if err != nil {
		t.Fatal(err)
	}
	s := a.StartSession(AuditRecord{Node: "pc1", User: "u1", Grant: "control"})
	s.Count("input_accepted", 3)
	s.Count("input_dropped", 1)
	s.End("viewer closed")
	s.End("again") // no-op
	a.Close()

	seq, last, err := VerifyAuditFile(p)
	if err != nil || seq != 2 || last == "" {
		t.Fatalf("verify: seq=%d err=%v", seq, err)
	}
	// reopen continues chain
	a2, err := OpenAuditLog(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := a2.Append(AuditRecord{Event: "control_select_output", Node: "pc1"}); err != nil {
		t.Fatal(err)
	}
	a2.Close()
	if seq, _, err := VerifyAuditFile(p); err != nil || seq != 3 {
		t.Fatalf("after reopen: seq=%d err=%v", seq, err)
	}
	data, _ := os.ReadFile(p)
	var end AuditRecord
	_ = json.Unmarshal(bytes.Split(data, []byte("\n"))[1], &end)
	if end.Event != "session_end" || end.Counts["input_accepted"] != 3 || end.User != "u1" {
		t.Fatalf("bad end record: %+v", end)
	}
}

func TestAuditTamperDetected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a, _ := OpenAuditLog(p)
	for i := 0; i < 5; i++ {
		_ = a.Append(AuditRecord{Event: "session_start", Node: "pc1", User: "alice"})
	}
	a.Close()
	orig, _ := os.ReadFile(p)
	lines := bytes.Split(bytes.TrimSpace(orig), []byte("\n"))

	cases := map[string][]byte{
		"edit field":  bytes.Replace(orig, []byte(`"alice"`), []byte(`"mallo"`), 1),
		"delete line": bytes.Join(append(append([][]byte{}, lines[:2]...), lines[3:]...), []byte("\n")),
		"swap lines":  bytes.Join([][]byte{lines[0], lines[2], lines[1], lines[3], lines[4]}, []byte("\n")),
	}
	for name, mod := range cases {
		if _, _, err := VerifyAudit(bytes.NewReader(mod)); err == nil {
			t.Errorf("%s: tamper not detected", name)
		}
		bad := filepath.Join(t.TempDir(), "bad.jsonl")
		_ = os.WriteFile(bad, mod, 0o600)
		if _, err := OpenAuditLog(bad); err == nil {
			t.Errorf("%s: OpenAuditLog accepted tampered log", name)
		}
	}
}

func TestAuditConcurrentAppend(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a, _ := OpenAuditLog(p)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = a.Append(AuditRecord{Event: "x"})
			}
		}()
	}
	wg.Wait()
	a.Close()
	if seq, _, err := VerifyAuditFile(p); err != nil || seq != 200 {
		t.Fatalf("seq=%d err=%v", seq, err)
	}
}

func TestAuditHandlerAuthAndFilter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a, _ := OpenAuditLog(p)
	defer a.Close()
	_ = a.Append(AuditRecord{Event: "session_start", Node: "pc1", User: "u1"})
	_ = a.Append(AuditRecord{Event: "session_start", Node: "pc2", User: "u2"})

	if rr := do(a.Handler(""), "", "/admin/audit"); rr.Code != http.StatusNotFound {
		t.Fatalf("no token configured: %d", rr.Code)
	}
	var nilLog *AuditLog
	if rr := do(nilLog.Handler("s"), "s", "/admin/audit"); rr.Code != http.StatusNotFound {
		t.Fatalf("nil log: %d", rr.Code)
	}
	h := a.Handler("secret")
	for _, tok := range []string{"", "wrong", "secretX"} {
		if rr := do(h, tok, "/admin/audit"); rr.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, rr.Code)
		}
	}
	rr := do(h, "secret", "/admin/audit?node=pc2&verify=1")
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	var out struct {
		Records  []AuditRecord `json:"records"`
		Verified *bool         `json:"verified"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Records) != 1 || out.Records[0].User != "u2" || out.Verified == nil || !*out.Verified {
		t.Fatalf("bad body: %s", rr.Body.String())
	}
	if rr := do(h, "secret", "/admin/audit?since="+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); !strings.Contains(rr.Body.String(), `"records":[]`) {
		t.Fatalf("since filter: %s", rr.Body.String())
	}
}

func do(h http.HandlerFunc, tok, url string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func BenchmarkAuditAppend(b *testing.B) {
	a, _ := OpenAuditLog(filepath.Join(b.TempDir(), "a.jsonl"))
	defer a.Close()
	for b.Loop() {
		_ = a.Append(AuditRecord{Event: "session_end", Node: "pc1", User: "u", Counts: map[string]int64{"input_accepted": 10}})
	}
}

// S4, wave 8: the tail anchor catches what the chain alone cannot — a
// truncated tail, a log rewritten from the anchor on, or a deleted log.
func TestAuditAnchorDetectsTruncation(t *testing.T) {
	dir := t.TempDir()
	p, anc := filepath.Join(dir, "audit.jsonl"), filepath.Join(dir, "anchor.json")
	var seen []uint64
	on := func(seq uint64, _ string) { seen = append(seen, seq) }
	a, err := OpenAuditLogAnchored(p, anc, on)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := a.Append(AuditRecord{Event: "session_start", Node: "pc1"}); err != nil {
			t.Fatal(err)
		}
	}
	a.Close()
	got, err := ReadAuditAnchor(anc)
	if err != nil || got.Seq != 3 || len(seen) != 3 {
		t.Fatalf("anchor=%+v err=%v seen=%v", got, err, seen)
	}
	// Clean reopen continues and moves the anchor (also re-announced on start).
	a, err = OpenAuditLogAnchored(p, anc, on)
	if err != nil {
		t.Fatalf("clean reopen: %v", err)
	}
	_ = a.Append(AuditRecord{Event: "session_end", Node: "pc1"})
	a.Close()
	if got, _ := ReadAuditAnchor(anc); got.Seq != 4 {
		t.Fatalf("anchor after reopen: %+v", got)
	}
	data, _ := os.ReadFile(p)
	lines := bytes.SplitAfter(data, []byte("\n"))

	// 1) Truncated tail: the chain alone still verifies.
	if err := os.WriteFile(p, bytes.Join(lines[:2], nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyAuditFile(p); err != nil {
		t.Fatalf("precondition: truncated chain must still self-verify: %v", err)
	}
	if _, err := OpenAuditLogAnchored(p, anc, nil); !errors.Is(err, ErrAuditTruncated) {
		t.Fatalf("truncated tail not detected: %v", err)
	}
	// 2) Rewritten from the anchor on: same length, different tail.
	_ = os.WriteFile(p, bytes.Join(lines[:3], nil), 0o600)
	b, _ := OpenAuditLog(p)
	_ = b.Append(AuditRecord{Event: "session_end", Node: "forged"})
	b.Close()
	if _, err := OpenAuditLogAnchored(p, anc, nil); !errors.Is(err, ErrAuditTruncated) {
		t.Fatalf("rewritten tail not detected: %v", err)
	}
	// 3) Log deleted.
	_ = os.Remove(p)
	if _, err := OpenAuditLogAnchored(p, anc, nil); !errors.Is(err, ErrAuditTruncated) {
		t.Fatalf("deleted log not detected: %v", err)
	}
	// Garbage anchor = error, not a silent reset.
	_ = os.WriteFile(anc, []byte("{}"), 0o600)
	if _, err := OpenAuditLogAnchored(p, anc, nil); err == nil {
		t.Fatal("broken anchor accepted")
	}
	// No anchor configured: behaviour of OpenAuditLog.
	if a, err := OpenAuditLogAnchored(filepath.Join(dir, "x.jsonl"), "", nil); err != nil || a.anchor != "" {
		t.Fatalf("no-anchor mode: %v", err)
	} else {
		a.Close()
	}
}
