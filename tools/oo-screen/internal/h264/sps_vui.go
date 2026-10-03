package h264

import "errors"

// RewriteSPSColourBT709 повертає SPS NAL (без стартового коду), у VUI якого
// явно записано BT.709: colour_primaries = transfer_characteristics =
// matrix_coefficients = 1 і video_full_range_flag = 0 (limited). ТЗ 1.3 / P3.
//
// Усе інше зберігається біт-у-біт: заголовок NAL, усі поля до VUI, у VUI —
// aspect_ratio і overscan до video_signal_type, та все після нього
// (chroma_loc, timing, HRD, bitstream_restriction) аж до rbsp_stop_one_bit.
// video_format зберігається, якщо був; інакше пишеться 5 (unspecified —
// значення за замовчуванням, тобто семантика не змінюється).
//
// Якщо VUI немає зовсім — додається мінімальний VUI, де всі інші
// *_present_flag = 0 (рівно те, що декодер і так виводить за відсутності VUI).
//
// Якщо SPS уже сигналізує саме це — повертається копія без змін (ідемпотентно).
// Профіль/рівень/геометрія не чіпаються. Результат заново екранується
// (emulation prevention).
func RewriteSPSColourBT709(nal []byte) ([]byte, error) {
	s, err := ParseSPS(nal)
	if err != nil {
		return nil, err
	}
	if s.VUIPresent && s.VideoSignalTypePresent && s.ColourDescriptionPresent && !s.FullRange &&
		s.ColourPrimaries == 1 && s.TransferCharacteristics == 1 && s.MatrixCoefficients == 1 {
		return append([]byte(nil), nal...), nil
	}
	rbsp := unescapeRBSP(nal[1:])
	stop := lastOneBit(rbsp)
	if stop < 0 || stop < s.vuiFlagPos {
		return nil, errors.New("h264: SPS without rbsp_stop_one_bit")
	}
	r := &bitReader{b: rbsp}
	w := &bitWriter{}
	copyBits := func(n int) {
		for ; n > 0; n-- {
			w.put(r.bits(1), 1)
		}
	}
	copyBits(s.vuiFlagPos)
	writeVST := func(videoFormat int) {
		w.put(1, 1)           // video_signal_type_present_flag
		w.put(videoFormat, 3) // video_format
		w.put(0, 1)           // video_full_range_flag = 0 (limited)
		w.put(1, 1)           // colour_description_present_flag
		w.put(1, 8)           // colour_primaries = BT.709
		w.put(1, 8)           // transfer_characteristics = BT.709
		w.put(1, 8)           // matrix_coefficients = BT.709
	}
	if r.bits(1) == 0 { // VUI не було
		w.put(1, 1) // vui_parameters_present_flag
		w.put(0, 1) // aspect_ratio_info_present_flag
		w.put(0, 1) // overscan_info_present_flag
		writeVST(5)
		w.put(0, 1) // chroma_loc_info_present_flag
		w.put(0, 1) // timing_info_present_flag
		w.put(0, 1) // nal_hrd_parameters_present_flag
		w.put(0, 1) // vcl_hrd_parameters_present_flag
		w.put(0, 1) // bitstream_restriction_flag
		// між vui-прапорцем і stop-бітом у старому SPS нічого не було
		r.pos = stop
	} else {
		w.put(1, 1)
		if r.bits(1) == 1 { // aspect_ratio_info_present_flag
			w.put(1, 1)
			idc := r.bits(8)
			w.put(idc, 8)
			if idc == 255 {
				copyBits(32)
			}
		} else {
			w.put(0, 1)
		}
		if r.bits(1) == 1 { // overscan_info_present_flag
			w.put(1, 1)
			copyBits(1)
		} else {
			w.put(0, 1)
		}
		vf := 5
		if r.bits(1) == 1 { // старий video_signal_type пропускаємо
			vf = r.bits(3)
			r.bits(1)
			if r.bits(1) == 1 {
				r.bits(24)
			}
		}
		writeVST(vf)
		copyBits(stop - r.pos) // решта VUI біт-у-біт
	}
	if r.err != nil {
		return nil, r.err
	}
	w.put(1, 1) // rbsp_stop_one_bit
	for w.n%8 != 0 {
		w.put(0, 1) // rbsp_alignment_zero_bit
	}
	out := append([]byte{nal[0]}, escapeRBSP(w.b)...)
	return out, nil
}

// RewriteAnnexBSPSColourBT709 переписує кожен SPS NAL в Annex-B буфері
// (див. RewriteSPSColourBT709). Інші NAL копіюються як є, стартові коди
// нормалізуються до 4-байтних. Якщо SPS немає — повертає вхід без змін.
// Помилка розбору SPS -> вхід без змін + помилка (ніколи не ламаємо потік).
func RewriteAnnexBSPSColourBT709(b []byte) ([]byte, error) {
	nals := SplitNALs(b)
	has := false
	for _, n := range nals {
		if len(n) > 0 && n[0]&0x1F == NALSPS {
			has = true
			break
		}
	}
	if !has {
		return b, nil
	}
	out := make([]byte, 0, len(b)+16)
	for _, n := range nals {
		if len(n) > 0 && n[0]&0x1F == NALSPS {
			nn, err := RewriteSPSColourBT709(n)
			if err != nil {
				return b, err
			}
			n = nn
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out, nil
}

// lastOneBit — позиція останнього 1-біта (rbsp_stop_one_bit); -1, якщо нема.
// Хвостові нульові байти (cabac_zero_words тощо) ігноруються.
func lastOneBit(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0 {
			for k := 0; k < 8; k++ {
				if b[i]>>uint(k)&1 == 1 {
					return i*8 + 7 - k
				}
			}
		}
	}
	return -1
}

// escapeRBSP вставляє emulation_prevention_three_byte (7.4.1).
func escapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/64+2)
	zeros := 0
	for _, v := range b {
		if zeros >= 2 && v <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

type bitWriter struct {
	b []byte
	n int // бітів записано
}

func (w *bitWriter) put(v, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[len(w.b)-1] |= 1 << uint(7-w.n%8)
		}
		w.n++
	}
}
