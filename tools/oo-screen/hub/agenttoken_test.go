package hub

import "testing"

func TestNodeTokenBoundToNode(t *testing.T) {
	a := NodeToken("m", "A")
	if len(a) != 64 || a == NodeToken("m", "B") || a == NodeToken("m2", "A") {
		t.Fatalf("токен не привʼязаний до ноди/секрету: %s", a)
	}
	if !NodeTokenValid("m", "A", a) || NodeTokenValid("m", "B", a) || NodeTokenValid("", "A", NodeToken("", "A")) || NodeTokenValid("m", "", NodeToken("m", "")) {
		t.Fatal("NodeTokenValid поводиться неправильно")
	}
}
