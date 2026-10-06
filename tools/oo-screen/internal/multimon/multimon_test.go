package multimon

import "testing"

func TestRoundTrip(t *testing.T) {
	for i := 1; i <= MaxIndex; i++ {
		b, n, ok := Parse(NodeID("pc-1", i))
		if !ok || b != "pc-1" || n != i {
			t.Fatalf("idx %d: %q %d %v", i, b, n, ok)
		}
	}
	if NodeID("pc", 0) != "pc" {
		t.Fatal("idx 0 must be the node itself")
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "pc", "#m1", "pc#m0", "pc#m01", "pc#m16", "pc#m", "pc#mx", "pc#m-1", "pc#m100", "a#m1#m2"} {
		if _, _, ok := Parse(s); ok {
			t.Errorf("%q parsed as monitor stream", s)
		}
	}
}
