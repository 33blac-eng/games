//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// C1, агентська половина: max_fps з хаба доходить до кадрового циклу через
// handleCtlMessage -> setMaxFps -> maxFpsGap. Зніми гілку TypeMaxFps у
// handleCtlMessage — стеля лишиться нулем; зніми TypeMaxFps з knownTypes
// (internal/control) — parseCtlJSON відкине повідомлення так само.
func TestMaxFpsReachesFrameLoop(t *testing.T) {
	t.Cleanup(func() { maxFpsWanted.Store(0) })

	handleCtlMessage([]byte(`{"v":1,"type":"max_fps","seq":3,"fps":10}`+"\n"), nil, nil, nil, nil)
	if got := maxFpsWanted.Load(); got != 10 {
		t.Fatalf("max_fps не дійшов: стеля %d, want 10", got)
	}
	if gap := maxFpsGap(int(maxFpsWanted.Load()), 30); gap != 100*time.Millisecond {
		t.Fatalf("проміжок при 10 к/с = %v, want 100ms", gap)
	}

	// Стеля вище за власний -fps агента нічого не притискає (хаб так знімає стелю: 60).
	handleCtlMessage([]byte(`{"v":1,"type":"max_fps","seq":4,"fps":60}`+"\n"), nil, nil, nil, nil)
	if gap := maxFpsGap(int(maxFpsWanted.Load()), 30); gap != 0 {
		t.Fatalf("fps=60 при -fps=30 притиснув цикл: %v", gap)
	}

	// Сміття поза 1..60 = без стелі, а не «0 кадрів/с».
	setMaxFps(10)
	setMaxFps(500)
	if got := maxFpsWanted.Load(); got != 0 {
		t.Fatalf("fps=500 -> стеля %d, want 0 (без стелі)", got)
	}
}

// TestMaxFpsResetOnNewHubSession — R5-G6: глядач поставив «Швидкість» 5 к/с,
// хаб перезалили. Новий хаб стелі не має і на OnOpen нічого не шле, тож агент
// мусить скинути її сам на новому dial — навіть якщо той dial ще не вдався
// (хаб піднімається). Прибери setMaxFps(0) з dialWebRTC — стеля 5 переживе
// рестарт хаба назавжди.
func TestMaxFpsResetOnNewHubSession(t *testing.T) {
	t.Cleanup(func() { maxFpsWanted.Store(0) })
	setMaxFps(5)

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "hub restarting", http.StatusServiceUnavailable)
	}))
	defer hub.Close()
	if tr, err := dialWebRTC(hub.URL, time.Second/30, nil, nil, nil, nil, nil); err == nil {
		tr.close()
		t.Fatal("dial до хаба, що відповідає 503, вдався")
	}
	if got := maxFpsWanted.Load(); got != 0 {
		t.Fatalf("після нового dial стеля %d, want 0 (новий хаб стелі не має)", got)
	}
}
