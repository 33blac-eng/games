"""bench/quality/metrics.py — PSNR, SSIM, text-sharpness metrics (numpy only)."""
import math
import numpy as np


def luma(rgb):
    """BT.709 luma (full-range float 0..255) from HxWx3 uint8/float."""
    a = np.asarray(rgb, dtype=np.float64)
    if a.ndim == 2:
        return a
    return 0.2126 * a[..., 0] + 0.7152 * a[..., 1] + 0.0722 * a[..., 2]


def psnr(ref, dist, peak=255.0):
    ref = np.asarray(ref, dtype=np.float64)
    dist = np.asarray(dist, dtype=np.float64)
    mse = np.mean((ref - dist) ** 2)
    if mse == 0:
        return math.inf
    return 10.0 * math.log10(peak * peak / mse)


def _gauss_kernel(size=11, sigma=1.5):
    x = np.arange(size) - (size - 1) / 2.0
    k = np.exp(-(x ** 2) / (2 * sigma ** 2))
    return k / k.sum()


def _filt(img, k):
    """Separable 'valid' convolution."""
    n = len(k)
    h, w = img.shape
    out = np.zeros((h, w - n + 1))
    for i, c in enumerate(k):
        out += c * img[:, i:i + w - n + 1]
    out2 = np.zeros((h - n + 1, out.shape[1]))
    for i, c in enumerate(k):
        out2 += c * out[i:i + h - n + 1, :]
    return out2


def ssim_map(ref, dist, peak=255.0):
    """Wang et al. 2004 SSIM map (Gaussian 11x11, sigma 1.5) on 2-D arrays."""
    x = np.asarray(ref, dtype=np.float64)
    y = np.asarray(dist, dtype=np.float64)
    c1 = (0.01 * peak) ** 2
    c2 = (0.03 * peak) ** 2
    k = _gauss_kernel()
    mx, my = _filt(x, k), _filt(y, k)
    sxx = _filt(x * x, k) - mx * mx
    syy = _filt(y * y, k) - my * my
    sxy = _filt(x * y, k) - mx * my
    return ((2 * mx * my + c1) * (2 * sxy + c2)) / ((mx * mx + my * my + c1) * (sxx + syy + c2))


def ssim(ref, dist, peak=255.0):
    return float(np.mean(ssim_map(luma(ref), luma(dist), peak)))


def gradient_mag(y):
    gx = np.zeros_like(y)
    gy = np.zeros_like(y)
    gx[:, 1:-1] = (y[:, 2:] - y[:, :-2]) / 2.0
    gy[1:-1, :] = (y[2:, :] - y[:-2, :]) / 2.0
    return np.sqrt(gx * gx + gy * gy)


def text_mask(rgb, thresh=64.0, dilate=1):
    """High-contrast pixels of the original: local 3x3 luma range > thresh, dilated."""
    y = luma(rgb)
    p = np.pad(y, 1, mode="edge")
    h, w = y.shape
    win = [p[i:i + h, j:j + w] for i in range(3) for j in range(3)]
    m = (np.max(win, axis=0) - np.min(win, axis=0)) > thresh
    for _ in range(dilate):
        q = np.pad(m, 1)
        m = np.any([q[i:i + h, j:j + w] for i in range(3) for j in range(3)], axis=0)
    return m


def text_sharpness(ref, dist, mask=None):
    """Returns (edge_ssim, grad_energy_ratio) inside the text mask.

    edge_ssim: SSIM between gradient-magnitude maps, averaged over masked pixels.
    grad_energy_ratio: sum(|grad dist|^2)/sum(|grad ref|^2) in mask (1.0 = as sharp,
    <1 = blurred, >1 = ringing/noise)."""
    if mask is None:
        mask = text_mask(ref)
    gr = gradient_mag(luma(ref))
    gd = gradient_mag(luma(dist))
    if not mask.any():
        return float("nan"), float("nan")
    smap = ssim_map(gr, gd, peak=255.0)
    o = 5  # valid-conv offset for 11-tap kernel
    mm = mask[o:o + smap.shape[0], o:o + smap.shape[1]]
    es = float(np.mean(smap[mm])) if mm.any() else float("nan")
    er = float(np.sum(gd[mask] ** 2) / max(np.sum(gr[mask] ** 2), 1e-12))
    return es, er
