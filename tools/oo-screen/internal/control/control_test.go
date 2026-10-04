package control

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Hello("t1-dev-token", 1)
	if err := Write(&buf, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("Write did not terminate line with \\n: %q", buf.String())
	}
	got, err := Read(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestReadRejectsUnterminatedEOF(t *testing.T) {
	// Валідний JSON-об'єкт, але БЕЗ завершального '\n' — імітує обрив
	// з'єднання рівно на межі повідомлення. Read має відкинути це як
	// помилку читання (io.EOF), а не тихо прийняти "хвіст".
	msg := Msg{V: Version, Type: TypeHeartbeat, Seq: 7}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := Read(bufio.NewReader(bytes.NewReader(b))) // без '\n'
	if err == nil {
		t.Fatalf("Read: expected error for unterminated line at EOF, got msg=%+v", got)
	}
}

func TestWriteDefaultsVersion(t *testing.T) {
	var buf bytes.Buffer
	m := Msg{Type: TypeHeartbeat, Seq: 5} // V left zero
	if err := Write(&buf, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.V != Version {
		t.Fatalf("expected default version %d, got %d", Version, got.V)
	}
}

func TestReadUnsupportedVersionFatal(t *testing.T) {
	line := `{"v":99,"type":"hello","seq":1}` + "\n"
	_, err := Read(bufio.NewReader(strings.NewReader(line)))
	if err == nil {
		t.Fatal("expected error for unsupported version")
	}
	var verr *ErrUnsupportedVersion
	if !errorsAs(err, &verr) {
		t.Fatalf("expected *ErrUnsupportedVersion, got %T: %v", err, err)
	}
	if verr.Got != 99 {
		t.Fatalf("expected Got=99, got %d", verr.Got)
	}
}

func TestReadKnownSkipsUnknownTypes(t *testing.T) {
	input := `{"v":1,"type":"future_thing","seq":1}` + "\n" +
		`{"v":1,"type":"another_unknown","seq":2}` + "\n" +
		`{"v":1,"type":"heartbeat","seq":3}` + "\n"
	var loggedCount int
	logf := func(format string, args ...any) { loggedCount++ }

	br := bufio.NewReader(strings.NewReader(input))
	m, err := ReadKnown(br, logf)
	if err != nil {
		t.Fatalf("ReadKnown: %v", err)
	}
	if m.Type != TypeHeartbeat || m.Seq != 3 {
		t.Fatalf("expected heartbeat seq=3, got %+v", m)
	}
	if loggedCount != 2 {
		t.Fatalf("expected 2 unknown-type log calls, got %d", loggedCount)
	}
}

func TestIsKnownType(t *testing.T) {
	for _, ty := range []string{
		TypeHello, TypeDecoderReady, TypeKeyframeRequest, TypeEpochChange,
		TypeHeartbeat, TypeBitrateTarget, TypeSelectOutput, TypeFallbackReason, TypeShutdown, TypeAck, TypeContentMode,
	} {
		if !IsKnownType(ty) {
			t.Errorf("expected %q to be known", ty)
		}
	}
	if IsKnownType("bogus") {
		t.Error("expected \"bogus\" to be unknown")
	}
}

// TestSelectOutputRoundTrip — вихід 0 (основний монітор) мусить доїхати НУЛЕМ, а
// не зникнути. Це і є негативний контроль до omitempty на Msg.Output: поле з
// дроту зникає, але розбирається назад у той самий 0, тож «перемкни на основний»
// не має перетворитись на «нічого не роби».
func TestSelectOutputRoundTrip(t *testing.T) {
	for _, idx := range []int{0, 1, 7} {
		var buf bytes.Buffer
		if err := Write(&buf, SelectOutput(3, idx)); err != nil {
			t.Fatalf("Write(%d): %v", idx, err)
		}
		got, err := Read(bufio.NewReader(&buf))
		if err != nil {
			t.Fatalf("Read(%d): %v", idx, err)
		}
		if got.Type != TypeSelectOutput || got.Seq != 3 || got.Output != idx {
			t.Fatalf("select_output round trip: got %+v, want output=%d", got, idx)
		}
	}
}

// TestSelectOutputNotOnOtherTypes — негативний контроль на конверт: нове поле не
// має протікати в чужі повідомлення. Інакше кожен heartbeat віз би "output":0.
func TestSelectOutputNotOnOtherTypes(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Heartbeat(1)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Contains(buf.String(), "output") {
		t.Fatalf("heartbeat не має нести output: %q", buf.String())
	}
}

// errorsAs is a tiny local wrapper to avoid importing "errors" just for As
// in a way that trips vet on unused import ordering in this small file.
func errorsAs(err error, target **ErrUnsupportedVersion) bool {
	if e, ok := err.(*ErrUnsupportedVersion); ok {
		*target = e
		return true
	}
	return false
}
