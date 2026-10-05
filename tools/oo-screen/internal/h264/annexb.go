// Package h264 — канонічний H.264-контракт (§5.2 плану): Annex-B AU-сплітер
// і мінімальний SPS-парсер. SDP profile-level-id (A) і WebCodecs-конфіг (B)
// виводяться з РОЗПАРСЕНОГО SPS, не з припущень.
package h264

import (
	"bytes"
	"errors"
)

// NAL unit types (H.264, nal_unit_type = b & 0x1F)
const (
	NALSlice = 1
	NALIDR   = 5
	NALSEI   = 6
	NALSPS   = 7
	NALPPS   = 8
	NALAUD   = 9
)

var startCode3 = []byte{0, 0, 1}

// SplitNALs розбиває Annex-B байти на NAL-юніти (без стартових кодів).
func SplitNALs(b []byte) [][]byte {
	var nals [][]byte
	i := 0
	for {
		idx := bytes.Index(b[i:], startCode3)
		if idx < 0 {
			break
		}
		start := i + idx + 3
		next := bytes.Index(b[start:], startCode3)
		var end int
		if next < 0 {
			end = len(b)
		} else {
			end = start + next
			// 4-байтний стартовий код: попередній байт 0 — не частина NAL
			if end > start && b[end-1] == 0 {
				end--
			}
		}
		if end > start {
			nals = append(nals, b[start:end])
		}
		if next < 0 {
			break
		}
		i = start + next
	}
	return nals
}

// AU — один повний access unit.
type AU struct {
	Data     []byte // Annex-B байти AU (зі стартовими кодами)
	Keyframe bool   // містить IDR
	HasSPS   bool
}

// SplitAUs розбиває Annex-B потік на access units. Межа AU — початок нового
// первинного слайсу (first_mb_in_slice==0) або SPS/AUD перед ним. Для потоку
// нашого контракту (повний AU на кадр, SPS/PPS з кожним IDR, без B-кадрів)
// достатньо правила: новий AU починається на AUD, SPS або на слайсі з
// first_mb_in_slice==0, якщо в поточному AU вже є слайс.
func SplitAUs(stream []byte) []AU {
	type nalPos struct {
		start, end int // межі включно зі стартовим кодом (start вказує на 00 00 [00] 01)
		typ        byte
		firstMB0   bool
	}
	var poss []nalPos
	i := 0
	for {
		idx := bytes.Index(stream[i:], startCode3)
		if idx < 0 {
			break
		}
		scStart := i + idx
		if scStart > 0 && stream[scStart-1] == 0 {
			scStart-- // 4-байтний стартовий код
		}
		payload := i + idx + 3
		if payload >= len(stream) {
			break
		}
		typ := stream[payload] & 0x1F
		firstMB0 := false
		if typ == NALSlice || typ == NALIDR {
			// перший біт RBSP після заголовка: ue(v) first_mb_in_slice —
			// значення 0 кодується бітом '1' на початку
			if payload+1 < len(stream) {
				firstMB0 = stream[payload+1]&0x80 != 0
			}
		}
		if len(poss) > 0 {
			poss[len(poss)-1].end = scStart
		}
		poss = append(poss, nalPos{start: scStart, end: len(stream), typ: typ, firstMB0: firstMB0})
		i = payload
	}
	var aus []AU
	var cur *AU
	curHasSlice := false
	flush := func() {
		if cur != nil && len(cur.Data) > 0 {
			aus = append(aus, *cur)
		}
		cur = &AU{}
		curHasSlice = false
	}
	cur = &AU{}
	for _, p := range poss {
		isSlice := p.typ == NALSlice || p.typ == NALIDR
		newAU := false
		switch {
		case p.typ == NALAUD || p.typ == NALSPS:
			if curHasSlice {
				newAU = true
			}
		case isSlice && p.firstMB0 && curHasSlice:
			newAU = true
		}
		if newAU {
			flush()
		}
		cur.Data = append(cur.Data, stream[p.start:p.end]...)
		if p.typ == NALIDR {
			cur.Keyframe = true
		}
		if p.typ == NALSPS {
			cur.HasSPS = true
		}
		if isSlice {
			curHasSlice = true
		}
	}
	if cur != nil && len(cur.Data) > 0 {
		aus = append(aus, *cur)
	}
	return aus
}

// SPS — мінімально потрібні поля.
type SPS struct {
	ProfileIDC      byte
	ConstraintFlags byte
	LevelIDC        byte
	Width, Height   int

	// VUI (Annex E). VUIPresent=false -> решта полів нульові.
	VUIPresent bool
	// VideoSignalTypePresent — video_signal_type_present_flag.
	VideoSignalTypePresent bool
	VideoFormat            int  // 5 = unspecified (дефолт, коли поля нема)
	FullRange              bool // video_full_range_flag
	// ColourDescriptionPresent — colour_description_present_flag; без нього
	// декодер/браузер вгадує колірний простір (часто BT.601).
	ColourDescriptionPresent bool
	ColourPrimaries          int // 1 = BT.709
	TransferCharacteristics  int // 1 = BT.709
	MatrixCoefficients       int // 1 = BT.709

	// vuiFlagPos — бітова позиція vui_parameters_present_flag у RBSP
	// (після nal-заголовка). Потрібна RewriteSPSColourBT709.
	vuiFlagPos int
}

// ProfileLevelID — hex-рядок для SDP profile-level-id та avc1-кодек-стрінга.
func (s *SPS) ProfileLevelID() string {
	const hex = "0123456789ABCDEF"
	b := []byte{s.ProfileIDC, s.ConstraintFlags, s.LevelIDC}
	out := make([]byte, 6)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0xF]
	}
	return string(out)
}

// CodecString — WebCodecs "avc1.PPCCLL".
func (s *SPS) CodecString() string { return "avc1." + s.ProfileLevelID() }

var ErrNotSPS = errors.New("h264: not an SPS NAL")

// ParseSPS парсить SPS NAL (без стартового коду). Підтримує профілі з
// chroma_format_idc-гілкою (High тощо).
func ParseSPS(nal []byte) (*SPS, error) {
	if len(nal) < 4 || nal[0]&0x1F != NALSPS {
		return nil, ErrNotSPS
	}
	rbsp := unescapeRBSP(nal[1:])
	br := &bitReader{b: rbsp}
	s := &SPS{}
	s.ProfileIDC = byte(br.bits(8))
	s.ConstraintFlags = byte(br.bits(8))
	s.LevelIDC = byte(br.bits(8))
	br.ue()     // seq_parameter_set_id
	chroma := 1 // chroma_format_idc: дефолт 4:2:0 для профілів без явного поля
	sepColour := 0
	switch s.ProfileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = br.ue()
		if chroma == 3 {
			sepColour = br.bits(1) // separate_colour_plane_flag
		}
		br.ue()              // bit_depth_luma_minus8
		br.ue()              // bit_depth_chroma_minus8
		br.bits(1)           // qpprime_y_zero_transform_bypass_flag
		if br.bits(1) == 1 { // seq_scaling_matrix_present_flag
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if br.bits(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := 8, 8
					for j := 0; j < size; j++ {
						if next != 0 {
							next = (last + br.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	br.ue() // log2_max_frame_num_minus4
	poc := br.ue()
	if poc == 0 {
		br.ue() // log2_max_pic_order_cnt_lsb_minus4
	} else if poc == 1 {
		br.bits(1)
		br.se()
		br.se()
		// num_ref_frames_in_pic_order_cnt_cycle: за стандартом 0..255. На
		// битому SPS ue() дає до 2^32, і цикл крутився б мільярди разів уже
		// після br.err — секунди CPU в горутині запису хаба.
		n := br.ue()
		if n > 255 {
			return nil, errors.New("h264: num_ref_frames_in_pic_order_cnt_cycle > 255")
		}
		for i := 0; i < n && br.err == nil; i++ {
			br.se()
		}
	}
	br.ue()    // max_num_ref_frames
	br.bits(1) // gaps_in_frame_num_value_allowed_flag
	wMbs := br.ue() + 1
	hMapUnits := br.ue() + 1
	frameMbsOnly := br.bits(1)
	if frameMbsOnly == 0 {
		br.bits(1) // mb_adaptive_frame_field_flag
	}
	br.bits(1) // direct_8x8_inference_flag
	cropL, cropR, cropT, cropB := 0, 0, 0, 0
	if br.bits(1) == 1 { // frame_cropping_flag
		cropL, cropR, cropT, cropB = br.ue(), br.ue(), br.ue(), br.ue()
	}
	s.vuiFlagPos = br.pos
	if br.bits(1) == 1 { // vui_parameters_present_flag
		s.VUIPresent = true
		if br.bits(1) == 1 { // aspect_ratio_info_present_flag
			if br.bits(8) == 255 { // Extended_SAR
				br.bits(16)
				br.bits(16)
			}
		}
		if br.bits(1) == 1 { // overscan_info_present_flag
			br.bits(1)
		}
		s.VideoFormat = 5
		s.ColourPrimaries, s.TransferCharacteristics, s.MatrixCoefficients = 2, 2, 2
		if br.bits(1) == 1 { // video_signal_type_present_flag
			s.VideoSignalTypePresent = true
			s.VideoFormat = br.bits(3)
			s.FullRange = br.bits(1) == 1
			if br.bits(1) == 1 { // colour_description_present_flag
				s.ColourDescriptionPresent = true
				s.ColourPrimaries = br.bits(8)
				s.TransferCharacteristics = br.bits(8)
				s.MatrixCoefficients = br.bits(8)
			}
		}
		// решта VUI (chroma loc, timing, HRD, bitstream_restriction) нам не
		// потрібна: переписувач копіює її біт-у-біт.
	}
	if br.err != nil {
		return nil, br.err
	}
	// crop-units за специфікацією (7-21..7-24): залежать від chroma-формату
	// й interlace, а не завжди ×2 (Codex R-T1 #19)
	chromaArrayType := chroma
	if sepColour == 1 {
		chromaArrayType = 0
	}
	cropX, cropY := 1, 1 // monochrome / 4:4:4
	switch chromaArrayType {
	case 1:
		cropX, cropY = 2, 2 // 4:2:0
	case 2:
		cropX, cropY = 2, 1 // 4:2:2
	}
	if frameMbsOnly == 0 {
		cropY *= 2
	}
	s.Width = wMbs*16 - (cropL+cropR)*cropX
	h := hMapUnits * 16
	if frameMbsOnly == 0 {
		h *= 2
	}
	s.Height = h - (cropT+cropB)*cropY
	return s, nil
}

func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if i+2 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 3 {
			out = append(out, 0, 0)
			i += 2
			continue
		}
		out = append(out, b[i])
	}
	return out
}

type bitReader struct {
	b   []byte
	pos int // біт
	err error
}

func (r *bitReader) bits(n int) int {
	v := 0
	for i := 0; i < n; i++ {
		byteIdx := r.pos >> 3
		if byteIdx >= len(r.b) {
			r.err = errors.New("h264: SPS bitstream overrun")
			return 0
		}
		v = v<<1 | int(r.b[byteIdx]>>(7-uint(r.pos&7))&1)
		r.pos++
	}
	return v
}

func (r *bitReader) ue() int {
	zeros := 0
	for r.bits(1) == 0 && r.err == nil {
		zeros++
		if zeros > 31 {
			r.err = errors.New("h264: ue overrun")
			return 0
		}
	}
	return (1 << uint(zeros)) - 1 + r.bits(zeros)
}

func (r *bitReader) se() int {
	u := r.ue()
	if u%2 == 0 {
		return -(u / 2)
	}
	return (u + 1) / 2
}
