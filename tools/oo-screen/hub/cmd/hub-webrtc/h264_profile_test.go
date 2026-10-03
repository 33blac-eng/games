package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCodecMismatchIsMachineReadable — 05.09.2026 фронт бачив голе «415» без
// причини, мовчки падав у MeshCentral (де звуку немає) і причину шукали 2.5
// години. Тіло 415 мусить нести КОД, профілі глядача і профіль хаба.
//
// Червоніє, якщо повернути текстову відповідь замість структури.
func TestCodecMismatchIsMachineReadable(t *testing.T) {
	why := videoCodecMismatch(sdpWithVideo(
		"m=video 9 UDP/TLS/RTP/SAVPF 102",
		"a=rtpmap:102 H264/90000",
		"a=fmtp:102 packetization-mode=1;profile-level-id=42e01f"), wantedProfileLevelID)
	if why == nil {
		t.Fatal("baseline-глядач мусив бути відхилений")
	}
	b, err := json.Marshal(why)
	if err != nil {
		t.Fatalf("тіло 415 не серіалізується: %v", err)
	}
	body := string(b)
	for _, want := range []string{`"error":"h264_profile_mismatch"`, `"42e01f"`, wantedProfileLevelID} {
		if !strings.Contains(body, want) {
			t.Fatalf("у тілі 415 немає %q: %s", want, body)
		}
	}
}

// TestHealthzShowsRejectedProfile — лічильник відмов мусить бути видимий
// монітору (deploy/ops/oo_screen_health.py), а не лише в журналі сервера.
func TestHealthzShowsRejectedProfile(t *testing.T) {
	prev := reg
	reg = newRegistry()
	t.Cleanup(func() { reg = prev })

	before := rejectedProfileTotal.Load()
	rejectedProfileTotal.Add(1)
	rejectedProfileAt.Store(1757030000)
	t.Cleanup(func() { rejectedProfileTotal.Store(before); rejectedProfileAt.Store(0) })

	rr := httptest.NewRecorder()
	handleHealthz(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("healthz не JSON: %v", err)
	}
	if v, ok := got["rejected_profile_total"].(float64); !ok || v != float64(before+1) {
		t.Fatalf("rejected_profile_total = %v (%T), чекали %d", got["rejected_profile_total"], got["rejected_profile_total"], before+1)
	}
	if v, ok := got["rejected_profile_at"].(float64); !ok || v != 1757030000 {
		t.Fatalf("rejected_profile_at = %v, чекали 1757030000", got["rejected_profile_at"])
	}
}
