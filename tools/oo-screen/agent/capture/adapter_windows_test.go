//go:build windows

package capture

import (
	"strings"
	"testing"
)

// Лічильник виходів і власне відкриття мусять дивитись на ОДИН адаптер.
// Поки кожен обирав його самостійно (count — EnumAdapters1(0), open — той, що
// віддавав D3D11CreateDevice(NULL)), на одногепешній машині це збігалось, а на
// гібридному ноутбуці (Intel iGPU + дискретна NVIDIA/AMD) — ні: count рахував
// монітори одного чипа, EnumOutputs питав інший і падав у DXGI_ERROR_NOT_FOUND.
//
// Тест ловить саме цю розбіжність. На машині з одним GPU він проходить
// тривіально — на гібридній він і є той гейт, що зачервонів би.
func TestOutputCountMatchesOpen(t *testing.T) {
	n, err := OutputCount()
	if err != nil {
		t.Skipf("DXGI тут недоступний: %v", err)
	}
	if n == 0 {
		t.Skip("виходів немає (headless / сесія 0) — перевіряти нічого")
	}

	for i := 0; i < n; i++ {
		c, err := New(i)
		if err == nil {
			c.Close()
			continue
		}
		if strings.Contains(err.Error(), "EnumOutputs") {
			t.Fatalf("вихід %d: OutputCount()=%d його називає, а oos_open не знаходить — адаптери розійшлись: %v", i, n, err)
		}
		// RDP / secure desktop / нема D3D11 — не про адаптер, тест не про це.
		t.Logf("вихід %d: не відкрився з іншої причини: %v", i, err)
	}

	// Негативний контроль: індекс поза межами МУСИТЬ впасти саме на EnumOutputs.
	c, err := New(n)
	switch {
	case err == nil:
		c.Close()
		t.Fatalf("виходу %d не існує (їх %d), а oos_open його відкрив", n, n)
	case strings.Contains(err.Error(), "EnumOutputs"):
		// очікувано
	case strings.Contains(err.Error(), "D3D11CreateDevice"),
		strings.Contains(err.Error(), "EnumAdapters1"):
		t.Skipf("D3D11-пристрій тут не створюється, негативний контроль недосяжний: %v", err)
	default:
		t.Fatalf("вихід %d мав впасти на EnumOutputs, а впав інакше: %v", n, err)
	}
}
