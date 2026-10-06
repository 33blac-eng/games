//go:build windows

/* dxgi.c — DXGI Desktop Duplication + cursor composite + GPU BGRA->NV12.
 *
 * Pipeline, created once and reused for every frame (Ф0 plan §4):
 *
 *   IDXGIOutputDuplication::AcquireNextFrame
 *        -> CopyResource into our own BGRA texture (the acquired one is
 *           only valid until ReleaseFrame, so we get off it immediately)
 *        -> composite the mouse pointer INTO that BGRA texture
 *        -> ID3D11VideoContext::VideoProcessorBlt  (BGRA -> NV12, GPU)
 *        -> CopyResource into an NV12 staging texture + Map  (Ф0 readback)
 *
 * The readback is deliberate and temporary: there is no encoder yet, so the
 * frame has to reach Go as bytes. See the TODO in capture_windows.go about the
 * zero-copy path (hand the NV12 ID3D11Texture2D straight to the MFT).
 *
 * Cursor compositing note: DXGI hands us the pointer as a separate shape, and
 * the desktop texture never contains it. We blend it on the CPU over a small
 * readback of just the pointer rectangle and push it back with
 * UpdateSubresource. That is a few KB per frame and gets monochrome, color and
 * masked-color all correct on Ф0; a compute-shader blend is the optimisation.
 */

#include <windows.h>
#include <d3d11.h>
#include <d3d11_1.h>
#include <dxgi1_2.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>

#include "dxgi.h"

/* mingw's libdxguid does not reliably export the video GUIDs, so we carry our
 * own copies rather than risk a link error. */
static const GUID OOS_IID_IDXGIOutput1 =
    {0x00cddea8,0x939b,0x4b83,{0xa3,0x40,0xa6,0x85,0x22,0x66,0x66,0xcc}};
static const GUID OOS_IID_IDXGIFactory1 =
    {0x770aae78,0xf26f,0x4dba,{0xa8,0x29,0x25,0x3c,0x83,0xd1,0xb3,0x87}};
static const GUID OOS_IID_ID3D11VideoDevice =
    {0x10ec4d5b,0x975a,0x4689,{0xb9,0xe4,0xd0,0xaa,0xc3,0x0f,0xe3,0x33}};
static const GUID OOS_IID_ID3D11VideoContext =
    {0x61f21c45,0x3c0e,0x4a74,{0x9c,0xea,0x67,0x10,0x0d,0x9a,0xd5,0xe4}};
static const GUID OOS_IID_ID3D11Texture2D =
    {0x6f15aaf2,0xd208,0x4e89,{0x9a,0xb4,0x48,0x95,0x35,0xd3,0x4f,0x9c}};
static const GUID OOS_IID_ID3D10Multithread =
    {0x9b7e4e00,0x342c,0x4106,{0xa1,0x9f,0x4f,0x27,0x04,0xf6,0x89,0xf0}};

#define SAFE_RELEASE(p) do { if (p) { (p)->lpVtbl->Release(p); (p) = NULL; } } while (0)

struct oos_cap {
    int32_t output_idx;

    ID3D11Device        *dev;
    ID3D11DeviceContext *ctx;
    IDXGIOutput1        *out1;
    IDXGIOutputDuplication *dupl;

    ID3D11VideoDevice   *vdev;
    ID3D11VideoContext  *vctx;
    ID3D11VideoProcessor *vproc;
    ID3D11VideoProcessorEnumerator *venum;
    ID3D11VideoProcessorOutputView *vout;
    ID3D11VideoProcessorInputView  *vin;

    ID3D11Texture2D *bgra;        /* our stable copy of the desktop */
    ID3D11Texture2D *nv12;        /* VideoProcessorBlt target */
    ID3D11Texture2D *nv12_stage;  /* CPU readback (Ф0 only) */
    ID3D11Texture2D *cur_stage;   /* small readback for cursor compositing */
    int32_t cur_stage_w, cur_stage_h;

    int32_t width, height;

    /* Cached pointer state; DXGI only re-sends the shape when it changes. */
    uint8_t *shape;
    uint32_t shape_cap;
    /* Blended cursor rectangle on its way back into bgra; kept across frames
     * (like shape) so compositing does no malloc/free per frame. */
    uint8_t *cur_tmp;
    size_t cur_tmp_cap;
    DXGI_OUTDUPL_POINTER_SHAPE_INFO shape_info;
    int32_t have_shape;
    int32_t cur_visible;
    int32_t cur_x, cur_y;

    /* A-24: the GDI fallback draws the pointer itself (DrawIconEx), so
     * convert_out must NOT composite the cached DXGI shape on top of it. Set by
     * oos_gdi_next, consumed and cleared by convert_out. */
    int32_t cursor_in_bgra;

    /* Cursor layer (agent -cursor-layer): 1 -> the pointer is NOT composited
     * into the image (neither DXGI shape nor GDI DrawIconEx); it travels as a
     * separate shape+position stream. Pointer-only updates are then no-ops.
     * shape_seq bumps on every new DXGI shape so the consumer knows when to
     * pull it with oos_cursor_shape. */
    int32_t cursor_layer;
    uint32_t shape_seq;

    /* Gap #2: scratch for GetFrameMoveRects/GetFrameDirtyRects, and whether
     * c->bgra holds a real desktop image yet (a no-op frame is only a no-op
     * relative to something we already converted). */
    uint8_t *meta;
    uint32_t meta_cap;
    int32_t have_image;

    int32_t mapped;
    D3D11_MAPPED_SUBRESOURCE map;

    int32_t no_readback;  /* 1 -> skip CopyResource+Map: encoder takes the texture */
    int64_t cpu_maps;     /* how many times we mapped NV12 on the CPU */

    HRESULT last_hr;

    /* Windows 7: no Desktop Duplication (IDXGIOutput1, Win8+) and no D3D11
     * video processor. gdi_only = the whole pipeline is BitBlt -> CPU
     * BGRA->NV12, no D3D device at all (oos_device returns NULL, so the
     * encoder takes its software/CPU path). See gdi_only_open. */
    int32_t gdi_only;
    RECT gdi_desk;          /* output rectangle in virtual-desktop coords */
    uint8_t *gdi_bgra;      /* current BitBlt image (top-down, w*4 pitch) */
    uint8_t *gdi_prev;      /* previous one: unchanged screen -> OOS_TIMEOUT */
    uint8_t *gdi_nv12;      /* Y plane, then interleaved UV (pitch = gdi_pitch) */
    int32_t gdi_pitch;
    ULONGLONG gdi_last_ms;  /* GetTickCount64 of the last grab (frame pacing) */
};

static void set_err(char *err, int32_t n, const char *what, HRESULT hr)
{
    if (!err || n <= 0) return;
    _snprintf(err, (size_t)n - 1, "%s: hr=0x%08lX", what, (unsigned long)hr);
    err[n - 1] = 0;
}

static int classify(HRESULT hr)
{
    /* A-07: INVALID_CALL gets its own code. Same recovery (recreate), separate
     * name — see the enum comment in dxgi.h. */
    if (hr == DXGI_ERROR_INVALID_CALL)
        return OOS_INVALID_CALL;
    if (hr == DXGI_ERROR_ACCESS_LOST || hr == DXGI_ERROR_DEVICE_REMOVED ||
        hr == DXGI_ERROR_DEVICE_RESET || hr == E_ACCESSDENIED)
        return OOS_ACCESS_LOST;
    return OOS_ERROR;
}

/* ---------------------------------------------------------------- pipeline */

static HRESULT make_textures(oos_cap *c)
{
    D3D11_TEXTURE2D_DESC d;
    HRESULT hr;

    memset(&d, 0, sizeof(d));
    d.Width = (UINT)c->width;
    d.Height = (UINT)c->height;
    d.MipLevels = 1;
    d.ArraySize = 1;
    d.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
    d.SampleDesc.Count = 1;
    d.Usage = D3D11_USAGE_DEFAULT;
    d.BindFlags = D3D11_BIND_RENDER_TARGET | D3D11_BIND_SHADER_RESOURCE;
    hr = c->dev->lpVtbl->CreateTexture2D(c->dev, &d, NULL, &c->bgra);
    if (FAILED(hr)) return hr;

    memset(&d, 0, sizeof(d));
    d.Width = (UINT)((c->width + 1) & ~1);
    d.Height = (UINT)((c->height + 1) & ~1);
    d.MipLevels = 1;
    d.ArraySize = 1;
    d.Format = DXGI_FORMAT_NV12;
    d.SampleDesc.Count = 1;
    d.Usage = D3D11_USAGE_DEFAULT;
    d.BindFlags = D3D11_BIND_RENDER_TARGET;
    hr = c->dev->lpVtbl->CreateTexture2D(c->dev, &d, NULL, &c->nv12);
    if (FAILED(hr)) return hr;

    d.BindFlags = 0;
    d.Usage = D3D11_USAGE_STAGING;
    d.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
    hr = c->dev->lpVtbl->CreateTexture2D(c->dev, &d, NULL, &c->nv12_stage);
    return hr;
}

static HRESULT make_video(oos_cap *c)
{
    D3D11_VIDEO_PROCESSOR_CONTENT_DESC cd;
    D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd;
    D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC ivd;
    HRESULT hr;

    hr = c->dev->lpVtbl->QueryInterface(c->dev, &OOS_IID_ID3D11VideoDevice,
                                        (void **)&c->vdev);
    if (FAILED(hr)) return hr;
    hr = c->ctx->lpVtbl->QueryInterface(c->ctx, &OOS_IID_ID3D11VideoContext,
                                        (void **)&c->vctx);
    if (FAILED(hr)) return hr;

    memset(&cd, 0, sizeof(cd));
    cd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
    cd.InputWidth = (UINT)c->width;
    cd.InputHeight = (UINT)c->height;
    cd.OutputWidth = (UINT)((c->width + 1) & ~1);
    cd.OutputHeight = (UINT)((c->height + 1) & ~1);
    cd.Usage = D3D11_VIDEO_USAGE_PLAYBACK_NORMAL;
    hr = c->vdev->lpVtbl->CreateVideoProcessorEnumerator(c->vdev, &cd, &c->venum);
    if (FAILED(hr)) return hr;
    hr = c->vdev->lpVtbl->CreateVideoProcessor(c->vdev, c->venum, 0, &c->vproc);
    if (FAILED(hr)) return hr;

    memset(&ovd, 0, sizeof(ovd));
    ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
    hr = c->vdev->lpVtbl->CreateVideoProcessorOutputView(
        c->vdev, (ID3D11Resource *)c->nv12, c->venum, &ovd, &c->vout);
    if (FAILED(hr)) return hr;

    memset(&ivd, 0, sizeof(ivd));
    ivd.FourCC = 0;
    ivd.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
    ivd.Texture2D.MipSlice = 0;
    ivd.Texture2D.ArraySlice = 0;
    hr = c->vdev->lpVtbl->CreateVideoProcessorInputView(
        c->vdev, (ID3D11Resource *)c->bgra, c->venum, &ivd, &c->vin);
    if (FAILED(hr)) return hr;

    c->vctx->lpVtbl->VideoProcessorSetStreamFrameFormat(
        c->vctx, c->vproc, 0, D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE);
    c->vctx->lpVtbl->VideoProcessorSetStreamOutputRate(
        c->vctx, c->vproc, 0, D3D11_VIDEO_PROCESSOR_OUTPUT_RATE_NORMAL, TRUE, NULL);
    /* Аудит якості Q-01 (research/QUALITY-AUDIT.md): за документацією D3D11
     * драйвер МОЖЕ сам вмикати «покращення» під час blit (auto-processing:
     * шумодав, підсилення країв тощо), а для тексту робочого столу кожне з
     * них — спотворення ще ДО енкодера. Скейлер енкодера (mft.c, make_scaler)
     * це вже вимикає; захоплення BGRA -> NV12 — ні. Те саме тут: auto-
     * processing off і кожен фільтр, що його оголошує процесор, явно вимкнено.
     * Void-виклики, перевіряти нічого. Чи вмикав щось драйвер на RGB-вході
     * (і, отже, чи є видимий виграш) — UNVERIFIED на залізі. */
    c->vctx->lpVtbl->VideoProcessorSetStreamAutoProcessingMode(c->vctx, c->vproc, 0, FALSE);
    {
        D3D11_VIDEO_PROCESSOR_CAPS caps;
        memset(&caps, 0, sizeof(caps));
        if (SUCCEEDED(c->venum->lpVtbl->GetVideoProcessorCaps(c->venum, &caps))) {
            int f;
            for (f = D3D11_VIDEO_PROCESSOR_FILTER_BRIGHTNESS;
                 f <= D3D11_VIDEO_PROCESSOR_FILTER_STEREO_ADJUSTMENT; f++) {
                if (caps.FilterCaps & (1u << f))
                    c->vctx->lpVtbl->VideoProcessorSetStreamFilter(
                        c->vctx, c->vproc, 0, (D3D11_VIDEO_PROCESSOR_FILTER)f, FALSE, 0);
            }
        }
    }
    /* Desktop is full-range sRGB; the NV12 we emit is studio-range BT.709,
     * which is what the encoder and every browser decoder expect. */
    {
        D3D11_VIDEO_PROCESSOR_COLOR_SPACE in_cs, out_cs;
        memset(&in_cs, 0, sizeof(in_cs));
        in_cs.Usage = 0;            /* playback */
        in_cs.RGB_Range = 0;        /* full */
        in_cs.YCbCr_Matrix = 1;     /* BT.709 */
        in_cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_0_255;
        c->vctx->lpVtbl->VideoProcessorSetStreamColorSpace(c->vctx, c->vproc, 0, &in_cs);

        memset(&out_cs, 0, sizeof(out_cs));
        out_cs.Usage = 0;
        out_cs.YCbCr_Matrix = 1;
        out_cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
        c->vctx->lpVtbl->VideoProcessorSetOutputColorSpace(c->vctx, c->vproc, &out_cs);
    }
    return S_OK;
}

static HRESULT make_dupl(oos_cap *c)
{
    DXGI_OUTDUPL_DESC dd;
    HRESULT hr = c->out1->lpVtbl->DuplicateOutput(c->out1,
                                                  (IUnknown *)c->dev, &c->dupl);
    if (FAILED(hr)) return hr;
    c->have_image = 0;  /* fresh duplication: never call its first frame a no-op */
    c->dupl->lpVtbl->GetDesc(c->dupl, &dd);
    c->width = (int32_t)dd.ModeDesc.Width;
    c->height = (int32_t)dd.ModeDesc.Height;
    return S_OK;
}

/* The adapter Desktop Duplication runs on. DXGI 1.1 puts the adapter whose
 * output shows the desktop primary at index 0, which is the one that owns the
 * outputs we duplicate.
 *
 * Both the output count and oos_open MUST come through here. They used to pick
 * an adapter independently: the count from EnumAdapters1(0), the capture from
 * whatever D3D11CreateDevice(NULL) returned. On one GPU that is the same chip
 * and the split is invisible; on a hybrid laptop (Intel iGPU + discrete
 * NVIDIA/AMD) it is not. The default adapter follows the per-application GPU
 * preference, and on a muxless Optimus box the discrete adapter drives no
 * display at all, so the count reports the iGPU's monitor while EnumOutputs on
 * the discrete adapter fails with DXGI_ERROR_NOT_FOUND. */
static HRESULT default_adapter(IDXGIAdapter1 **out)
{
    IDXGIFactory1 *f = NULL;
    HRESULT hr;
    *out = NULL;
    hr = CreateDXGIFactory1(&OOS_IID_IDXGIFactory1, (void **)&f);
    if (FAILED(hr)) return hr;
    hr = f->lpVtbl->EnumAdapters1(f, 0, out);
    SAFE_RELEASE(f);
    return hr;
}

int32_t oos_output_count(void)
{
    IDXGIAdapter1 *a = NULL;
    IDXGIOutput *o = NULL;
    int32_t n = 0;
    if (FAILED(default_adapter(&a))) return -1;
    /* Only S_OK means "there is another output". Any other persistent HRESULT
     * (E_INVALIDARG, DXGI_ERROR_DEVICE_REMOVED, ...) used to fall through the
     * != NOT_FOUND test and spin here forever on the same index. */
    for (;;) {
        HRESULT hr = a->lpVtbl->EnumOutputs(a, (UINT)n, &o);
        if (hr == DXGI_ERROR_NOT_FOUND) break;
        if (hr != S_OK) { SAFE_RELEASE(a); return -1; }
        SAFE_RELEASE(o);
        n++;
    }
    SAFE_RELEASE(a);
    return n;
}

/* Геометрія одного виходу БЕЗ відкриття дуплікації: консолі треба показати
 * список моніторів, а платити за це створенням D3D11-девайса на кожен монітор
 * не варто. Ідемо тим самим default_adapter(), що й oos_open — інакше індекс у
 * списку не збігався б з індексом, який приймає oos_open (та сама пастка про
 * гібридний Optimus, що описана над default_adapter).
 *
 * Розміри — з DesktopCoordinates, тобто такі, якими монітор бачить людина
 * (повернутий екран уже врахований); ModeDesc дуплікації неповернутий, і для
 * енкодера ми беремо саме його через oos_width/oos_height. */
int oos_output_info(int32_t idx, int32_t *width, int32_t *height, int32_t *primary)
{
    IDXGIAdapter1 *a = NULL;
    IDXGIOutput *o = NULL;
    DXGI_OUTPUT_DESC od;
    MONITORINFO mi;

    if (!width || !height || !primary) return OOS_ERROR;
    *width = 0; *height = 0; *primary = 0;
    if (idx < 0) return OOS_ERROR;
    if (FAILED(default_adapter(&a))) return OOS_ERROR;
    if (a->lpVtbl->EnumOutputs(a, (UINT)idx, &o) != S_OK) { SAFE_RELEASE(a); return OOS_ERROR; }
    if (FAILED(o->lpVtbl->GetDesc(o, &od))) { SAFE_RELEASE(o); SAFE_RELEASE(a); return OOS_ERROR; }
    SAFE_RELEASE(o);
    SAFE_RELEASE(a);

    *width  = (int32_t)(od.DesktopCoordinates.right - od.DesktopCoordinates.left);
    *height = (int32_t)(od.DesktopCoordinates.bottom - od.DesktopCoordinates.top);

    memset(&mi, 0, sizeof(mi));
    mi.cbSize = sizeof(mi);
    if (od.Monitor && GetMonitorInfoW(od.Monitor, &mi) && (mi.dwFlags & MONITORINFOF_PRIMARY))
        *primary = 1;
    return OOS_OK;
}

/* DesktopCoordinates виходу idx — де він лежить на віртуальному робочому
 * столі. Для шару курсора: GetCursorInfo дає позицію в координатах
 * віртуального столу, а DXGI — відносно виходу. Як і oos_output_info, без
 * дуплікації й без стану капчера — можна кликати з будь-якої горутини. */
int oos_output_rect(int32_t idx, int32_t *left, int32_t *top, int32_t *right, int32_t *bottom)
{
    IDXGIAdapter1 *a = NULL;
    IDXGIOutput *o = NULL;
    DXGI_OUTPUT_DESC od;

    if (!left || !top || !right || !bottom || idx < 0) return OOS_ERROR;
    if (FAILED(default_adapter(&a))) return OOS_ERROR;
    if (a->lpVtbl->EnumOutputs(a, (UINT)idx, &o) != S_OK) { SAFE_RELEASE(a); return OOS_ERROR; }
    if (FAILED(o->lpVtbl->GetDesc(o, &od))) { SAFE_RELEASE(o); SAFE_RELEASE(a); return OOS_ERROR; }
    SAFE_RELEASE(o);
    SAFE_RELEASE(a);
    *left = (int32_t)od.DesktopCoordinates.left;
    *top = (int32_t)od.DesktopCoordinates.top;
    *right = (int32_t)od.DesktopCoordinates.right;
    *bottom = (int32_t)od.DesktopCoordinates.bottom;
    return OOS_OK;
}

static int gdi_draw_cursor(oos_cap *c, HDC dc, const RECT *desk);

/* F9 (дефолт ON): чи справді прибирати вказівник із картинки. Шар курсора
 * має сенс лише тоді, коли є ДЖЕРЕЛО ФОРМИ (DXGI pointer shape). На
 * GDI-only (Windows 7, без DXGI-дуплікації) і до першої форми DXGI форми
 * нема — глядач із шаром не мав би ЖОДНОГО вказівника. Тож тоді малюємо
 * вказівник у кадр, як без шару (DrawIconEx / composite), навіть коли хаб
 * дозволив шар. */
static int layer_hides_pointer(const oos_cap *c)
{
    return c->cursor_layer && !c->gdi_only && c->have_shape;
}

/* BT.709 limited range, the same colorimetry the DXGI video processor path
 * advertises, so a Windows 7 PC does not look different from the others. */
static void gdi_bgra_to_nv12(oos_cap *c)
{
    int32_t w = c->width, h = c->height, pitch = c->gdi_pitch, x, y;
    int32_t ah = (h + 1) & ~1;
    uint8_t *Y = c->gdi_nv12, *UV = c->gdi_nv12 + (size_t)pitch * (size_t)ah;
    const uint8_t *src = c->gdi_bgra;
    for (y = 0; y < h; y++) {
        const uint8_t *s = src + (size_t)y * (size_t)w * 4;
        uint8_t *d = Y + (size_t)y * (size_t)pitch;
        for (x = 0; x < w; x++, s += 4)
            d[x] = (uint8_t)((47 * s[2] + 157 * s[1] + 16 * s[0] + 128) / 256 + 16);
    }
    for (y = 0; y < ah / 2; y++) {
        int32_t y0 = 2 * y, y1 = (2 * y + 1 < h) ? 2 * y + 1 : 2 * y;
        const uint8_t *r0 = src + (size_t)y0 * (size_t)w * 4, *r1 = src + (size_t)y1 * (size_t)w * 4;
        uint8_t *d = UV + (size_t)y * (size_t)pitch;
        for (x = 0; x < w; x += 2) {
            int32_t x1 = (x + 1 < w) ? x + 1 : x;
            int bb = r0[x*4] + r0[x1*4] + r1[x*4] + r1[x1*4];
            int gg = r0[x*4+1] + r0[x1*4+1] + r1[x*4+1] + r1[x1*4+1];
            int rr = r0[x*4+2] + r0[x1*4+2] + r1[x*4+2] + r1[x1*4+2];
            d[x]     = (uint8_t)((-26 * rr - 87 * gg + 112 * bb + 512) / 1024 + 128);
            d[x + 1] = (uint8_t)((112 * rr - 102 * gg - 10 * bb + 512) / 1024 + 128);
        }
    }
}

static int gdi_only_open(oos_cap *c, char *err, int32_t err_len)
{
    IDXGIAdapter1 *a = NULL;
    IDXGIOutput *o = NULL;
    DXGI_OUTPUT_DESC od;
    size_t n;
    /* DXGI 1.1 enumeration exists on Windows 7: same indices as oos_open. */
    memset(&od, 0, sizeof(od));
    if (FAILED(default_adapter(&a)) ||
        a->lpVtbl->EnumOutputs(a, (UINT)c->output_idx, &o) != S_OK ||
        FAILED(o->lpVtbl->GetDesc(o, &od))) {
        SAFE_RELEASE(o); SAFE_RELEASE(a);
        if (c->output_idx != 0) { set_err(err, err_len, "gdi: EnumOutputs", E_FAIL); return OOS_ERROR; }
        od.DesktopCoordinates.left = 0;
        od.DesktopCoordinates.top = 0;
        od.DesktopCoordinates.right = GetSystemMetrics(SM_CXSCREEN);
        od.DesktopCoordinates.bottom = GetSystemMetrics(SM_CYSCREEN);
    }
    SAFE_RELEASE(o); SAFE_RELEASE(a);
    c->gdi_desk = od.DesktopCoordinates;
    c->width = (int32_t)(od.DesktopCoordinates.right - od.DesktopCoordinates.left);
    c->height = (int32_t)(od.DesktopCoordinates.bottom - od.DesktopCoordinates.top);
    if (c->width <= 0 || c->height <= 0) { set_err(err, err_len, "gdi: empty output", E_FAIL); return OOS_ERROR; }
    c->gdi_pitch = (c->width + 15) & ~15;
    n = (size_t)c->width * (size_t)c->height * 4;
    c->gdi_bgra = (uint8_t *)malloc(n);
    c->gdi_prev = (uint8_t *)calloc(1, n);
    c->gdi_nv12 = (uint8_t *)calloc(1, (size_t)c->gdi_pitch * (size_t)((c->height + 1) & ~1) * 3 / 2);
    if (!c->gdi_bgra || !c->gdi_prev || !c->gdi_nv12) { set_err(err, err_len, "gdi: malloc", E_OUTOFMEMORY); return OOS_ERROR; }
    c->gdi_only = 1;
    return OOS_OK;
}

/* One BitBlt of this output into gdi_bgra (pointer drawn unless cursor layer). */
static int gdi_grab(oos_cap *c, char *err, int32_t err_len)
{
    BITMAPINFO bi;
    HDC screen, mem;
    HBITMAP bmp, old;
    void *bits = NULL;
    int ok = 0;
    screen = GetDC(NULL);
    if (!screen) { set_err(err, err_len, "gdi: GetDC", E_FAIL); return OOS_ACCESS_LOST; }
    mem = CreateCompatibleDC(screen);
    memset(&bi, 0, sizeof(bi));
    bi.bmiHeader.biSize = sizeof(BITMAPINFOHEADER);
    bi.bmiHeader.biWidth = c->width;
    bi.bmiHeader.biHeight = -c->height;
    bi.bmiHeader.biPlanes = 1;
    bi.bmiHeader.biBitCount = 32;
    bi.bmiHeader.biCompression = BI_RGB;
    bmp = mem ? CreateDIBSection(mem, &bi, DIB_RGB_COLORS, &bits, NULL, 0) : NULL;
    if (bmp && bits) {
        old = (HBITMAP)SelectObject(mem, bmp);
        if (BitBlt(mem, 0, 0, c->width, c->height, screen,
                   c->gdi_desk.left, c->gdi_desk.top, SRCCOPY | CAPTUREBLT)) {
            if (!layer_hides_pointer(c)) gdi_draw_cursor(c, mem, &c->gdi_desk);
            GdiFlush();
            memcpy(c->gdi_bgra, bits, (size_t)c->width * (size_t)c->height * 4);
            ok = 1;
        } else {
            set_err(err, err_len, "gdi: BitBlt", HRESULT_FROM_WIN32(GetLastError()));
        }
        SelectObject(mem, old);
    } else {
        set_err(err, err_len, "gdi: CreateDIBSection", E_FAIL);
    }
    if (bmp) DeleteObject(bmp);
    if (mem) DeleteDC(mem);
    ReleaseDC(NULL, screen);
    /* A secure desktop (UAC, lock screen) makes BitBlt fail: same contract as
     * DXGI ACCESS_LOST, the Go side recreates and retries. */
    return ok ? OOS_OK : OOS_ACCESS_LOST;
}

/* Finishes a GDI-only oos_open with one real grab. A secure desktop (lock
 * screen, UAC) makes BitBlt fail; without this check gdi_only_open "succeeded"
 * there, the first oos_next reported ACCESS_LOST, and the Go reinit reopened
 * every MinBackoff (10 ms) — two log lines and ~19 MB of buffers per round —
 * for as long as the PC stayed locked, never giving up the way it does for
 * DXGI. Same contract as DuplicateOutput's E_ACCESSDENIED: OOS_ACCESS_LOST
 * from oos_open, and the caller backs off. The grabbed image is not reported:
 * have_image stays 0, so the first oos_next still returns a frame. */
static int gdi_only_start(oos_cap *c, oos_cap **out, char *err, int32_t err_len)
{
    int st = gdi_grab(c, err, err_len);
    if (st != OOS_OK) {
        oos_close(c);
        return st;
    }
    *out = c;
    return OOS_OK;
}

static void gdi_fill_frame(oos_cap *c, oos_frame *frame)
{
    memset(frame, 0, sizeof(*frame));
    frame->y = c->gdi_nv12;
    frame->y_pitch = c->gdi_pitch;
    frame->uv = c->gdi_nv12 + (size_t)c->gdi_pitch * (size_t)((c->height + 1) & ~1);
    frame->uv_pitch = c->gdi_pitch;
    frame->width = c->width;
    frame->height = c->height;
    frame->cursor_visible = c->cur_visible;
    frame->cursor_composited = c->cursor_in_bgra;
    frame->cursor_shape_type = OOS_CUR_NONE;
    frame->cursor_x = c->cur_x;
    frame->cursor_y = c->cur_y;
    frame->dirty_area = (int64_t)c->width * (int64_t)c->height;
    c->cursor_in_bgra = 0;
    c->have_image = 1;
}

/* oos_next for gdi_only: pace to timeout_ms (the agent's frame budget), grab,
 * and report an unchanged screen as OOS_TIMEOUT so the CPU encoder is not fed
 * the same picture 30 times a second (the agent's keepalive covers silence). */
static int gdi_only_next(oos_cap *c, uint32_t timeout_ms, oos_frame *frame, char *err, int32_t err_len)
{
    ULONGLONG now = GetTickCount64();
    size_t n = (size_t)c->width * (size_t)c->height * 4;
    int st;
    if (c->gdi_last_ms && now - c->gdi_last_ms < timeout_ms)
        Sleep((DWORD)(timeout_ms - (now - c->gdi_last_ms)));
    c->gdi_last_ms = GetTickCount64();
    st = gdi_grab(c, err, err_len);
    if (st != OOS_OK) return st;
    if (c->have_image && memcmp(c->gdi_bgra, c->gdi_prev, n) == 0) return OOS_TIMEOUT;
    memcpy(c->gdi_prev, c->gdi_bgra, n);
    gdi_bgra_to_nv12(c);
    gdi_fill_frame(c, frame);
    return OOS_OK;
}

int oos_open(int32_t output_idx, oos_cap **out, char *err, int32_t err_len)
{
    static const D3D_FEATURE_LEVEL levels[] = {
        D3D_FEATURE_LEVEL_11_1, D3D_FEATURE_LEVEL_11_0, D3D_FEATURE_LEVEL_10_1,
    };
    oos_cap *c;
    HRESULT hr = E_FAIL;
    IDXGIAdapter1 *adap = NULL;
    IDXGIOutput *o = NULL;
    ID3D10Multithread *mt = NULL;
    D3D_FEATURE_LEVEL got;

    *out = NULL;
    c = (oos_cap *)calloc(1, sizeof(oos_cap));
    if (!c) { set_err(err, err_len, "calloc", E_OUTOFMEMORY); return OOS_ERROR; }
    c->output_idx = output_idx;

    /* Test/rollback switch: force the Windows 7 GDI-only pipeline anywhere. */
    {
        char fg[4] = {0};
        if (GetEnvironmentVariableA("OO_SCREEN_FORCE_GDI", fg, sizeof fg) && fg[0] == '1') {
            if (gdi_only_open(c, err, err_len) == OOS_OK) return gdi_only_start(c, out, err, err_len);
            goto fail;
        }
    }

    hr = default_adapter(&adap);
    if (FAILED(hr)) { set_err(err, err_len, "EnumAdapters1", hr); goto fail; }

    /* An explicit adapter requires D3D_DRIVER_TYPE_UNKNOWN; passing HARDWARE
     * alongside one is E_INVALIDARG. */
    hr = D3D11CreateDevice((IDXGIAdapter *)adap, D3D_DRIVER_TYPE_UNKNOWN, NULL,
                           D3D11_CREATE_DEVICE_BGRA_SUPPORT |
                               D3D11_CREATE_DEVICE_VIDEO_SUPPORT,
                           levels, (UINT)(sizeof(levels) / sizeof(levels[0])),
                           D3D11_SDK_VERSION, &c->dev, &got, &c->ctx);
    if (FAILED(hr)) {
        /* Windows 7: VIDEO_SUPPORT / feature level 11_1 -> DXGI_ERROR_UNSUPPORTED
         * or E_INVALIDARG. No duplication there anyway: GDI-only pipeline. */
        SAFE_RELEASE(adap);
        if (gdi_only_open(c, err, err_len) == OOS_OK) return gdi_only_start(c, out, err, err_len);
        c->last_hr = hr;
        set_err(err, err_len, "D3D11CreateDevice", hr);
        goto fail;
    }

    /* VideoProcessorBlt can be issued from a different goroutine thread than
     * the one that created the device; make the immediate context safe. */
    if (SUCCEEDED(c->ctx->lpVtbl->QueryInterface(c->ctx, &OOS_IID_ID3D10Multithread,
                                                 (void **)&mt))) {
        mt->lpVtbl->SetMultithreadProtected(mt, TRUE);
        SAFE_RELEASE(mt);
    }

    hr = adap->lpVtbl->EnumOutputs(adap, (UINT)output_idx, &o);
    SAFE_RELEASE(adap);
    if (FAILED(hr)) { set_err(err, err_len, "EnumOutputs", hr); goto fail; }
    hr = o->lpVtbl->QueryInterface(o, &OOS_IID_IDXGIOutput1, (void **)&c->out1);
    SAFE_RELEASE(o);
    if (FAILED(hr)) {
        /* No IDXGIOutput1 = no Desktop Duplication (pre-Windows 8). */
        SAFE_RELEASE(c->ctx); SAFE_RELEASE(c->dev);
        if (gdi_only_open(c, err, err_len) == OOS_OK) return gdi_only_start(c, out, err, err_len);
        set_err(err, err_len, "QI IDXGIOutput1", hr); goto fail;
    }

    hr = make_dupl(c);
    if (FAILED(hr)) {
        c->last_hr = hr;
        /* E_ACCESSDENIED here is the classic "we are on a secure desktop or an
         * RDP session" case; the caller decides whether to retry. */
        set_err(err, err_len, "DuplicateOutput", hr);
        { int cl = classify(hr); oos_close(c); return cl; }
    }
    hr = make_textures(c);
    if (FAILED(hr)) { set_err(err, err_len, "CreateTexture2D", hr); goto fail; }
    hr = make_video(c);
    if (FAILED(hr)) { set_err(err, err_len, "video processor", hr); goto fail; }

    *out = c;
    return OOS_OK;

fail:
    c->last_hr = hr;
    oos_close(c);
    return OOS_ERROR;
}

/* ------------------------------------------------------------------ cursor */

static int ensure_shape(oos_cap *c, uint32_t need)
{
    if (c->shape_cap >= need) return 1;
    { uint8_t *p = (uint8_t *)realloc(c->shape, need);
      if (!p) return 0;
      c->shape = p; c->shape_cap = need; }
    return 1;
}

/* Reads the pointer shape DXGI just told us changed. */
static HRESULT fetch_shape(oos_cap *c, uint32_t size)
{
    UINT got = 0;
    HRESULT hr;
    if (!ensure_shape(c, size)) return E_OUTOFMEMORY;
    hr = c->dupl->lpVtbl->GetFramePointerShape(c->dupl, size, c->shape, &got,
                                               &c->shape_info);
    if (FAILED(hr)) return hr;
    c->have_shape = 1;
    c->shape_seq++;
    return S_OK;
}

/* Blend one 32-bit BGRA cursor pixel-row set over `dst` (a mapped region of
 * the desktop copy). All three DXGI shape types handled. */
static void blend_cursor(oos_cap *c, uint8_t *dst, int dst_pitch,
                         int rx, int ry, int rw, int rh)
{
    const DXGI_OUTDUPL_POINTER_SHAPE_INFO *si = &c->shape_info;
    int x, y;

    if (si->Type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME) {
        /* Height is 2x: AND mask on top, XOR mask below. 1bpp rows. */
        int ch = (int)si->Height / 2;
        for (y = 0; y < rh; y++) {
            int sy = ry + y;
            if (sy < 0 || sy >= ch) continue;
            for (x = 0; x < rw; x++) {
                int sx = rx + x;
                int byte, bit, andv, xorv;
                uint8_t *p;
                if (sx < 0 || sx >= (int)si->Width) continue;
                byte = sy * (int)si->Pitch + (sx / 8);
                bit = 7 - (sx % 8);
                andv = (c->shape[byte] >> bit) & 1;
                xorv = (c->shape[byte + ch * (int)si->Pitch] >> bit) & 1;
                p = dst + (size_t)y * dst_pitch + (size_t)x * 4;
                if (andv == 0) {
                    p[0] = xorv ? 0xFF : 0x00;
                    p[1] = xorv ? 0xFF : 0x00;
                    p[2] = xorv ? 0xFF : 0x00;
                } else if (xorv) {
                    p[0] = (uint8_t)(p[0] ^ 0xFF);
                    p[1] = (uint8_t)(p[1] ^ 0xFF);
                    p[2] = (uint8_t)(p[2] ^ 0xFF);
                }
                p[3] = 0xFF;
            }
        }
        return;
    }

    for (y = 0; y < rh; y++) {
        int sy = ry + y;
        if (sy < 0 || sy >= (int)si->Height) continue;
        for (x = 0; x < rw; x++) {
            int sx = rx + x;
            const uint8_t *s;
            uint8_t *p;
            if (sx < 0 || sx >= (int)si->Width) continue;
            s = c->shape + (size_t)sy * si->Pitch + (size_t)sx * 4;
            p = dst + (size_t)y * dst_pitch + (size_t)x * 4;
            if (si->Type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR) {
                /* alpha==0 -> replace, alpha==0xFF -> XOR with the desktop */
                if (s[3] == 0) {
                    p[0] = s[0]; p[1] = s[1]; p[2] = s[2];
                } else {
                    p[0] = (uint8_t)(p[0] ^ s[0]);
                    p[1] = (uint8_t)(p[1] ^ s[1]);
                    p[2] = (uint8_t)(p[2] ^ s[2]);
                }
                p[3] = 0xFF;
            } else {
                /* COLOR: straight source-over using the shape's own alpha. */
                unsigned a = s[3];
                if (a == 0) continue;
                if (a == 255) {
                    p[0] = s[0]; p[1] = s[1]; p[2] = s[2];
                } else {
                    p[0] = (uint8_t)((s[0] * a + p[0] * (255 - a)) / 255);
                    p[1] = (uint8_t)((s[1] * a + p[1] * (255 - a)) / 255);
                    p[2] = (uint8_t)((s[2] * a + p[2] * (255 - a)) / 255);
                }
                p[3] = 0xFF;
            }
        }
    }
}

static HRESULT ensure_cur_stage(oos_cap *c, int w, int h)
{
    D3D11_TEXTURE2D_DESC d;
    if (c->cur_stage && c->cur_stage_w >= w && c->cur_stage_h >= h) return S_OK;
    SAFE_RELEASE(c->cur_stage);
    memset(&d, 0, sizeof(d));
    d.Width = (UINT)w; d.Height = (UINT)h;
    d.MipLevels = 1; d.ArraySize = 1;
    d.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
    d.SampleDesc.Count = 1;
    d.Usage = D3D11_USAGE_STAGING;
    d.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
    c->cur_stage_w = w; c->cur_stage_h = h;
    return c->dev->lpVtbl->CreateTexture2D(c->dev, &d, NULL, &c->cur_stage);
}

/* Composites the cached pointer into c->bgra at the cached position.
 * Returns 1 if anything was drawn. */
static int composite_cursor(oos_cap *c)
{
    int sw, sh, x0, y0, x1, y1, rw, rh;
    D3D11_BOX box;
    D3D11_MAPPED_SUBRESOURCE m;
    HRESULT hr;

    if (!c->cur_visible || !c->have_shape) return 0;
    sw = (int)c->shape_info.Width;
    sh = (int)c->shape_info.Height;
    if (c->shape_info.Type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME) sh /= 2;
    if (sw <= 0 || sh <= 0) return 0;

    x0 = c->cur_x; y0 = c->cur_y;
    x1 = x0 + sw;  y1 = y0 + sh;
    if (x0 < 0) x0 = 0;
    if (y0 < 0) y0 = 0;
    if (x1 > c->width) x1 = c->width;
    if (y1 > c->height) y1 = c->height;
    rw = x1 - x0; rh = y1 - y0;
    if (rw <= 0 || rh <= 0) return 0;   /* pointer is off this output */

    if (FAILED(ensure_cur_stage(c, sw, sh))) return 0;

    box.left = (UINT)x0; box.top = (UINT)y0; box.front = 0;
    box.right = (UINT)x1; box.bottom = (UINT)y1; box.back = 1;
    c->ctx->lpVtbl->CopySubresourceRegion(c->ctx, (ID3D11Resource *)c->cur_stage,
                                          0, 0, 0, 0,
                                          (ID3D11Resource *)c->bgra, 0, &box);
    hr = c->ctx->lpVtbl->Map(c->ctx, (ID3D11Resource *)c->cur_stage, 0,
                             D3D11_MAP_READ, 0, &m);
    if (FAILED(hr)) return 0;
    blend_cursor(c, (uint8_t *)m.pData, (int)m.RowPitch,
                 x0 - c->cur_x, y0 - c->cur_y, rw, rh);
    /* Copy out before Unmap: UpdateSubresource cannot read a mapped resource. */
    {
        size_t need = (size_t)rw * 4 * (size_t)rh;
        uint8_t *tmp;
        int i;
        if (c->cur_tmp_cap < need) {
            uint8_t *p = (uint8_t *)realloc(c->cur_tmp, need);
            if (!p) { c->ctx->lpVtbl->Unmap(c->ctx, (ID3D11Resource *)c->cur_stage, 0); return 0; }
            c->cur_tmp = p; c->cur_tmp_cap = need;
        }
        tmp = c->cur_tmp;
        for (i = 0; i < rh; i++)
            memcpy(tmp + (size_t)i * rw * 4,
                   (uint8_t *)m.pData + (size_t)i * m.RowPitch, (size_t)rw * 4);
        c->ctx->lpVtbl->Unmap(c->ctx, (ID3D11Resource *)c->cur_stage, 0);
        c->ctx->lpVtbl->UpdateSubresource(c->ctx, (ID3D11Resource *)c->bgra, 0,
                                          &box, tmp, (UINT)(rw * 4), 0);
    }
    return 1;
}

/* ------------------------------------------------------------------- frame */

static int convert_out(oos_cap *c, oos_frame *frame, int32_t mouse_only,
                       uint32_t accumulated, char *err, int32_t err_len);

typedef struct {
    int32_t valid, dirty_count, move_count;
    int64_t dirty_area, move_area;
} oos_rects;

static int64_t clipped_area(const RECT *r, int32_t w, int32_t h)
{
    LONG l = r->left < 0 ? 0 : r->left, t = r->top < 0 ? 0 : r->top;
    LONG rr = r->right > w ? w : r->right, b = r->bottom > h ? h : r->bottom;
    if (rr <= l || b <= t) return 0;
    return (int64_t)(rr - l) * (int64_t)(b - t);
}

/* Reads move + dirty rects for the frame currently acquired (must run before
 * ReleaseFrame). On any failure the frame is reported as fully dirty, which is
 * always safe: the worst case is encoding a frame we could have skipped. */
static void read_rects(oos_cap *c, const DXGI_OUTDUPL_FRAME_INFO *fi, oos_rects *r)
{
    UINT need = fi->TotalMetadataBufferSize, used = 0, used2 = 0;
    int64_t full = (int64_t)c->width * (int64_t)c->height, sum = 0;
    HRESULT hr;
    UINT i;
    memset(r, 0, sizeof(*r));
    if (fi->LastPresentTime.QuadPart == 0) {  /* pointer-only update: no image change */
        r->valid = 1;
        return;
    }
    if (need == 0) goto unknown;
    if (c->meta_cap < need) {
        uint8_t *p = (uint8_t *)realloc(c->meta, need);
        if (!p) goto unknown;
        c->meta = p; c->meta_cap = need;
    }
    hr = c->dupl->lpVtbl->GetFrameMoveRects(c->dupl, need,
            (DXGI_OUTDUPL_MOVE_RECT *)c->meta, &used);
    if (FAILED(hr)) goto unknown;
    r->move_count = (int32_t)(used / sizeof(DXGI_OUTDUPL_MOVE_RECT));
    for (i = 0; i < (UINT)r->move_count; i++)
        sum += clipped_area(&((DXGI_OUTDUPL_MOVE_RECT *)c->meta)[i].DestinationRect,
                            c->width, c->height);
    r->move_area = sum > full ? full : sum;
    hr = c->dupl->lpVtbl->GetFrameDirtyRects(c->dupl, need - used,
            (RECT *)(c->meta + used), &used2);
    if (FAILED(hr)) goto unknown;
    r->dirty_count = (int32_t)(used2 / sizeof(RECT));
    sum = 0;
    for (i = 0; i < (UINT)r->dirty_count; i++)
        sum += clipped_area(&((RECT *)(c->meta + used))[i], c->width, c->height);
    r->dirty_area = sum > full ? full : sum;
    r->valid = 1;
    return;
unknown:
    memset(r, 0, sizeof(*r));
    r->dirty_area = full;
}

int oos_next(oos_cap *c, uint32_t timeout_ms, oos_frame *frame,
             char *err, int32_t err_len)
{
    DXGI_OUTDUPL_FRAME_INFO fi;
    IDXGIResource *res = NULL;
    ID3D11Texture2D *tex = NULL;
    HRESULT hr;
    oos_rects rects;
    int32_t prev_visible, prev_x, prev_y, noop;
    int st;

    if (!c) { set_err(err, err_len, "no capture", E_POINTER); return OOS_ERROR; }
    if (c->gdi_only) return gdi_only_next(c, timeout_ms, frame, err, err_len);
    if (!c->dupl) {
        /* A-17: resuming after oos_suspend — one DuplicateOutput on the SAME
         * device instead of a full pipeline (device+MFT) rebuild per resume. */
        hr = make_dupl(c);
        if (FAILED(hr)) { set_err(err, err_len, "DuplicateOutput(resume)", hr); return classify(hr); }
    }
    oos_release(c);

    memset(&fi, 0, sizeof(fi));
    hr = c->dupl->lpVtbl->AcquireNextFrame(c->dupl, timeout_ms, &fi, &res);
    c->last_hr = hr;
    if (hr == DXGI_ERROR_WAIT_TIMEOUT) return OOS_TIMEOUT;
    if (FAILED(hr)) {
        set_err(err, err_len, "AcquireNextFrame", hr);
        return classify(hr);
    }

    /* Gap #2: dirty/move rects are only readable while the frame is held. */
    read_rects(c, &fi, &rects);
    prev_visible = c->cur_visible; prev_x = c->cur_x; prev_y = c->cur_y;

    /* Pointer position first: it is valid even on a mouse-only update. */
    if (fi.LastMouseUpdateTime.QuadPart != 0) {
        c->cur_visible = fi.PointerPosition.Visible ? 1 : 0;
        c->cur_x = fi.PointerPosition.Position.x;
        c->cur_y = fi.PointerPosition.Position.y;
    }
    if (fi.PointerShapeBufferSize > 0) {
        HRESULT shr = fetch_shape(c, fi.PointerShapeBufferSize);
        if (FAILED(shr)) c->have_shape = 0;
    }

    /* The cursor is composited into the image, so a pointer move IS a pixel
     * change. Only a frame with zero dirty AND zero move rects and an
     * unchanged pointer (position, visibility, shape) is a real no-op. */
    noop = c->have_image && rects.valid &&
           rects.dirty_count == 0 && rects.move_count == 0 &&
           (layer_hides_pointer(c) ||   /* pointer is not in the image: ignore it */
            (fi.PointerShapeBufferSize == 0 &&
             c->cur_visible == prev_visible &&
             (!c->cur_visible || (c->cur_x == prev_x && c->cur_y == prev_y))));
    if (noop) {
        SAFE_RELEASE(res);
        c->dupl->lpVtbl->ReleaseFrame(c->dupl);
        memset(frame, 0, sizeof(*frame));
        frame->width = c->width;
        frame->height = c->height;
        frame->cursor_visible = c->cur_visible;
        frame->cursor_shape_type = c->have_shape ? (int32_t)c->shape_info.Type : OOS_CUR_NONE;
        frame->cursor_x = c->cur_x;
        frame->cursor_y = c->cur_y;
        frame->mouse_only = (fi.LastPresentTime.QuadPart == 0) ? 1 : 0;
        frame->accumulated_frames = fi.AccumulatedFrames;
        frame->rects_valid = 1;
        frame->no_change = 1;
        frame->cursor_shape_seq = c->shape_seq;
        return OOS_OK;
    }

    hr = res->lpVtbl->QueryInterface(res, &OOS_IID_ID3D11Texture2D, (void **)&tex);
    SAFE_RELEASE(res);
    if (FAILED(hr)) {
        c->dupl->lpVtbl->ReleaseFrame(c->dupl);
        set_err(err, err_len, "QI ID3D11Texture2D", hr);
        return OOS_ERROR;
    }
    /* Get onto our own texture immediately; the acquired one dies at
     * ReleaseFrame. On a mouse-only update the desktop image is unchanged, but
     * copying is still correct and keeps the code one path. */
    c->ctx->lpVtbl->CopyResource(c->ctx, (ID3D11Resource *)c->bgra,
                                 (ID3D11Resource *)tex);
    SAFE_RELEASE(tex);
    c->dupl->lpVtbl->ReleaseFrame(c->dupl);

    c->have_image = 1;

    st = convert_out(c, frame,
                     (fi.LastPresentTime.QuadPart == 0) ? 1 : 0,
                     fi.AccumulatedFrames, err, err_len);
    if (st == OOS_OK) {
        frame->rects_valid = rects.valid;
        frame->dirty_count = rects.dirty_count;
        frame->move_count = rects.move_count;
        frame->dirty_area = rects.dirty_area;
        frame->move_area = rects.move_area;
    }
    return st;
}

/* Everything after a fresh desktop image lands in c->bgra: composite the
 * pointer, convert BGRA->NV12 on the video processor, and (unless the consumer
 * takes the texture straight to the encoder) read the planes back. Shared by
 * the duplication path and the GDI fallback. */
static int convert_out(oos_cap *c, oos_frame *frame, int32_t mouse_only,
                       uint32_t accumulated, char *err, int32_t err_len)
{
    D3D11_VIDEO_PROCESSOR_STREAM stream;
    HRESULT hr;
    int composited;

    /* A-24: the pointer may already be in c->bgra (GDI path drew it). */
    if (layer_hides_pointer(c)) {
        composited = 0;          /* cursor layer: pointer travels separately */
        c->cursor_in_bgra = 0;
    } else if (c->cursor_in_bgra) {
        composited = 1;
        c->cursor_in_bgra = 0;
    } else {
        composited = composite_cursor(c);
    }

    memset(&stream, 0, sizeof(stream));
    stream.Enable = TRUE;
    stream.OutputIndex = 0;
    stream.InputFrameOrField = 0;
    stream.pInputSurface = c->vin;
    hr = c->vctx->lpVtbl->VideoProcessorBlt(c->vctx, c->vproc, c->vout, 0, 1, &stream);
    if (FAILED(hr)) {
        c->last_hr = hr;
        set_err(err, err_len, "VideoProcessorBlt", hr);
        return classify(hr);
    }

    /* CPU readback. Skipped entirely when the consumer takes c->nv12 straight
     * to the encoder (oos_set_readback(0)) — that is the zero-copy path, and
     * the absence of this Map is what Додаток C asks to be proven. */
    memset(frame, 0, sizeof(*frame));
    if (c->no_readback) {
        frame->y = NULL;
        frame->uv = NULL;
        frame->y_pitch = 0;
        frame->uv_pitch = 0;
    } else {
    c->ctx->lpVtbl->CopyResource(c->ctx, (ID3D11Resource *)c->nv12_stage,
                                 (ID3D11Resource *)c->nv12);
    hr = c->ctx->lpVtbl->Map(c->ctx, (ID3D11Resource *)c->nv12_stage, 0,
                             D3D11_MAP_READ, 0, &c->map);
    if (FAILED(hr)) {
        c->last_hr = hr;
        set_err(err, err_len, "Map nv12 staging", hr);
        return classify(hr);
    }
    c->mapped = 1;
    c->cpu_maps++;

    frame->y = (const uint8_t *)c->map.pData;
    frame->y_pitch = (int32_t)c->map.RowPitch;
    frame->uv = (const uint8_t *)c->map.pData +
                (size_t)c->map.RowPitch * (size_t)((c->height + 1) & ~1);
    frame->uv_pitch = (int32_t)c->map.RowPitch;
    }
    frame->width = c->width;
    frame->height = c->height;
    frame->cursor_visible = c->cur_visible;
    frame->cursor_composited = composited;
    frame->cursor_shape_type = c->have_shape ? (int32_t)c->shape_info.Type : OOS_CUR_NONE;
    frame->cursor_x = c->cur_x;
    frame->cursor_y = c->cur_y;
    frame->mouse_only = mouse_only;
    frame->accumulated_frames = accumulated;
    frame->cursor_shape_seq = c->shape_seq;
    return OOS_OK;
}

/* A-24: draw the live pointer into `dc`, which holds this output's image at
 * 1:1 with its top-left at desk->left/top. Also refreshes the cached pointer
 * position so the frame reports where it was drawn. Returns 1 when drawn.
 *
 * The hot spot matters: ptScreenPos is the pointer's HOT SPOT, DrawIconEx takes
 * the icon's top-left, and for an I-beam or a resize arrow the two differ by
 * half the glyph. */
static int gdi_draw_cursor(oos_cap *c, HDC dc, const RECT *desk)
{
    CURSORINFO ci;
    ICONINFO ii;
    int x, y, hx = 0, hy = 0;

    memset(&ci, 0, sizeof ci);
    ci.cbSize = sizeof ci;
    c->cur_visible = 0;
    if (!GetCursorInfo(&ci) || !(ci.flags & CURSOR_SHOWING) || !ci.hCursor)
        return 0;

    x = ci.ptScreenPos.x - desk->left;
    y = ci.ptScreenPos.y - desk->top;
    if (x < 0 || y < 0 || x >= c->width || y >= c->height)
        return 0;  /* pointer is on another monitor */

    c->cur_visible = 1;
    c->cur_x = x;
    c->cur_y = y;

    memset(&ii, 0, sizeof ii);
    if (GetIconInfo(ci.hCursor, &ii)) {
        hx = (int)ii.xHotspot;
        hy = (int)ii.yHotspot;
        /* GetIconInfo hands over two bitmap HANDLES we own; leaking them once
         * per frame would bleed GDI objects until the process hits its quota. */
        if (ii.hbmMask) DeleteObject(ii.hbmMask);
        if (ii.hbmColor) DeleteObject(ii.hbmColor);
    }
    if (!DrawIconEx(dc, x - hx, y - hy, ci.hCursor, 0, 0, 0, NULL, DI_NORMAL))
        return 0;
    c->cursor_in_bgra = 1;
    return 1;
}

int oos_gdi_next(oos_cap *c, oos_frame *frame, char *err, int32_t err_len)
{
    DXGI_OUTPUT_DESC od;
    BITMAPINFO bi;
    HDC screen = NULL, mem = NULL;
    HBITMAP bmp = NULL, old = NULL;
    void *bits = NULL;
    int st = OOS_ERROR;

    if (c && c->gdi_only) {
        int gst = gdi_grab(c, err, err_len);
        if (gst != OOS_OK) return gst;
        memcpy(c->gdi_prev, c->gdi_bgra, (size_t)c->width * (size_t)c->height * 4);
        gdi_bgra_to_nv12(c);
        gdi_fill_frame(c, frame);
        return OOS_OK;
    }
    if (!c || !c->bgra || !c->out1) {
        set_err(err, err_len, "no pipeline", E_POINTER);
        return OOS_ERROR;
    }
    oos_release(c);

    /* Source origin in DESKTOP coordinates: on a second monitor the output does
     * not start at (0,0) of the virtual screen. */
    if (FAILED(c->out1->lpVtbl->GetDesc(c->out1, &od))) {
        set_err(err, err_len, "IDXGIOutput::GetDesc", E_FAIL);
        return OOS_ERROR;
    }

    screen = GetDC(NULL);
    if (!screen) { set_err(err, err_len, "GetDC(NULL)", E_FAIL); return OOS_ERROR; }
    mem = CreateCompatibleDC(screen);
    if (!mem) { set_err(err, err_len, "CreateCompatibleDC", E_FAIL); goto done; }

    memset(&bi, 0, sizeof(bi));
    bi.bmiHeader.biSize = sizeof(BITMAPINFOHEADER);
    bi.bmiHeader.biWidth = c->width;
    bi.bmiHeader.biHeight = -c->height;  /* negative = top-down, like the texture */
    bi.bmiHeader.biPlanes = 1;
    bi.bmiHeader.biBitCount = 32;
    bi.bmiHeader.biCompression = BI_RGB;
    /* A DIB section hands us the pixels directly — no GetDIBits round trip. */
    bmp = CreateDIBSection(mem, &bi, DIB_RGB_COLORS, &bits, NULL, 0);
    if (!bmp || !bits) { set_err(err, err_len, "CreateDIBSection", E_FAIL); goto done; }
    old = (HBITMAP)SelectObject(mem, bmp);

    /* Plain BitBlt at 1:1: the DC and the DXGI rectangle are both in physical
     * pixels because the process declares per-monitor DPI awareness at start
     * (capture_windows.go init). Without that the screen DC would be
     * virtualised and a scaled display would come out cropped. */
    if (!BitBlt(mem, 0, 0, c->width, c->height, screen,
                od.DesktopCoordinates.left, od.DesktopCoordinates.top,
                SRCCOPY | CAPTUREBLT)) {
        set_err(err, err_len, "BitBlt", HRESULT_FROM_WIN32(GetLastError()));
        goto done;
    }
    /* A-24: BitBlt never draws the pointer, and on this path DXGI has given us
     * no shape to composite either — so the GDI frame used to arrive with no
     * cursor at all. That is exactly the frame a viewer stares at longest: it
     * is the FIRST frame of a session on a still desktop, and a remote desktop
     * without a mouse pointer reads as "frozen". Draw it with the same GDI we
     * already hold the DC for. */
    if (!layer_hides_pointer(c)) gdi_draw_cursor(c, mem, &od.DesktopCoordinates);
    GdiFlush();  /* the DIB bits are written by GDI asynchronously */

    c->ctx->lpVtbl->UpdateSubresource(c->ctx, (ID3D11Resource *)c->bgra, 0,
                                      NULL, bits, (UINT)(c->width * 4), 0);
    /* mouse_only=0: this is a full desktop image. */
    st = convert_out(c, frame, 0, 0, err, err_len);
    if (st == OOS_OK) {
        c->have_image = 1;
        /* Full desktop image: everything counts as changed. */
        frame->dirty_area = (int64_t)c->width * (int64_t)c->height;
    }

done:
    if (old) SelectObject(mem, old);
    if (bmp) DeleteObject(bmp);
    if (mem) DeleteDC(mem);
    ReleaseDC(NULL, screen);
    return st;
}

/* Text tiles (internal/tiles): copy the current BGRA desktop (pointer
 * included, exactly what was last converted; with the cursor layer on the
 * pointer is never composited, so tiles are cursor-free) into dst. A one-shot staging
 * texture is created, mapped once and released: this runs at most once per
 * static episode, so keeping 8-15 MB of staging memory alive is not worth it. */
int oos_read_bgra(oos_cap *c, uint8_t *dst, int32_t dst_pitch, char *err, int32_t err_len)
{
    D3D11_TEXTURE2D_DESC d;
    D3D11_MAPPED_SUBRESOURCE m;
    ID3D11Texture2D *stage = NULL;
    HRESULT hr;
    int32_t y;

    if (c && c->gdi_only && c->have_image && dst && dst_pitch >= c->width * 4) {
        for (y = 0; y < c->height; y++)
            memcpy(dst + (size_t)y * (size_t)dst_pitch, c->gdi_prev + (size_t)y * (size_t)c->width * 4, (size_t)c->width * 4);
        return OOS_OK;
    }
    if (!c || !c->bgra || !c->have_image || !dst || dst_pitch < c->width * 4) {
        set_err(err, err_len, "read_bgra: no image", E_POINTER);
        return OOS_ERROR;
    }
    memset(&d, 0, sizeof(d));
    d.Width = (UINT)c->width;
    d.Height = (UINT)c->height;
    d.MipLevels = 1;
    d.ArraySize = 1;
    d.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
    d.SampleDesc.Count = 1;
    d.Usage = D3D11_USAGE_STAGING;
    d.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
    hr = c->dev->lpVtbl->CreateTexture2D(c->dev, &d, NULL, &stage);
    if (FAILED(hr)) { c->last_hr = hr; set_err(err, err_len, "read_bgra: CreateTexture2D", hr); return classify(hr); }
    c->ctx->lpVtbl->CopyResource(c->ctx, (ID3D11Resource *)stage, (ID3D11Resource *)c->bgra);
    hr = c->ctx->lpVtbl->Map(c->ctx, (ID3D11Resource *)stage, 0, D3D11_MAP_READ, 0, &m);
    if (FAILED(hr)) {
        c->last_hr = hr;
        SAFE_RELEASE(stage);
        set_err(err, err_len, "read_bgra: Map", hr);
        return classify(hr);
    }
    for (y = 0; y < c->height; y++)
        memcpy(dst + (size_t)y * (size_t)dst_pitch,
               (const uint8_t *)m.pData + (size_t)y * m.RowPitch, (size_t)c->width * 4);
    c->ctx->lpVtbl->Unmap(c->ctx, (ID3D11Resource *)stage, 0);
    SAFE_RELEASE(stage);
    return OOS_OK;
}

void oos_release(oos_cap *c)
{
    if (c && c->mapped) {
        c->ctx->lpVtbl->Unmap(c->ctx, (ID3D11Resource *)c->nv12_stage, 0);
        c->mapped = 0;
    }
}

void oos_close(oos_cap *c)
{
    if (!c) return;
    oos_release(c);
    SAFE_RELEASE(c->vin);
    SAFE_RELEASE(c->vout);
    SAFE_RELEASE(c->vproc);
    SAFE_RELEASE(c->venum);
    SAFE_RELEASE(c->vctx);
    SAFE_RELEASE(c->vdev);
    SAFE_RELEASE(c->cur_stage);
    SAFE_RELEASE(c->nv12_stage);
    SAFE_RELEASE(c->nv12);
    SAFE_RELEASE(c->bgra);
    SAFE_RELEASE(c->dupl);
    SAFE_RELEASE(c->out1);
    SAFE_RELEASE(c->ctx);
    SAFE_RELEASE(c->dev);
    if (c->shape) free(c->shape);
    free(c->cur_tmp);
    if (c->meta) free(c->meta);
    free(c->gdi_bgra);
    free(c->gdi_prev);
    free(c->gdi_nv12);
    free(c);
}

/* Zero-copy exports: the encoder needs the same ID3D11Device (for
 * IMFDXGIDeviceManager) and the NV12 VideoProcessorBlt target itself. */
void *oos_device(oos_cap *c)       { return c ? (void *)c->dev : NULL; }

/* A-17: release ONLY the desktop duplication — the thing that conflicts with
 * MeshCentral on the same output while nobody is watching. Device, video
 * processor and staging textures stay alive, so the encoder bound to the
 * device stays valid and a resume costs one DuplicateOutput, not thousands of
 * MFShutdown/MFStartup + D3D11CreateDevice cycles a day. */
void oos_suspend(oos_cap *c)
{
    if (!c) return;
    oos_release(c);
    SAFE_RELEASE(c->dupl);
}
void *oos_nv12_texture(oos_cap *c) { return c ? (void *)c->nv12 : NULL; }
void oos_set_readback(oos_cap *c, int32_t enable) { if (c) c->no_readback = !enable; }
void oos_set_cursor_layer(oos_cap *c, int32_t enable) { if (c) c->cursor_layer = enable ? 1 : 0; }

int oos_cursor_shape(oos_cap *c, int32_t *type, int32_t *w, int32_t *h,
                     int32_t *pitch, int32_t *hot_x, int32_t *hot_y,
                     uint8_t *buf, uint32_t cap, uint32_t *len, uint32_t *seq)
{
    uint32_t need;
    if (!c || !c->have_shape || !c->shape) return OOS_ERROR;
    need = c->shape_info.Pitch * c->shape_info.Height;
    if (need > c->shape_cap) need = c->shape_cap;
    *type = (int32_t)c->shape_info.Type;
    *w = (int32_t)c->shape_info.Width;
    *h = (int32_t)c->shape_info.Height;
    *pitch = (int32_t)c->shape_info.Pitch;
    *hot_x = (int32_t)c->shape_info.HotSpot.x;
    *hot_y = (int32_t)c->shape_info.HotSpot.y;
    *seq = c->shape_seq;
    *len = need;
    if (!buf || cap < need) return OOS_INVALID_CALL;  /* *len says how much */
    memcpy(buf, c->shape, need);
    return OOS_OK;
}
int64_t oos_cpu_maps(oos_cap *c)   { return c ? c->cpu_maps : 0; }

int32_t oos_width(oos_cap *c)  { return c ? c->width : 0; }
int32_t oos_height(oos_cap *c) { return c ? c->height : 0; }
int32_t oos_last_hr(oos_cap *c) { return c ? (int32_t)c->last_hr : 0; }
