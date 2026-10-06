package control

import "testing"

func TestParseEncCaps(t *testing.T) {
	got := ParseEncCaps("ROIEnabled=M DirtyRectEnabled=- MaxQP=S Bogus=M MinQP=? junk")
	want := map[string]string{"ROIEnabled": CapModifiable, "DirtyRectEnabled": CapUnsupported, "MaxQP": CapSupported}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%q, want %q", k, got[k], v)
		}
	}
	if len(ParseEncCaps("")) != 0 {
		t.Fatal("порожній звіт")
	}
}
