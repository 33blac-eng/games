package control

import "strings"

// EncCapsProps — CODECAPI-властивості, які опитує probe_codecapi (mft.c), у
// тому самому порядку. C2 (WORLD-COMPARISON-2026 §5 п. 2): без цього звіту
// ROI / LTR / intra refresh / Min-MaxQP на конкретному GPU — віра, а не факт.
var EncCapsProps = []string{
	"ROIEnabled", "DirtyRectEnabled", "GradualIntraRefresh",
	"LTRBufferControl", "MarkLTRFrame", "UseLTRFrame",
	"MinQP", "MaxQP", "EncodeQP", "EncodeFrameTypeQP", "ContentType",
}

// Стани властивості у звіті.
const (
	CapModifiable  = "modifiable"  // IsSupported і IsModifiable == S_OK
	CapSupported   = "supported"   // лише IsSupported
	CapUnsupported = "unsupported" // інакше (або помилка)
)

// ParseEncCaps розбирає «Ім'я=M|S|-» через пробіл. Невідомі імена й
// значення пропускаються — рядок іде з C, але хаб усе одно перевіряє ще раз.
func ParseEncCaps(s string) map[string]string {
	known := map[string]bool{}
	for _, p := range EncCapsProps {
		known[p] = true
	}
	out := map[string]string{}
	for _, f := range strings.Fields(s) {
		k, v, ok := strings.Cut(f, "=")
		if !ok || !known[k] {
			continue
		}
		switch v {
		case "M":
			out[k] = CapModifiable
		case "S":
			out[k] = CapSupported
		case "-":
			out[k] = CapUnsupported
		}
	}
	return out
}
