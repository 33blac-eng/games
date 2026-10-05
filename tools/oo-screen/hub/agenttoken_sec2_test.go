package hub

import "testing"

// Повторний аудит #17: токен ноди звʼязаний з ТОЧНИМИ байтами node_id —
// жодної канонікалізації, яка дала б чужій ноді прийняти той самий токен.
func TestSec2NodeTokenNoCanonicalisation(t *testing.T) {
	const m = "master-secret"
	tok := NodeToken(m, "pc1")
	if !NodeTokenValid(m, "pc1", tok) {
		t.Fatal("власний токен не прийнято")
	}
	for _, other := range []string{"PC1", "pc1 ", " pc1", "pc1\x00", "pс1" /* кирилична с */, "ｐｃ1", "pc1\n", ""} {
		if NodeTokenValid(m, other, tok) {
			t.Fatalf("токен pc1 прийнято для %q", other)
		}
	}
	if NodeTokenValid(m, "pc1", tok+" ") || NodeTokenValid(m, "pc1", tok[:63]) {
		t.Fatal("змінений токен прийнято")
	}
	if NodeTokenValid("", "pc1", NodeToken("", "pc1")) {
		t.Fatal("порожній master прийнято")
	}
	if NodeTokenValid(m, "", NodeToken(m, "")) {
		t.Fatal("порожній node_id прийнято")
	}
}
