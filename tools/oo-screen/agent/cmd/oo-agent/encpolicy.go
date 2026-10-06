package main

import (
	"strconv"
	"time"

	"github.com/organicoils/oo-screen/agent/encode"
	"github.com/organicoils/oo-screen/internal/h264"
	"github.com/organicoils/oo-screen/internal/refine"
)

// Політики rate control енкодера (TASK.md крок 4). Платформонезалежна
// частина: розбір env і зіставлення AU з видом кадру — тестується на Linux.
// Симуляція кожної політики: bench/quality/ratecontrol_run.py,
// результати — bench/quality/RESULTS-ratecontrol.md.
type rcPolicy struct {
	// RefineAfterIDR — IDR від rate control на нерухомому екрані заводить
	// refine заново (refine.Config.AfterKeyframe). OO_SCREEN_REFINE_AFTER_IDR,
	// типово увімкнено.
	RefineAfterIDR bool
	// RefineQPAware — пропускати кроки refine, не кращі за найгірший QP на
	// екрані (QP — із заголовка слайса AU). OO_SCREEN_REFINE_QP_AWARE, типово
	// вимкнено: у симуляції виграш у межах шуму, а slice QP апаратного MFT
	// (з AQ) — UNVERIFIED.
	RefineQPAware bool
}

// envBool: "1"/"true"/"on" — так, "0"/"false"/"off" — ні, інше — def.
func envBool(getenv func(string) string, key string, def bool) bool {
	switch getenv(key) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	}
	return def
}

// envInt: ціле в [lo,hi], інакше def.
func envInt(getenv func(string) string, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(getenv(key))
	if err != nil || v < lo || v > hi {
		return def
	}
	return v
}

func rcPolicyFromEnv(getenv func(string) string) rcPolicy {
	return rcPolicy{
		// Типово УВІМКНЕНО: bench/quality/refine_after_idr.py — таблиця на 2M
		// після IDR 20.8 dB -> 38.1 dB (edge-SSIM 0.81 -> 0.999); ціна — та
		// сама, що й refine після руху. "0" вимикає.
		RefineAfterIDR: envBool(getenv, "OO_SCREEN_REFINE_AFTER_IDR", true),
		RefineQPAware:  envBool(getenv, "OO_SCREEN_REFINE_QP_AWARE", false),
	}
}

// auKinds — вид кадру (refine.Frame без Key/QP), поданого в енкодер, за його
// PTS. Апаратний MFT конвеєрний: AU кадру повертається одним із НАСТУПНИХ
// Encode, тож «що це було» — лише за PTS. Обмежений: на випадок, коли MFT
// якийсь кадр так і не віддав (flush, перебудова), найстаріші записи
// витісняються.
type auKinds struct {
	m     map[time.Duration]refine.Frame
	order []time.Duration
}

const auKindsMax = 32

func (k *auKinds) note(pts time.Duration, f refine.Frame) {
	if k.m == nil {
		k.m = make(map[time.Duration]refine.Frame, auKindsMax)
	}
	if _, dup := k.m[pts]; !dup {
		k.order = append(k.order, pts)
	}
	k.m[pts] = f
	for len(k.order) > auKindsMax {
		delete(k.m, k.order[0])
		k.order = k.order[1:]
	}
}

func (k *auKinds) take(pts time.Duration) (refine.Frame, bool) {
	f, ok := k.m[pts]
	if ok {
		delete(k.m, pts)
		for i, p := range k.order {
			if p == pts {
				k.order = append(k.order[:i], k.order[i+1:]...)
				break
			}
		}
	}
	return f, ok
}

// observeAUs — кожен AU, що вийшов з енкодера, повідомляє refine: вид кадру
// (за PTS), чи це IDR і його QP з потоку. Невідомий PTS (кадр до перебудови
// енкодера) рахується рухом — так безпечніше: невідомий QP не дає
// QPAware пропустити refine.
func observeAUs(aus []encode.AU, kinds *auKinds, qpr *h264.QPReader, r *refine.State, now time.Time) {
	for _, au := range aus {
		f, ok := kinds.take(au.PTS)
		if !ok {
			f = refine.Frame{Motion: true}
		}
		f.Key = au.Keyframe
		if q, ok := qpr.Observe(au.Data); ok {
			f.QP = q
		}
		r.Coded(now, f)
	}
}
