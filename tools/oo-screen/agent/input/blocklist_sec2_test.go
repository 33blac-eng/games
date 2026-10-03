package input

import "testing"

// Повторний аудит #37: блок-лист ловить Win незалежно від форми, в якій її
// прислав глядач (0xE05B або 0x5B+extended), а биті записи — помилка, не
// тихо порожній список.
func TestSec2BlocklistForms(t *testing.T) {
	m, err := ParseBlockedKeys(" 0xE05B , 0xe05c,")
	if err != nil {
		t.Fatal(err)
	}
	in := &Injector{blocked: m}
	for _, ev := range []Event{
		{V: Version, Kind: KindKey, Scancode: 0xE05B, Down: true},
		{V: Version, Kind: KindKey, Scancode: 0x5B, Extended: true, Down: true},
		{V: Version, Kind: KindKey, Scancode: 0xE05C},
	} {
		if !in.isBlocked(ev) {
			t.Errorf("не заблоковано: %+v", ev)
		}
	}
	if in.isBlocked(Event{V: Version, Kind: KindKey, Scancode: 0x5B}) {
		t.Error("незахищений 0x5B (не extended) хибно заблоковано — інша клавіша")
	}
	for _, bad := range []string{"0xE05B;0xE05C", "win", "0", "0x10000", "-1", "0xE05B,,x"} {
		if _, err := ParseBlockedKeys(bad); err == nil {
			t.Errorf("битий блок-лист %q прийнято", bad)
		}
	}
}
