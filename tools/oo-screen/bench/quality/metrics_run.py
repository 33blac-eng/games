#!/usr/bin/env python3
"""bench/quality/metrics_run.py — image-quality metrics before/after encode (TASK.md step 5).

One command scores every desktop screenshot of the corpus through the agent-like encode path
and writes a markdown table (image x variant x metric) plus a JSON for diffing; a second mode
diffs two JSON runs (e.g. before/after a commit).

    python3 metrics_run.py run                     # -> RESULTS-metrics.md + RESULTS-metrics.json
    python3 metrics_run.py run --quick             # 1080p only, 4 variants (smoke, ~1 min)
    python3 metrics_run.py run --json a.json --md a.md --images excel,tinyfont --variants main-idr,main-steady
    python3 metrics_run.py compare base.json new.json [--md diff.md] [--fail-on-regression]

    # current tree against the committed baseline
    python3 metrics_run.py run --json /tmp/new.json --md /tmp/new.md --jobs 4
    python3 metrics_run.py compare RESULTS-metrics.json /tmp/new.json

    # numbers before/after a commit
    git stash; python3 metrics_run.py run --json /tmp/before.json --md /tmp/before.md; git stash pop
    python3 metrics_run.py run --json /tmp/after.json --md /tmp/after.md
    python3 metrics_run.py compare /tmp/before.json /tmp/after.json

Corpus: bench/corpus/*.png (legacy: sheet/code/colortext at 1080p+1440p) and
bench/corpus/screens/*.png (Excel-like 11 px, 8-9 px fonts, ClearType-like subpixel text, mixed
desktop), all synthetic and deterministic (gen_corpus.py). Real screenshots: drop PNGs into
another directory and pass --corpus DIR (repeatable).

Encode: x264 via ffmpeg, configured from the agent's MFT settings, which are PARSED FROM THE
SOURCE TREE (agent/encode/mft.c, agent/cmd/oo-agent/{main,output}.go, internal/refine) so a commit
that changes them changes the numbers. The real agent encodes with a Media Foundation MFT
(NVENC/QSV/AMF or the Microsoft software MFT) — x264 is a stand-in: SIMULATION, NOT MEASURED ON
WINDOWS. Absolute numbers will differ from the MFT; use them for relative comparison.

VMAF: libvmaf filter of `--vmaf-ffmpeg` / $OO_VMAF_FFMPEG / the system ffmpeg, whichever has it;
otherwise the column is n/a (stock Ubuntu ffmpeg has only vmafmotion).
"""
import argparse, ast, concurrent.futures, datetime, glob, json, math, operator, os, re, subprocess, sys, time
import numpy as np
from PIL import Image

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import metrics as M  # noqa: E402
import pipeline as P  # noqa: E402

ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))  # tools/oo-screen
CORPORA = [os.path.join(HERE, "..", "corpus"), os.path.join(HERE, "..", "corpus", "screens")]
SCHEMA = 1

# metric key -> (column title, decimals, kind). kind: "up" higher is better, "one" closer to 1.0
# is better, "cost" bits (reported, not a quality regression).
METRICS = {
    "psnr": ("PSNR dB", 2, "up"),
    "psnr_y": ("PSNR-Y dB", 2, "up"),
    "psnr_c": ("PSNR-CbCr dB", 2, "up"),
    "ssim": ("SSIM", 4, "up"),
    "vmaf": ("VMAF", 2, "up"),
    "txt_essim": ("text edge-SSIM", 4, "up"),
    "txt_grad": ("text grad-energy", 3, "one"),
    "txt_contrast": ("text contrast", 3, "one"),
    "txt_psnr_c": ("text PSNR-CbCr dB", 2, "up"),
    "kbit": ("kbit", 0, "cost"),
}
# compare: a change worse than this is flagged as a regression
THRESH = {"psnr": 0.3, "psnr_y": 0.3, "psnr_c": 0.3, "ssim": 0.001, "vmaf": 0.5, "txt_essim": 0.002,
          "txt_grad": 0.01, "txt_contrast": 0.01, "txt_psnr_c": 0.3}

VARIANTS = {
    "raw420": "colour chain only: BT.709 limited 4:2:0 (2x2 box down, bilinear up), no codec — ceiling of any 4:2:0 H.264 path",
    "main-idr": "Main/CABAC, PCVBR at the agent's auto bitrate; frame 0 = IDR right after a full-screen change (HRD-limited). "
                "On a still screen this is what the viewer sees until refine (the agent skips unchanged frames)",
    "main-steady": "same stream, frame 30 of 30 identical inputs = where x264 rate control converges if the static frame "
                   "kept being re-encoded for 1 s. Reference only: the agent does NOT re-encode unchanged frames (DXGI no-op skip)",
    "main-refine": "refine emulation: the still frame at the last refine QP (internal/refine DefaultQPs) as one intra frame, "
                   "I/P QP equal. The agent sends refine P-frames (QP 22 then 18) over the post-change frame, paced under "
                   "the peak rate; ffmpeg's x264 cannot force a per-frame QP inside a VBV stream (zones are ignored there — "
                   "checked), so quality ~ a QP-18 frame, bytes = intra upper bound",
    "cbase-idr": "Constrained Baseline (CAVLC, 42c0xx — OpenH264 / lowest profile Chrome lists), same bitrate, frame 0",
    "cbase-steady": "Constrained Baseline, frame 30 of 30 (reference, see main-steady)",
    "main2m-idr": "Main at 2 Mbit/s (congested office link after hub bitrate control), frame 0",
    "main2m-steady": "Main at 2 Mbit/s, frame 30 of 30 (reference, see main-steady)",
}
QUICK_VARIANTS = ["raw420", "main-idr", "main-steady", "main-refine"]


# ---------------------------------------------------------------- agent config from the source tree

_OPS = {ast.Add: operator.add, ast.Sub: operator.sub, ast.Mult: operator.mul, ast.Div: operator.truediv}


def _arith(expr, **names):
    """Evaluate a tiny C arithmetic expression (+-*/, ints, given names) without eval()."""
    def ev(n):
        if isinstance(n, ast.Expression):
            return ev(n.body)
        if isinstance(n, ast.BinOp) and type(n.op) in _OPS:
            return _OPS[type(n.op)](ev(n.left), ev(n.right))
        if isinstance(n, ast.Constant) and isinstance(n.value, (int, float)):
            return n.value
        if isinstance(n, ast.Name) and n.id in names:
            return names[n.id]
        raise ValueError(expr)
    try:
        tree = ast.parse(expr, mode="eval")
    except SyntaxError as e:
        raise ValueError(expr) from e
    return ev(tree)


DEFAULT_CFG = {"baseline_bps": 8_000_000, "baseline_pixels": 1920 * 1080, "max_auto_bps": 30_000_000,
               "peak_ratio": 1.5, "hrd_seconds": 0.5, "refs": 2, "bframes": 0, "profile": "Main",
               "fps": 30, "gop_seconds": 10, "refine_qps": [22, 18]}


def _read(rel):
    try:
        with open(os.path.join(ROOT, rel), encoding="utf-8") as f:
            return f.read()
    except OSError:
        return ""


def agent_config(root=None):
    """Encoder settings of the agent, parsed from the tree; any value that cannot be parsed keeps
    its DEFAULT_CFG value and is listed in cfg['_defaults'] (so drift is visible, not silent)."""
    global ROOT
    if root:
        ROOT = root
    cfg, miss = dict(DEFAULT_CFG), []
    out_go, main_go = _read("agent/cmd/oo-agent/output.go"), _read("agent/cmd/oo-agent/main.go")
    mft, refine = _read("agent/encode/mft.c"), _read("internal/refine/refine.go")

    def grab(key, text, pat, conv):
        m = re.search(pat, text, re.S)
        try:
            cfg[key] = conv(m)
        except Exception:  # noqa: BLE001 — any parse failure falls back to the default
            miss.append(key)

    grab("baseline_bps", out_go, r"baselineBitrateBps\s*=\s*([\d_]+)", lambda m: int(m.group(1).replace("_", "")))
    grab("baseline_pixels", out_go, r"baselinePixels\s*=\s*([\d_ *]+)\n", lambda m: int(_arith(m.group(1).strip())))
    grab("max_auto_bps", out_go, r"maxAutoBitrateBps\s*=\s*([\d_]+)", lambda m: int(m.group(1).replace("_", "")))
    grab("peak_ratio", mft, r"peak_bps\(int32_t mean\)\s*\{\s*return \(ULONG\)\((.*?)\);",
         lambda m: float(_arith(m.group(1), mean=1.0)))
    grab("hrd_seconds", mft, r"hrd_bits\(int32_t mean\)\s*\{\s*return \(ULONG\)\((.*?)\);",
         lambda m: float(_arith(m.group(1), mean=1.0)))
    grab("refs", mft, r"OOS_AVEncVideoMaxNumRefFrame,\s*(\d+)\)", lambda m: int(m.group(1)))
    grab("bframes", mft, r"OOS_AVEncMPVDefaultBPictureCount,\s*(\d+)\)", lambda m: int(m.group(1)))
    grab("profile", mft, r"profiles\[\]\s*=\s*\{\s*eAVEncH264VProfile_(\w+)", lambda m: m.group(1))
    grab("fps", main_go, r'flag\.Int\("fps",\s*(\d+)', lambda m: int(m.group(1)))
    grab("gop_seconds", main_go, r'flag\.Int\("gop-seconds",\s*(\d+)', lambda m: int(m.group(1)))
    grab("refine_qps", refine, r"DefaultQPs\s*=\s*\[\]int\{([\d,\s]+)\}",
         lambda m: [int(x) for x in m.group(1).split(",") if x.strip()])
    cfg["_defaults"] = miss
    return cfg


def auto_kbps(cfg, w, h):
    """agent/cmd/oo-agent/output.go defaultBitrate(): bitrate scales with pixels, capped."""
    bps = min(cfg["baseline_bps"] * w * h // cfg["baseline_pixels"], cfg["max_auto_bps"])
    return bps // 1000


# ---------------------------------------------------------------- scoring

def score(ref, out, mask, vmaf_ff=None):
    _, cb0, cr0 = P.rgb_to_yuv(ref)
    _, cb1, cr1 = P.rgb_to_yuv(out)
    es, er = M.text_sharpness(ref, out, mask)
    r = {"psnr": M.psnr(ref, out), "psnr_y": M.psnr(M.luma(ref), M.luma(out)),
         "psnr_c": (M.psnr(cb0, cb1) + M.psnr(cr0, cr1)) / 2, "ssim": M.ssim(ref, out),
         "vmaf": P.vmaf_rgb(ref, out, vmaf_ff) if vmaf_ff else None,
         "txt_essim": es, "txt_grad": er, "txt_contrast": M.text_contrast(ref, out, mask),
         "txt_psnr_c": (M.masked_psnr(cb0, cb1, mask) + M.masked_psnr(cr0, cr1, mask)) / 2}
    return r


def encodes_for(variants):
    """Group variants by the encode that produces them (one x264 run feeds idr + steady)."""
    need = {}
    for v in variants:
        if v == "raw420":
            need.setdefault("raw", []).append((v, None))
        elif v == "main-refine":
            need.setdefault("refine", []).append((v, 0))
        else:
            enc, which = v.rsplit("-", 1)
            need.setdefault(enc, []).append((v, 0 if which == "idr" else -1))
    return need


def run_image(path, variants, cfg, ff="ffmpeg", vmaf_ff=None, frames=30, log=print):
    ref = np.asarray(Image.open(path).convert("RGB"))
    H, W = ref.shape[:2]
    mask = M.text_mask(ref)
    kb = auto_kbps(cfg, W, H)
    prof = {"main": "main", "cbase": "baseline", "main2m": "main"}
    rows = []
    for enc, outs in encodes_for(variants).items():
        t = time.time()
        x264v = None
        if enc == "raw":
            frames_rgb, sizes, plid = [P.run(ref, "420")[0]], [None], None
        elif enc == "refine":
            r = P.agent_x264(ref, "main", qp=cfg["refine_qps"][-1], frames=1, refs=cfg["refs"], ffmpeg=ff, keep=[0])
            frames_rgb, sizes, plid, x264v = r["frames"], r["bytes"], r["plid"], r["x264"]
        else:
            rate = 2000 if enc == "main2m" else kb
            r = P.agent_x264(ref, prof[enc], kbps=rate, peak=cfg["peak_ratio"], hrd=cfg["hrd_seconds"], refs=cfg["refs"],
                             gop=cfg["gop_seconds"] * cfg["fps"], fps=cfg["fps"], frames=frames, ffmpeg=ff, keep=[0, -1])
            frames_rgb, plid, x264v = r["frames"], r["plid"], r["x264"]
            steady = r["bytes"][1:] or r["bytes"]
            sizes = [r["bytes"][0], sum(steady) / len(steady)]
        for v, idx in outs:
            k = 0 if idx in (None, 0) else 1
            m = score(ref, frames_rgb[k], mask, vmaf_ff)
            m["kbit"] = None if sizes[k] is None else sizes[k] * 8 / 1000
            rows.append({"image": os.path.basename(path), "variant": v, "w": W, "h": H, "plid": plid, "x264": x264v,
                         "kbps": None if enc in ("raw", "refine") else (2000 if enc == "main2m" else kb), "metrics": m})
            log(f"{os.path.basename(path):24s} {v:14s} " + " ".join(
                f"{k}={fmt(m[k], METRICS[k][1])}" for k in ("psnr", "ssim", "vmaf", "txt_essim", "txt_contrast", "kbit"))
                + f" ({time.time() - t:.1f}s)")
    order = {v: i for i, v in enumerate(variants)}
    return sorted(rows, key=lambda r: order[r["variant"]])


def fmt(x, n=2):
    if x is None or (isinstance(x, float) and math.isnan(x)):
        return "n/a"
    if x == math.inf:
        return "inf"
    return f"{x:.{n}f}"


def _jsonable(x):
    if isinstance(x, float) and (math.isnan(x) or math.isinf(x)):
        return None if math.isnan(x) else ("inf" if x > 0 else "-inf")
    if isinstance(x, (float, np.floating)):
        return round(float(x), 5)
    if isinstance(x, dict):
        return {k: _jsonable(v) for k, v in x.items()}
    if isinstance(x, list):
        return [_jsonable(v) for v in x]
    return x


def _num(x):
    return math.inf if x == "inf" else (-math.inf if x == "-inf" else x)


def summarize(rows, variants):
    summ = {}
    for v in variants:
        rs = [r for r in rows if r["variant"] == v]
        if not rs:
            continue
        s = {}
        for k in METRICS:
            vals = [_num(r["metrics"].get(k)) for r in rs]
            vals = [x for x in vals if x is not None and not (isinstance(x, float) and math.isnan(x))]
            if not vals:
                s[k] = None
            elif k.startswith("psnr") or k == "txt_psnr_c":
                fin = [x for x in vals if x != math.inf]  # lossless images would make the mean inf
                s[k] = float(np.mean(fin)) if fin else math.inf
            else:
                s[k] = float(np.mean(vals))
        s["n"] = len(rs)
        s["min_txt_contrast"] = min((_num(r["metrics"]["txt_contrast"]) for r in rs), default=None)
        summ[v] = s
    return summ


def git_info():
    def g(*a):
        try:
            return subprocess.run(["git", *a], cwd=HERE, capture_output=True, text=True, timeout=10).stdout.strip()
        except (OSError, subprocess.SubprocessError):
            return ""
    return {"commit": g("rev-parse", "--short", "HEAD") or None, "dirty": bool(g("status", "--porcelain", "--untracked-files=no", "--", ROOT))}


def find_vmaf(arg, ffmpeg="ffmpeg"):
    for cand in (arg, os.environ.get("OO_VMAF_FFMPEG"), ffmpeg):
        if cand and P.have_libvmaf_at(cand):
            return cand
    return None


# ---------------------------------------------------------------- markdown

def md_run(doc):
    cfg, env = doc["agent_config"], doc["env"]
    cols = list(METRICS)
    hdr = "| " + " | ".join(METRICS[k][0] for k in cols) + " |"
    sep = "|" + "---|" * len(cols)
    L = ["# bench/quality — metrics before/after encode (SIMULATION)", "",
         "> **SIMULATION, NOT MEASURED ON WINDOWS.** Synthetic corpus, the agent's MFT replaced by x264 configured "
         "from the agent's own settings (parsed from the source tree), decoded by ffmpeg, scored against the original "
         "PNG (\"before\" = the screenshot, \"after\" = what the decoder hands the browser, chroma upsampled bilinearly). "
         "No DXGI capture, no MediaFoundation, no browser compositor. Use for relative comparison between commits/variants.", "",
         f"Generated by `bench/quality/metrics_run.py` at {doc['created']} on commit `{doc['git']['commit']}`"
         f"{' (dirty tree)' if doc['git']['dirty'] else ''}. JSON: `{doc.get('json_name', 'metrics.json')}` "
         "(diff two runs with `metrics_run.py compare A.json B.json`).", "",
         "## Setup", "",
         f"- Encoder: {env['ffmpeg']} / {env.get('x264') or 'libx264'}, preset veryfast, tune zerolatency, threads {env['threads']} (pinned: sliced threads + VBV are not bit-exact run to run), "
         f"B={cfg['bframes']}, refs={cfg['refs']}, GOP {cfg['gop_seconds']} s x {cfg['fps']} fps, no scene-cut IDR, "
         f"PCVBR: maxrate = {cfg['peak_ratio']}x mean, VBV = {cfg['hrd_seconds']} s of mean (agent/encode/mft.c peak_bps/hrd_bits). "
         f"Auto bitrate = {cfg['baseline_bps'] // 1000} kbit/s x pixels / {cfg['baseline_pixels']} px, cap "
         f"{cfg['max_auto_bps'] // 1000} kbit/s (output.go defaultBitrate). Agent profile ladder starts at {cfg['profile']}; "
         f"refine QPs {cfg['refine_qps']}." + (f" **Not parsed, defaults used: {cfg['_defaults']}.**" if cfg["_defaults"] else ""),
         f"- Profiles actually emitted (SPS profile-level-id): {', '.join(sorted({r['plid'] for r in doc['rows'] if r['plid']}))} "
         "— only 42xxxx/4dxxxx are Chrome-WebRTC safe (TASK.md: 64xxxx = 415).",
         "- Colour: RGB -> Y'CbCr BT.709 limited, 4:2:0 (2x2 box), decoder side bilinear chroma upsample.",
         "- VMAF: " + (f"libvmaf (default model) from `{env['vmaf_ffmpeg']}` — on a still frame VMAF of an image against "
                       "itself is ~97.4, not 100 (temporal feature = 0)." if env["vmaf_ffmpeg"] else
                       "**n/a** — no ffmpeg with the libvmaf filter found (stock distro ffmpeg has only vmafmotion); "
                       "pass `--vmaf-ffmpeg PATH` or set `OO_VMAF_FFMPEG` (e.g. a BtbN/johnvansickle static build)."),
         "- Text metrics are OCR-free and use a text mask = pixels whose 3x3 luma range in the original > 64, dilated 1 px:",
         "  - *text edge-SSIM* — SSIM of gradient-magnitude maps inside the mask (stroke shape preserved);",
         "  - *text grad-energy* — sum|grad after|^2 / sum|grad before|^2 in the mask (1 = as sharp, <1 blurred, >1 ringing);",
         "  - *text contrast* — mean 3x3 luma range after / before in the mask (1 = full stroke contrast, <1 washed out);",
         "  - *text PSNR-CbCr* — chroma PSNR in the mask (ClearType fringes and coloured text; what 4:2:0 destroys).",
         "- kbit: idr = size of the IDR; steady = mean of the 29 static P-frames; refine = the single QP-fixed intra frame.",
         "- Not modelled: capture, the MFT's own rate control/AQ, NACK/PLI/loss, browser decode and compositor scaling, "
         "DPI. The real MFT (NVENC/QSV/AMF/MS software) will give different absolute numbers — UNVERIFIED on Windows.", "",
         "### Variants", ""]
    L += [f"- `{v}` — {d}" for v, d in doc["variants"].items()]
    L += ["", "## Mean over corpus", "", "| variant | n " + hdr, "|---|---" + sep]
    for v, s in doc["summary"].items():
        L.append(f"| {v} | {s['n']} | " + " | ".join(fmt(_num(s[k]), METRICS[k][1]) for k in cols) + " |")
    L += ["", "PSNR means skip lossless (inf) images.", ""]
    hl = highlights(doc)
    if hl:
        L += ["## Highlights (computed from the table above)", ""] + [f"- {h}" for h in hl] + [""]
    L += ["## Per image", ""]
    for img in dict.fromkeys(r["image"] for r in doc["rows"]):
        rs = [r for r in doc["rows"] if r["image"] == img]
        L += [f"### {img} ({rs[0]['w']}x{rs[0]['h']}, auto {auto_kbps(cfg, rs[0]['w'], rs[0]['h'])} kbit/s)", "",
              "| variant " + hdr, "|---" + sep]
        L += [f"| {r['variant']} | " + " | ".join(fmt(_num(r["metrics"][k]), METRICS[k][1]) for k in cols) + " |" for r in rs]
        L.append("")
    return "\n".join(L)


def highlights(doc):
    """A few machine-written sentences so the report reads without squinting at 100 rows."""
    S, rows, out = doc["summary"], doc["rows"], []

    def g(v, k):
        x = S.get(v, {}).get(k)
        return None if x is None else _num(x)

    def pair(a, b, label):
        if a in S and b in S:
            parts = [f"{METRICS[k][0]} {fmt(g(a, k), METRICS[k][1])} -> {fmt(g(b, k), METRICS[k][1])}"
                     for k in ("psnr_y", "ssim", "vmaf", "txt_essim", "txt_contrast") if g(a, k) is not None]
            ka, kb = g(a, "kbit"), g(b, "kbit")
            if ka and kb:
                parts.append(f"kbit {ka:.0f} -> {kb:.0f}")
            out.append(f"{label} (`{a}` -> `{b}`): " + "; ".join(parts) + ".")

    pair("main-idr", "main-refine", "Still screen in the agent: post-change frame -> refine")
    pair("main-idr", "main-steady", "x264 re-encoding the unchanged frame for 1 s instead (no skip; reference)")
    pair("main-steady", "cbase-steady", "Constrained Baseline instead of Main, same bitrate")
    pair("main-idr", "cbase-idr", "Constrained Baseline IDR")
    pair("main-steady", "main2m-steady", "2 Mbit/s instead of the auto bitrate")
    if "raw420" in S and "main-steady" in S:
        out.append(f"4:2:0 alone already costs chroma: text PSNR-CbCr {fmt(g('raw420', 'txt_psnr_c'))} dB with no codec "
                   f"(codec steady {fmt(g('main-steady', 'txt_psnr_c'))} dB) while PSNR-Y stays {fmt(g('raw420', 'psnr_y'))} dB "
                   "— the colour-fringe/coloured-text loss is the subsampling, not the bitrate.")
    for v in ("main-idr", "main2m-idr"):
        rs = [r for r in rows if r["variant"] == v and r["metrics"].get("txt_essim") is not None]
        if rs:
            w = min(rs, key=lambda r: _num(r["metrics"]["txt_essim"]))
            out.append(f"Worst text edge-SSIM in `{v}`: {w['image']} = {fmt(_num(w['metrics']['txt_essim']), 4)} "
                       f"(text contrast {fmt(_num(w['metrics']['txt_contrast']), 3)}).")
    return out


# ---------------------------------------------------------------- compare

def _worse(k, old, new):
    """How much worse `new` is than `old` for metric k (>0 = worse), in metric units."""
    kind = METRICS[k][2]
    if kind == "up":
        if old == math.inf and new == math.inf:
            return 0.0
        return old - new
    if kind == "one":
        return abs(1 - new) - abs(1 - old)
    return 0.0


def compare(a, b):
    """Returns (markdown, regressions list)."""
    ka = {(r["image"], r["variant"]): r for r in a["rows"]}
    kb = {(r["image"], r["variant"]): r for r in b["rows"]}
    common = [k for k in ka if k in kb]
    cols = list(METRICS)
    regs = []
    L = ["# bench/quality — metrics compare", "",
         f"A: commit `{a['git']['commit']}`{' (dirty)' if a['git']['dirty'] else ''}, {a['created']}  ",
         f"B: commit `{b['git']['commit']}`{' (dirty)' if b['git']['dirty'] else ''}, {b['created']}", ""]
    for key in ("ffmpeg", "x264", "vmaf_ffmpeg", "threads", "frames"):
        if a["env"].get(key) != b["env"].get(key):
            L.append(f"> **env differs** — {key}: `{a['env'].get(key)}` vs `{b['env'].get(key)}`; deltas include tool noise.")
    cfg_diff = [k for k in sorted(set(a["agent_config"]) | set(b["agent_config"]))
                if a["agent_config"].get(k) != b["agent_config"].get(k)]
    L += ["", "## Agent config", ""]
    L += [f"- `{k}`: {a['agent_config'].get(k)} -> {b['agent_config'].get(k)}" for k in cfg_diff] or ["- unchanged"]
    only_a = sorted(set(ka) - set(kb))
    only_b = sorted(set(kb) - set(ka))
    if only_a or only_b:
        L += ["", f"Rows only in A: {len(only_a)}, only in B: {len(only_b)} (not compared)."]
    L += ["", "## Mean delta per variant (B - A, common rows)", "",
          "| variant | n | " + " | ".join("Δ " + METRICS[k][0] for k in cols) + " |", "|---|---|" + "---|" * len(cols)]
    for v in dict.fromkeys(k[1] for k in common):
        ks = [k for k in common if k[1] == v]
        cells = []
        for m in cols:
            d = [_num(kb[k]["metrics"].get(m)) - _num(ka[k]["metrics"].get(m)) for k in ks
                 if ka[k]["metrics"].get(m) is not None and kb[k]["metrics"].get(m) is not None
                 and math.isfinite(_num(ka[k]["metrics"][m])) and math.isfinite(_num(kb[k]["metrics"][m]))]
            cells.append(f"{np.mean(d):+.{max(METRICS[m][1], 2)}f}" if d else "n/a")
        L.append(f"| {v} | {len(ks)} | " + " | ".join(cells) + " |")
    for k in common:
        for m, th in THRESH.items():
            o, n = ka[k]["metrics"].get(m), kb[k]["metrics"].get(m)
            if o is None or n is None:
                continue
            o, n = _num(o), _num(n)
            if (isinstance(o, float) and math.isnan(o)) or (isinstance(n, float) and math.isnan(n)):
                continue
            w = _worse(m, o, n)
            if w > th:
                regs.append({"image": k[0], "variant": k[1], "metric": m, "a": o, "b": n, "worse_by": w})
    L += ["", f"## Regressions (worse than threshold: {', '.join(f'{m} {t}' for m, t in THRESH.items())})", ""]
    if regs:
        L += ["| image | variant | metric | A | B |", "|---|---|---|---|---|"]
        L += [f"| {r['image']} | {r['variant']} | {r['metric']} | {fmt(r['a'], METRICS[r['metric']][1])} | "
              f"{fmt(r['b'], METRICS[r['metric']][1])} |" for r in regs]
    else:
        L.append("None.")
    return "\n".join(L) + "\n", regs


# ---------------------------------------------------------------- CLI

def cmd_run(a):
    cfg = agent_config()
    corpora = a.corpus or CORPORA
    paths = sorted(p for c in corpora for p in glob.glob(os.path.join(c, "*.png")))
    if a.quick:
        paths = [p for p in paths if "1440p" not in os.path.basename(p)]
    if a.images:
        want = a.images.split(",")
        paths = [p for p in paths if any(w in os.path.basename(p) for w in want)]
    variants = a.variants.split(",") if a.variants else (QUICK_VARIANTS if a.quick else list(VARIANTS))
    bad = [v for v in variants if v not in VARIANTS]
    if bad:
        sys.exit(f"unknown variants {bad}; known: {list(VARIANTS)}")
    if not paths:
        sys.exit("no corpus images found")
    if not P.have_ffmpeg() and variants != ["raw420"]:
        sys.exit("ffmpeg with libx264 not found (needed for every variant but raw420)")
    vmaf_ff = None if a.no_vmaf else find_vmaf(a.vmaf_ffmpeg, a.ffmpeg)
    t0 = time.time()
    if a.jobs > 1:
        with concurrent.futures.ProcessPoolExecutor(a.jobs) as ex:
            futs = [ex.submit(run_image, p, variants, cfg, a.ffmpeg, vmaf_ff, a.frames) for p in paths]
            rows = [r for f in futs for r in f.result()]
    else:
        rows = [r for p in paths for r in run_image(p, variants, cfg, a.ffmpeg, vmaf_ff, a.frames)]
    doc = {"schema": SCHEMA, "tool": "bench/quality/metrics_run.py",
           "created": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
           "git": git_info(), "agent_config": cfg,
           "env": {"ffmpeg": P.ffmpeg_version(a.ffmpeg),
                   "x264": next((r["x264"] for r in rows if r["x264"]), None), "vmaf_ffmpeg": P.ffmpeg_version(vmaf_ff) if vmaf_ff else None,
                   "threads": 1, "frames": a.frames, "numpy": np.__version__, "seconds": round(time.time() - t0, 1)},
           "variants": {v: VARIANTS[v] for v in variants}, "rows": rows, "summary": summarize(rows, variants),
           "json_name": os.path.basename(a.json)}
    doc = _jsonable(doc)
    with open(a.json, "w") as f:
        f.write(dump_json(doc))
    with open(a.md, "w") as f:
        f.write(md_run(doc) + "\n")
    print("wrote", a.json, a.md, f"({time.time() - t0:.0f}s)")


def dump_json(doc):
    """Indented JSON with one row per line: small, and `git diff` shows exactly the rows that moved."""
    head = {k: v for k, v in doc.items() if k != "rows"}
    body = json.dumps(head, indent=1, ensure_ascii=False)[:-2]
    rows = ",\n  ".join(json.dumps(r, ensure_ascii=False) for r in doc["rows"])
    return body + ',\n "rows": [\n  ' + rows + "\n ]\n}\n"


def cmd_compare(a):
    with open(a.a) as f:
        A = json.load(f)
    with open(a.b) as f:
        B = json.load(f)
    md, regs = compare(A, B)
    if a.md:
        with open(a.md, "w") as f:
            f.write(md)
    print(md)
    if regs and a.fail_on_regression:
        sys.exit(1)


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv or argv[0] not in ("run", "compare", "-h", "--help"):
        argv.insert(0, "run")
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run", help="score the corpus, write markdown + JSON")
    r.add_argument("--corpus", action="append", help="PNG directory (repeatable); default bench/corpus + bench/corpus/screens")
    r.add_argument("--md", default=os.path.join(HERE, "RESULTS-metrics.md"))
    r.add_argument("--json", default=os.path.join(HERE, "RESULTS-metrics.json"))
    r.add_argument("--images", help="comma-separated substrings of image names to keep")
    r.add_argument("--variants", help="comma-separated subset of: " + ",".join(VARIANTS))
    r.add_argument("--quick", action="store_true", help="1080p only and 4 variants")
    r.add_argument("--frames", type=int, default=30, help="static frames per encode (steady = last)")
    r.add_argument("--ffmpeg", default="ffmpeg", help="ffmpeg with libx264 used for encode/decode")
    r.add_argument("--vmaf-ffmpeg", help="ffmpeg with libvmaf (default: $OO_VMAF_FFMPEG, then --ffmpeg)")
    r.add_argument("--no-vmaf", action="store_true")
    r.add_argument("--jobs", type=int, default=1, help="images scored in parallel (processes)")
    r.set_defaults(fn=cmd_run)
    c = sub.add_parser("compare", help="diff two JSON runs")
    c.add_argument("a")
    c.add_argument("b")
    c.add_argument("--md", help="also write the diff markdown here")
    c.add_argument("--fail-on-regression", action="store_true", help="exit 1 if any metric regressed past THRESH")
    c.set_defaults(fn=cmd_compare)
    a = ap.parse_args(argv)
    a.fn(a)


if __name__ == "__main__":
    main()
