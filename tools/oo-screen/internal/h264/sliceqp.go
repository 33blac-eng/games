package h264

import "errors"

// QP кадру з бітового потоку (TASK.md крок 4, refine з урахуванням QP).
//
// Навіщо агенту QP: Media Foundation MFT не повертає QP закодованого кадру, а
// політиці дошліфування (internal/refine) він потрібен — refine-кадр із QP 22
// поверх кадру, який rate control уже закодував із QP 16, нічого не покращує і
// лише витрачає біти (симуляція bench/quality/ratecontrol_run.py). QP кадру —
// це pic_init_qp (PPS) + slice_qp_delta (заголовок слайса); MB-дельти (AQ,
// ROI) поверх нього сюди не входять, тож це базовий QP кадру.
//
// QPReader тримає останні SPS/PPS, що пройшли в потоці (MFT повторює їх з
// кожним IDR, агент — теж, encode.withHeaders), і дочитує заголовок першого
// слайса AU рівно до slice_qp_delta. Без SPS/PPS або на незнайомому синтаксисі
// (slice groups/FMO) повертає ok=false — політика тоді поводиться як раніше.

// PPS — поля, потрібні для заголовка слайса.
type pps struct {
	spsID                    int
	entropyCABAC             bool
	bottomFieldPicOrder      bool
	numRefIdxL0, numRefIdxL1 int // num_ref_idx_lX_default_active
	weightedPred             bool
	weightedBipredIdc        int
	picInitQP                int // 26 + pic_init_qp_minus26
	redundantPicCntPresent   bool
}

var errSliceSyntax = errors.New("h264: unsupported slice syntax")

func parsePPS(nal []byte) (int, *pps, error) {
	br := &bitReader{b: unescapeRBSP(nal[1:])}
	id := br.ue()
	p := &pps{spsID: br.ue()}
	p.entropyCABAC = br.bits(1) == 1
	p.bottomFieldPicOrder = br.bits(1) == 1
	if br.ue() != 0 { // num_slice_groups_minus1: FMO — не наш випадок
		return 0, nil, errSliceSyntax
	}
	p.numRefIdxL0 = br.ue() + 1
	p.numRefIdxL1 = br.ue() + 1
	p.weightedPred = br.bits(1) == 1
	p.weightedBipredIdc = br.bits(2)
	p.picInitQP = 26 + br.se()
	br.se()    // pic_init_qs_minus26
	br.se()    // chroma_qp_index_offset
	br.bits(1) // deblocking_filter_control_present_flag
	br.bits(1) // constrained_intra_pred_flag
	p.redundantPicCntPresent = br.bits(1) == 1
	if br.err != nil {
		return 0, nil, br.err
	}
	if id > 255 || p.spsID > 31 || p.picInitQP < 0 || p.picInitQP > 51 {
		return 0, nil, errSliceSyntax
	}
	return id, p, nil
}

// QPReader — QP кадрів потоку. Не потокобезпечний: живе там, де AU виходять
// з енкодера.
type QPReader struct {
	sps map[int]*SPS
	pps map[int]*pps
}

// NewQPReader — порожній читач; SPS/PPS береться з потоку.
func NewQPReader() *QPReader {
	return &QPReader{sps: map[int]*SPS{}, pps: map[int]*pps{}}
}

// Observe запам'ятовує SPS/PPS з AU (Annex-B) і повертає QP першого слайса
// кадру. ok=false — QP невідомий (ще не було SPS/PPS, AU без слайса,
// непідтримуваний синтаксис).
func (r *QPReader) Observe(au []byte) (qp int, ok bool) {
	for _, nal := range SplitNALs(au) {
		if len(nal) < 2 {
			continue
		}
		switch nal[0] & 0x1F {
		case NALSPS:
			if s, err := ParseSPS(nal); err == nil && s.spsID < 32 {
				r.sps[s.spsID] = s
			}
		case NALPPS:
			if id, p, err := parsePPS(nal); err == nil {
				r.pps[id] = p
			}
		case NALSlice, NALIDR:
			if q, err := r.sliceQP(nal); err == nil {
				return q, true
			}
			return 0, false
		}
	}
	return 0, false
}

// sliceQP — 7.3.3 slice_header() до slice_qp_delta включно.
func (r *QPReader) sliceQP(nal []byte) (int, error) {
	nalRefIdc := int(nal[0]>>5) & 3
	idr := nal[0]&0x1F == 5
	br := &bitReader{b: unescapeRBSP(nal[1:])}
	br.ue() // first_mb_in_slice
	st := br.ue() % 5
	p := r.pps[br.ue()]
	if p == nil || br.err != nil {
		return 0, errSliceSyntax
	}
	s := r.sps[p.spsID]
	if s == nil {
		return 0, errSliceSyntax
	}
	const (
		sliceP, sliceB, sliceI, sliceSP, sliceSI = 0, 1, 2, 3, 4
	)
	if s.separateColourPlane {
		br.bits(2) // colour_plane_id
	}
	br.bits(s.log2MaxFrameNum) // frame_num
	field := false
	if !s.frameMbsOnly {
		if field = br.bits(1) == 1; field {
			br.bits(1) // bottom_field_flag
		}
	}
	if idr {
		br.ue() // idr_pic_id
	}
	if s.pocType == 0 {
		br.bits(s.log2MaxPocLsb)
		if p.bottomFieldPicOrder && !field {
			br.se() // delta_pic_order_cnt_bottom
		}
	}
	if s.pocType == 1 && !s.deltaPicOrderAlwaysZero {
		br.se()
		if p.bottomFieldPicOrder && !field {
			br.se()
		}
	}
	if p.redundantPicCntPresent {
		br.ue()
	}
	if st == sliceB {
		br.bits(1) // direct_spatial_mv_pred_flag
	}
	l0, l1 := p.numRefIdxL0, p.numRefIdxL1
	if st == sliceP || st == sliceSP || st == sliceB {
		if br.bits(1) == 1 { // num_ref_idx_active_override_flag
			l0 = br.ue() + 1
			if st == sliceB {
				l1 = br.ue() + 1
			}
		}
	}
	if l0 > 32 || l1 > 32 {
		return 0, errSliceSyntax
	}
	// ref_pic_list_modification()
	listMod := func() {
		if br.bits(1) == 0 {
			return
		}
		for n := 0; n < 100 && br.err == nil; n++ {
			idc := br.ue()
			if idc == 3 {
				return
			}
			br.ue() // abs_diff_pic_num_minus1 / long_term_pic_num
		}
		br.err = errSliceSyntax
	}
	if st != sliceI && st != sliceSI {
		listMod()
	}
	if st == sliceB {
		listMod()
	}
	// pred_weight_table()
	if (p.weightedPred && (st == sliceP || st == sliceSP)) || (p.weightedBipredIdc == 1 && st == sliceB) {
		br.ue() // luma_log2_weight_denom
		if s.chromaArrayType != 0 {
			br.ue() // chroma_log2_weight_denom
		}
		lists := []int{l0}
		if st == sliceB {
			lists = append(lists, l1)
		}
		for _, n := range lists {
			for i := 0; i < n && br.err == nil; i++ {
				if br.bits(1) == 1 {
					br.se()
					br.se()
				}
				if s.chromaArrayType != 0 && br.bits(1) == 1 {
					br.se()
					br.se()
					br.se()
					br.se()
				}
			}
		}
	}
	// dec_ref_pic_marking()
	if nalRefIdc != 0 {
		if idr {
			br.bits(2) // no_output_of_prior_pics_flag, long_term_reference_flag
		} else if br.bits(1) == 1 { // adaptive_ref_pic_marking_mode_flag
			for n := 0; ; n++ {
				if n > 100 || br.err != nil {
					return 0, errSliceSyntax
				}
				op := br.ue()
				if op == 0 {
					break
				}
				if op == 1 || op == 3 {
					br.ue()
				}
				if op == 2 {
					br.ue()
				}
				if op == 3 || op == 6 {
					br.ue()
				}
				if op == 4 {
					br.ue()
				}
			}
		}
	}
	if p.entropyCABAC && st != sliceI && st != sliceSI {
		br.ue() // cabac_init_idc
	}
	qp := p.picInitQP + br.se()
	if br.err != nil {
		return 0, br.err
	}
	if qp < 0 || qp > 51 {
		return 0, errSliceSyntax
	}
	return qp, nil
}
