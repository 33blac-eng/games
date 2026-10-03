package input

// held.go — SEC #37: what the operator is holding down right now, and how to
// let go of it when the session ends.
//
// A key-down whose key-up never arrives (the viewer's tab crashed, the network
// dropped, the DataChannel closed) leaves Windows believing Ctrl or the left
// button is still pressed: the next person at the physical machine types
// shortcuts and drags windows without meaning to. The injector therefore
// remembers every key and button it pressed and ReleaseAll sends the matching
// up-events. Tracking is pure Go so it is tested on any platform; only the
// final SendInput call is Windows-specific.
//
// Key combinations themselves stay pass-through by design ("full control"),
// but OO_AGENT_INPUT_BLOCK_KEYS can list scancodes that are never injected,
// e.g. "0xE05B,0xE05C" for both Windows keys.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// sendRaw is the SendInput call; a variable so tests can observe ReleaseAll.
var sendRaw = sendInputs

// ErrKeyBlocked — the key is on the operator-configured blocklist.
var ErrKeyBlocked = errors.New("input: key blocked by OO_AGENT_INPUT_BLOCK_KEYS")

// keyID identifies a physical key after normalizeScancode, or a character
// typed with KEYEVENTF_UNICODE (scan==0, uni!=0).
type keyID struct {
	scan     uint16
	extended bool
	uni      rune
}

func keyOf(ev Event) keyID {
	if ev.Scancode != 0 {
		s, ext := normalizeScancode(ev.Scancode, ev.Extended)
		return keyID{scan: s, extended: ext}
	}
	return keyID{uni: ev.Unicode}
}

// heldState is guarded by Injector.mu.
type heldState struct {
	keys    map[keyID]struct{}
	buttons map[Button]struct{}
}

// track records what a successfully injected event left pressed.
func (h *heldState) track(ev Event) {
	switch ev.Kind {
	case KindKey:
		k := keyOf(ev)
		if ev.Down {
			if h.keys == nil {
				h.keys = make(map[keyID]struct{})
			}
			h.keys[k] = struct{}{}
		} else {
			delete(h.keys, k)
		}
	case KindMouseButton:
		if ev.Down {
			if h.buttons == nil {
				h.buttons = make(map[Button]struct{})
			}
			h.buttons[ev.Button] = struct{}{}
		} else {
			delete(h.buttons, ev.Button)
		}
	}
}

// releaseInputs returns the up-events for everything held and forgets it.
func (h *heldState) releaseInputs() []rawInput {
	var out []rawInput
	for k := range h.keys {
		if k.scan != 0 {
			flags := uint32(keyScancode | keyUp)
			if k.extended {
				flags |= keyExtended
			}
			out = append(out, keyEvent(k.scan, flags))
			continue
		}
		out = append(out, keyInputs(Event{V: Version, Kind: KindKey, Unicode: k.uni})...)
	}
	for b := range h.buttons {
		codes := buttons[b]
		out = append(out, mouseEvent(0, 0, codes.data, codes.up))
	}
	h.keys, h.buttons = nil, nil
	return out
}

// Held reports how many keys and mouse buttons are currently held.
func (in *Injector) Held() (keys, buttons int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.held.keys), len(in.held.buttons)
}

// ReleaseAll sends key-up / button-up for everything this injector pressed and
// did not release. Call it when the input channel closes or the last viewer
// leaves. Safe to call repeatedly; with nothing held it does nothing.
func (in *Injector) ReleaseAll() error {
	in.mu.Lock()
	inputs := in.held.releaseInputs()
	in.mu.Unlock()
	if len(inputs) == 0 {
		return nil
	}
	return sendRaw(inputs)
}

// ParseBlockedKeys parses a comma-separated scancode list ("0xE05B, 91").
func ParseBlockedKeys(s string) (map[keyID]struct{}, error) {
	out := make(map[keyID]struct{})
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.ParseUint(f, 0, 16)
		if err != nil || v == 0 {
			return nil, fmt.Errorf("input: bad scancode %q in blocklist", f)
		}
		sc, ext := normalizeScancode(uint16(v), false)
		out[keyID{scan: sc, extended: ext}] = struct{}{}
	}
	return out, nil
}

// SetBlockedKeys replaces the blocklist (nil/empty = nothing blocked).
func (in *Injector) SetBlockedKeys(m map[keyID]struct{}) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.blocked = m
}

func (in *Injector) isBlocked(ev Event) bool {
	if ev.Kind != KindKey || ev.Scancode == 0 {
		return false
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	_, ok := in.blocked[keyOf(ev)]
	return ok
}

// blockedKeysFromEnv reads OO_AGENT_INPUT_BLOCK_KEYS; a malformed list is an
// error so a typo cannot silently leave the Windows key enabled.
func blockedKeysFromEnv() (map[keyID]struct{}, error) {
	return ParseBlockedKeys(os.Getenv("OO_AGENT_INPUT_BLOCK_KEYS"))
}
