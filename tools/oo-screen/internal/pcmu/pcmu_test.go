package pcmu

import "testing"

// TestKnownCodewords — якорі з таблиці G.711. Без них round-trip нижче лише
// доводить, що Encode і Decode згодні МІЖ СОБОЮ, а не що це μ-law: пара
// «+1 / −1» пройшла б round-trip ідеально й не була б кодеком.
func TestKnownCodewords(t *testing.T) {
	cases := []struct {
		name string
		in   int16
		code byte
		out  int16
	}{
		// Знаковий біт у G.711 стоїть 1 для ВІДʼЄМНОГО і байт віддається
		// інвертованим — тому таблиця ulaw2linear починається з −32124 на
		// індексі 0x00, а +32124 лежить на 0x80, а не навпаки.
		{"нуль", 0, 0xFF, 0},
		{"тиша декодується в нуль", 0, Silence, 0},
		{"максимум додатний", 32767, 0x80, 32124},
		{"максимум відʼємний", -32768, 0x00, -32124},
	}
	for _, c := range cases {
		if got := Encode(c.in); got != c.code {
			t.Errorf("%s: Encode(%d) = %#02x, want %#02x", c.name, c.in, got, c.code)
		}
		if got := Decode(c.code); got != c.out {
			t.Errorf("%s: Decode(%#02x) = %d, want %d", c.name, c.code, got, c.out)
		}
	}
}

// TestRoundTripError — головна властивість кодека: помилка квантування росте
// РАЗОМ з амплітудою (логарифмічна шкала), і ніде не перевищує 8% від
// повної шкали. Саме це відрізняє робочий μ-law від зіпсованого: у зламаному
// експоненті помилка на тихих семплах злітає до тисяч.
func TestRoundTripError(t *testing.T) {
	for s := -32768; s <= 32767; s++ {
		in := int16(s)
		out := Decode(Encode(in))

		want := in
		if want > clip {
			want = clip
		}
		if want < -clip {
			want = -clip
		}
		diff := int(out) - int(want)
		if diff < 0 {
			diff = -diff
		}
		// Крок найгрубішого сегмента μ-law — 256; помилка не більша за
		// півкроку плюс сам зсув сегмента.
		limit := 4 + abs(int(want))/12
		if diff > limit {
			t.Fatalf("Encode/Decode(%d) = %d: помилка %d > межі %d", in, out, diff, limit)
		}
	}
}

// TestMonotonic — кодек мусить зберігати ПОРЯДОК: гучніше на вході не сміє
// стати тихішим на виході. Перестановка знаку чи мантиси це ловить одразу.
func TestMonotonic(t *testing.T) {
	prev := Decode(Encode(-32768))
	for s := -32767; s <= 32767; s++ {
		cur := Decode(Encode(int16(s)))
		if cur < prev {
			t.Fatalf("немонотонно на %d: %d після %d", s, cur, prev)
		}
		prev = cur
	}
}

func TestAppendSilence(t *testing.T) {
	b := AppendSilence(nil, FrameSamples)
	if len(b) != 160 {
		t.Fatalf("кадр 20мс = %d байт, want 160", len(b))
	}
	for i, v := range b {
		if Decode(v) != 0 {
			t.Fatalf("семпл тиші %d декодується в %d, want 0", i, Decode(v))
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
