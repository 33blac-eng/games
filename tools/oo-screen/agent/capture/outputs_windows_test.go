//go:build windows

package capture

import "testing"

// Список моніторів їде в offer, і консоль вибирає з нього індекс, який потім
// піде в capture.New. Тому Outputs() мусить збігатися з OutputCount() рядок у
// рядок: розбіжність тут = консоль пропонує монітор, якого капчер не відкриє.
//
// Негативні контролі всередині: нульовий розмір (oos_output_info віддав
// порожній DXGI_OUTPUT_DESC) і кількість основних моніторів ≠ 1 (прапорець
// узятий зі стелі, а не з GetMonitorInfo) валять тест.
func TestOutputsMatchCount(t *testing.T) {
	n, err := OutputCount()
	if err != nil {
		t.Skipf("DXGI тут недоступний: %v", err)
	}
	if n == 0 {
		t.Skip("виходів немає (headless / сесія 0) — перевіряти нічого")
	}

	list, err := Outputs()
	if err != nil {
		t.Fatalf("Outputs(): %v", err)
	}
	if len(list) != n {
		t.Fatalf("Outputs() віддав %d виходів, а OutputCount() каже %d", len(list), n)
	}

	primaries := 0
	for i, o := range list {
		if o.Index != i {
			t.Fatalf("вихід #%d має Index=%d — індекс у списку мусить бути тим самим, що приймає capture.New", i, o.Index)
		}
		if o.Width <= 0 || o.Height <= 0 {
			t.Fatalf("вихід %d: розмір %dx%d — DXGI_OUTPUT_DESC не прочитався", i, o.Width, o.Height)
		}
		if o.Primary {
			primaries++
		}
	}
	if primaries != 1 {
		t.Fatalf("основних моніторів %d із %d, має бути рівно 1", primaries, len(list))
	}
	t.Logf("виходів %d: %+v", len(list), list)
}
