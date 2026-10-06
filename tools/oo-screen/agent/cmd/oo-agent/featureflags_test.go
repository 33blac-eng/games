package main

import "testing"

// Прапорці командного рядка — ЄДИНИЙ спосіб увімкнути звук і ввід на бойовому
// ПК: агента там запускає Register-ScheduledTask, яка передає лише аргументи,
// без змінних середовища. Якщо ці тести червоніють, обидві фічі написані й
// недосяжні.
func TestFeatureFlagsEnable(t *testing.T) {
	prevA, prevI := audioEnabled, inputEnabled
	defer func() { audioEnabled, inputEnabled = prevA, prevI }()

	audioEnabled, inputEnabled = false, false
	applyFeatureFlags(true, true)
	if !audioEnabled {
		t.Error("-audio не ввімкнув звук")
	}
	if !inputEnabled {
		t.Error("-input не ввімкнув ввід")
	}
}

// Негативний контроль №1: без прапорців нічого не вмикається само.
func TestFeatureFlagsStayOffByDefault(t *testing.T) {
	prevA, prevI := audioEnabled, inputEnabled
	defer func() { audioEnabled, inputEnabled = prevA, prevI }()

	audioEnabled, inputEnabled = false, false
	applyFeatureFlags(false, false)
	if audioEnabled || inputEnabled {
		t.Fatalf("фіча ввімкнулась без прапорця: audio=%v input=%v", audioEnabled, inputEnabled)
	}
}

// Негативний контроль №2: прапорець лише вмикає. -audio=false не сміє гасити
// те, що людина ввімкнула через OO_SCREEN_AUDIO у своєму середовищі.
func TestFeatureFlagsNeverDisableEnv(t *testing.T) {
	prevA, prevI := audioEnabled, inputEnabled
	defer func() { audioEnabled, inputEnabled = prevA, prevI }()

	audioEnabled, inputEnabled = true, true
	applyFeatureFlags(false, false)
	if !audioEnabled || !inputEnabled {
		t.Fatalf("прапорець вимкнув фічу з env: audio=%v input=%v", audioEnabled, inputEnabled)
	}
}

// Прапорці незалежні: -audio не вмикає ввід (канал керування чужим ПК не сміє
// приїхати причепом до звуку).
func TestFeatureFlagsIndependent(t *testing.T) {
	prevA, prevI := audioEnabled, inputEnabled
	defer func() { audioEnabled, inputEnabled = prevA, prevI }()

	audioEnabled, inputEnabled = false, false
	applyFeatureFlags(true, false)
	if inputEnabled {
		t.Fatal("-audio увімкнув ВВІД — канал керування чужим ПК не сміє приїжджати причепом")
	}

	// І назад. Обидва напрямки перевіряються окремо не з педантизму: перший
	// негативний контроль цього гейта зламав саме цю гілку — і гейт лишився
	// зеленим, бо дивився лише в один бік.
	audioEnabled, inputEnabled = false, false
	applyFeatureFlags(false, true)
	if audioEnabled {
		t.Fatal("-input увімкнув ЗВУК — мікрофон чужої кімнати не сміє приїжджати причепом")
	}
}
