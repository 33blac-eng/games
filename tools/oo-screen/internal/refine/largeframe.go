package refine

// Правило великого кадру (C3, як у CRD webrtc_video_encoder_wrapper): кадр,
// що змінив більшу частину екрана, при малому HRD-бюджеті на змінений піксель
// rate control однаково закодує «милом» — але спершу перевищить HRD і дасть
// стрибок затримки (RESULTS-ratecontrol.md: MaxQP 36 на 2M — черга 371 -> 744
// мс). Краще одразу дешевий кадр з високим MinQP, а якість доведе top-off
// (Converge) на нерухомому екрані.

// LargeFrameChanged — частка зміненої площі, з якої кадр «великий».
const LargeFrameChanged = 0.5

// LargeFrameBitsPerPixel — HRD-бюджет (біт) на змінений піксель, нижче
// якого бюджет вважається малим. HRD агента — пів секунди середнього
// бітрейту (mft.c hrd_bits): 1080p, повна зміна, 2 Мбіт/с -> ~0.48 біт/пкс
// (правило діє), 8 Мбіт/с -> ~1.9 (не діє).
const LargeFrameBitsPerPixel = 1.0

// LargeFrameMinQP — MinQP для кадру з часткою зміни changed (0..1) при
// середньому бітрейті bitrateBps і площі кадру pixels; minQP — налаштоване
// значення (0 — правило вимкнено). 0 — кадр не великий або бюджет достатній.
func LargeFrameMinQP(changed float64, bitrateBps, pixels, minQP int) int {
	if minQP <= 0 || bitrateBps <= 0 || pixels <= 0 || changed < LargeFrameChanged {
		return 0
	}
	hrd := float64(bitrateBps) / 2
	if hrd/(changed*float64(pixels)) >= LargeFrameBitsPerPixel {
		return 0
	}
	if minQP > 51 {
		minQP = 51
	}
	return minQP
}
