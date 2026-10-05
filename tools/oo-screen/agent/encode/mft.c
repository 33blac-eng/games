//go:build windows

/* mft.c — hardware Media Foundation H.264 encoder (Ф0, plan §5.1/§5.2/§5.5).
 *
 * Pipeline:
 *   MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER, MFT_ENUM_FLAG_HARDWARE) -> first H.264
 *     -> MF_TRANSFORM_ASYNC_UNLOCK (hardware MFTs are asynchronous)
 *     -> MF_LOW_LATENCY, ICodecAPI: AVLowLatencyMode, CBR, GOP=2s, B=0
 *     -> output type H.264 High@4.2, input type NV12
 *     -> [optional] MFT_MESSAGE_SET_D3D_MANAGER with the capture device
 *   submit: NV12 bytes -> IMFMediaBuffer copy,  OR
 *           NV12 ID3D11Texture2D -> CopyResource into a pool slot ->
 *           MFCreateDXGISurfaceBuffer  (no CPU map anywhere)
 *   poll:   METransformHaveOutput -> ProcessOutput -> Annex-B AU
 *
 * The MFT is async, so both submit and poll pump IMFMediaEventGenerator:
 * METransformNeedInput credits an input slot, METransformHaveOutput queues an AU.
 * MF_E_NOTACCEPTING can therefore only happen if the credit accounting is wrong;
 * it is handled anyway (re-queue the credit, return AGAIN).
 */

#define COBJMACROS
#define CINTERFACE
#define WIN32_LEAN_AND_MEAN
#define INITGUID

#include <windows.h>
#include <initguid.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfobjects.h>
#include <mftransform.h>
#include <mferror.h>
/* ICodecAPI is declared in strmif.h; the CI runner's mingw-w64 does not pull it in
 * transitively through mfidl.h/d3d11.h, so include it explicitly. */
#include <strmif.h>
#include <codecapi.h>
#include <d3d11.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "mft.h"

/* mingw's codecapi.h only exposes the CODECAPI_* GUIDs through __uuidof (C++).
 * The STATIC_CODECAPI_* token lists are available in C, so materialise the
 * handful we actually set. Same values, no C++ needed.
 * The extra indirection lets STATIC_CODECAPI_* expand to its 11 tokens before
 * DEFINE_GUID counts arguments. */
#define OOS_GUID_(name, ...) DEFINE_GUID(name, __VA_ARGS__)
#define OOS_GUID(name, spec) OOS_GUID_(name, spec)
OOS_GUID(OOS_AVLowLatencyMode,            STATIC_CODECAPI_AVLowLatencyMode);
OOS_GUID(OOS_AVEncCommonRateControlMode,  STATIC_CODECAPI_AVEncCommonRateControlMode);
OOS_GUID(OOS_AVEncCommonMeanBitRate,      STATIC_CODECAPI_AVEncCommonMeanBitRate);
OOS_GUID(OOS_AVEncMPVGOPSize,             STATIC_CODECAPI_AVEncMPVGOPSize);
OOS_GUID(OOS_AVEncMPVDefaultBPictureCount,STATIC_CODECAPI_AVEncMPVDefaultBPictureCount);
OOS_GUID(OOS_AVEncCommonQualityVsSpeed,   STATIC_CODECAPI_AVEncCommonQualityVsSpeed);
OOS_GUID(OOS_AVEncVideoMaxNumRefFrame,    STATIC_CODECAPI_AVEncVideoMaxNumRefFrame);
OOS_GUID(OOS_AVEncCommonMaxBitRate,       STATIC_CODECAPI_AVEncCommonMaxBitRate);
OOS_GUID(OOS_AVEncCommonBufferSize,       STATIC_CODECAPI_AVEncCommonBufferSize);
OOS_GUID(OOS_AVEncVideoForceKeyFrame,     STATIC_CODECAPI_AVEncVideoForceKeyFrame);
/* ТЗ P8: software-only knobs (Microsoft H264 Video Encoder MFT, Win8+). */
OOS_GUID(OOS_AVEncNumWorkerThreads,       STATIC_CODECAPI_AVEncNumWorkerThreads);
OOS_GUID(OOS_AVEncH264CABACEnable,        STATIC_CODECAPI_AVEncH264CABACEnable);

/* Static-screen refine (ТЗ P4). CODECAPI_AVEncVideoMaxQP comes from the
 * STATIC_ token list like the others; MFSampleExtension_VideoEncodeQP is a
 * plain DEFINE_GUID in mfapi.h (storage allocated via INITGUID). Verified
 * against mingw-w64 headers. */
OOS_GUID(OOS_AVEncVideoMaxQP,             STATIC_CODECAPI_AVEncVideoMaxQP);

/* IID_ICodecAPI: mingw declares it extern in strmif.h, but icodecapi.h (which
 * would define it) redefines struct CodecAPIEventData and cannot be included
 * alongside mfapi.h; no mingw import lib provides it either. */
DEFINE_GUID(IID_ICodecAPI,
            0x901db4c7, 0x31ce, 0x41a2, 0x85,0xdc, 0x8f,0xa0,0xbf,0x41,0xb8,0xda);

/* CLSID of the built-in "Microsoft H264 Video Encoder MFT" (wmcodecdsp.h). We
 * define it locally rather than pulling the whole wmcodecdsp.h COBJMACROS surface
 * in. INITGUID is set above, so this also allocates the storage. */
DEFINE_GUID(OOS_CLSID_CMSH264EncoderMFT,
            0x6ca50344, 0x051a, 0x4ded, 0x97,0x79, 0xa4,0x33,0x05,0x16,0x5e,0x35);

/* A-18: MFT_ENUM_ADAPTER_LUID — LUID адаптера, на якому живе апаратний MFT.
 * У заголовках mingw його немає; значення взяте з Windows SDK 10.0.22621,
 * um/mfapi.h ({1D39518C-E220-4DA8-A07F-BA172552D6B1}). */
DEFINE_GUID(OOS_MFT_ENUM_ADAPTER_LUID,
            0x1d39518c, 0xe220, 0x4da8, 0xa0,0x7f, 0xba,0x17,0x25,0x52,0xd6,0xb1);

/* MFSetAttributeSize/Ratio are C++-only inlines in mingw's mfapi.h. */
static HRESULT oos_set_pair(IMFAttributes *a, REFGUID key, UINT32 hi, UINT32 lo)
{
    return IMFAttributes_SetUINT64(a, key, ((UINT64)hi << 32) | (UINT64)lo);
}
#define MFSetAttributeSize(a, k, w, h)  oos_set_pair((a), (k), (w), (h))
#define MFSetAttributeRatio(a, k, n, d) oos_set_pair((a), (k), (n), (d))

#define SAFE_RELEASE(p) do { if (p) { IUnknown_Release((IUnknown *)(p)); (p) = NULL; } } while (0)

#define POOL_SLOTS 6
#define OUTQ_SLOTS 16

typedef struct {
    uint8_t *buf;
    int32_t  cap;
    int32_t  len;
    int32_t  keyframe;
    int64_t  pts;
} out_slot;

struct oos_enc {
    IMFTransform          *mft;
    IMFMediaEventGenerator *evgen;   /* NULL for a synchronous MFT */
    ICodecAPI             *codec;
    DWORD                  in_id, out_id;
    int32_t                async;
    int32_t                is_hardware;   /* 1 = hardware MFT, 0 = software fallback */
    int32_t                provides_samples;
    DWORD                  out_buf_size;

    int32_t 	width, height, fps, bitrate, gop;
    int32_t     level;      /* negotiated H.264 level_idc, -1 = MFT's choice */
    int32_t     profile;    /* eAVEncH264VProfile_* the MFT actually accepted */

    /* D3D11 zero-copy path */
    ID3D11Device        *dev;
    ID3D11DeviceContext *ctx;
    IMFDXGIDeviceManager *devmgr;
    UINT                  reset_token;
    ID3D11Texture2D      *pool[POOL_SLOTS];
    ID3D11VideoProcessorOutputView *pool_view[POOL_SLOTS];
    int32_t               pool_next;
    int32_t               d3d;

    /* Optional GPU scaler for src != encoded size (NV12 -> NV12). Still no CPU
     * map: VideoProcessorBlt runs on the same device as the capture pipeline. */
    int32_t               src_w, src_h;
    ID3D11VideoDevice    *vdev;
    ID3D11VideoContext   *vctx;
    ID3D11VideoProcessor *vproc;
    ID3D11VideoProcessorEnumerator *venum;
    ID3D11VideoProcessorInputView  *vin;
    ID3D11Texture2D                *vin_tex;  /* which texture vin wraps */
    uint64_t                        vin_gen;   /* A-06: its generation; the address alone is not identity */
    int32_t                         vin_valid; /* A-06: 0 until the first view is built */

    /* async credit accounting */
    int32_t need_input;

    /* finished access units */
    out_slot outq[OUTQ_SLOTS];
    int32_t  outq_head, outq_len;
    /* AUs the MFT produced that did not fit the queue. Never silently ignored:
     * the next submit fails with this count so the caller can resync (§5.5 —
     * a submitted frame is guaranteed to be delivered, so a drop is an error,
     * not a policy). */
    int32_t  outq_dropped;
    int32_t  inflight;         /* A-16: frames handed to the MFT, not yet returned */
    char     cfg_report[512];  /* A-14: ICodecAPI knobs the MFT refused */

    /* buffer handed to Go by the last poll; freed on release/close */
    uint8_t *held;

    uint8_t *seq_hdr;       /* cached SPS/PPS, Annex-B */
    int32_t  seq_hdr_len;

    char     name[256];
    int32_t  cpu_maps;      /* stays 0 on the DXGI path — the gate's evidence */
    int32_t  draining;
    int32_t  mf_held;       /* this encoder holds a ref on the MTA anchor */
    int64_t  last_pts;
    int32_t  refine_qp;     /* >0: per-frame QP for the following submits (ТЗ P4) */

    /* Timing of the last submit, split so the gate can tell the MFT's own cost
     * apart from back-pressure (waiting for METransformNeedInput). */
    int64_t  last_wait_us;
    int64_t  last_process_us;
    LARGE_INTEGER qpf;
};

/* ------------------------------------------------------- COM/MF apartment
 *
 * THREADING MODEL (fix for "MF used without CoInitializeEx").
 *
 * Go calls into this file from arbitrary goroutines, i.e. from arbitrary and
 * changing OS threads, and cgo gives no thread affinity between calls. COM
 * apartment state is strictly per-thread, so calling CoInitializeEx from
 * whichever Go thread happens to run oos_enc_open would leave every later
 * call on a thread that never initialised COM, and the matching
 * CoUninitialize/MFShutdown could never be guaranteed to run on that same
 * thread either.
 *
 * Instead we keep ONE dedicated anchor thread that calls
 * CoInitializeEx(COINIT_MULTITHREADED) + MFStartup and then parks until the
 * last encoder is closed. That makes the process MTA exist for the whole
 * lifetime of any encoder, and Windows places a thread that never called
 * CoInitializeEx into that existing MTA implicitly — so every Go thread that
 * reaches ProcessInput/ProcessOutput/GetEvent is an MTA thread, with no
 * proxying and no marshalling. This is legal for exactly the objects we use:
 * an MF_TRANSFORM_ASYNC hardware MFT is required to be free-threaded (MF's
 * own work-queue threads call it concurrently with ours), IMFMediaEventGenerator
 * and IMFDXGIDeviceManager are likewise free-threaded, and the shared
 * ID3D11Device has SetMultithreadProtected(TRUE) set below. Go-side the
 * Encoder mutex still serialises our own calls, so no LockOSThread is needed.
 *
 * The anchor is refcounted: two encoders no longer tear MF down for each
 * other, which the old per-open MFStartup / per-close MFShutdown pair did.
 */
static CRITICAL_SECTION oos_mf_lock;
static INIT_ONCE         oos_mf_once = INIT_ONCE_STATIC_INIT;
static int32_t           oos_mf_refs;
static HANDLE            oos_mf_thread, oos_mf_ready, oos_mf_quit;
static HRESULT           oos_mf_hr;

static BOOL CALLBACK oos_mf_init_lock(PINIT_ONCE o, PVOID p, PVOID *ctx)
{
    (void)o; (void)p; (void)ctx;
    InitializeCriticalSection(&oos_mf_lock);
    return TRUE;
}

static DWORD WINAPI oos_mf_anchor(LPVOID p)
{
    HRESULT hr;
    (void)p;
    hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (SUCCEEDED(hr)) {
        hr = MFStartup(MF_VERSION, MFSTARTUP_FULL);
        if (FAILED(hr)) CoUninitialize();
    }
    oos_mf_hr = hr;
    SetEvent(oos_mf_ready);
    if (FAILED(hr)) return 0;
    WaitForSingleObject(oos_mf_quit, INFINITE);
    MFShutdown();
    CoUninitialize();
    return 0;
}

/* Starts (or joins) the MTA anchor. Returns the CoInitializeEx/MFStartup HRESULT. */
static HRESULT oos_mf_acquire(void)
{
    HRESULT hr = S_OK;
    InitOnceExecuteOnce(&oos_mf_once, oos_mf_init_lock, NULL, NULL);
    EnterCriticalSection(&oos_mf_lock);
    if (oos_mf_refs == 0) {
        oos_mf_hr    = E_FAIL;
        oos_mf_ready = CreateEventW(NULL, TRUE, FALSE, NULL);
        oos_mf_quit  = CreateEventW(NULL, TRUE, FALSE, NULL);
        if (!oos_mf_ready || !oos_mf_quit) {
            hr = HRESULT_FROM_WIN32(GetLastError());
        } else {
            oos_mf_thread = CreateThread(NULL, 0, oos_mf_anchor, NULL, 0, NULL);
            if (!oos_mf_thread) {
                hr = HRESULT_FROM_WIN32(GetLastError());
            } else {
                WaitForSingleObject(oos_mf_ready, INFINITE);
                hr = oos_mf_hr;
            }
        }
        if (FAILED(hr)) {
            if (oos_mf_thread) { WaitForSingleObject(oos_mf_thread, INFINITE);
                                 CloseHandle(oos_mf_thread); oos_mf_thread = NULL; }
            if (oos_mf_ready) { CloseHandle(oos_mf_ready); oos_mf_ready = NULL; }
            if (oos_mf_quit)  { CloseHandle(oos_mf_quit);  oos_mf_quit  = NULL; }
            LeaveCriticalSection(&oos_mf_lock);
            return hr;
        }
    }
    oos_mf_refs++;
    LeaveCriticalSection(&oos_mf_lock);
    return S_OK;
}

static void oos_mf_release(void)
{
    InitOnceExecuteOnce(&oos_mf_once, oos_mf_init_lock, NULL, NULL);
    EnterCriticalSection(&oos_mf_lock);
    if (oos_mf_refs > 0 && --oos_mf_refs == 0) {
        SetEvent(oos_mf_quit);
        WaitForSingleObject(oos_mf_thread, INFINITE);
        CloseHandle(oos_mf_thread); oos_mf_thread = NULL;
        CloseHandle(oos_mf_ready);  oos_mf_ready  = NULL;
        CloseHandle(oos_mf_quit);   oos_mf_quit   = NULL;
    }
    LeaveCriticalSection(&oos_mf_lock);
}

static void set_err(char *err, int32_t n, const char *what, HRESULT hr)
{
    if (!err || n <= 0) return;
    _snprintf(err, (size_t)n - 1, "%s: hr=0x%08lX", what, (unsigned long)hr);
    err[n - 1] = 0;
}

static void set_msg(char *err, int32_t n, const char *what)
{
    if (!err || n <= 0) return;
    _snprintf(err, (size_t)n - 1, "%s", what);
    err[n - 1] = 0;
}

/* ---------------------------------------------------------------- media types */

static HRESULT set_output_type_level(oos_enc *e, UINT32 profile, int level)
{
    IMFMediaType *t = NULL;
    HRESULT hr = MFCreateMediaType(&t);
    if (FAILED(hr)) return hr;

    IMFMediaType_SetGUID(t, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(t, &MF_MT_SUBTYPE, &MFVideoFormat_H264);
    IMFMediaType_SetUINT32(t, &MF_MT_AVG_BITRATE, (UINT32)e->bitrate);
    MFSetAttributeSize((IMFAttributes *)t, &MF_MT_FRAME_SIZE,
                       (UINT32)e->width, (UINT32)e->height);
    MFSetAttributeRatio((IMFAttributes *)t, &MF_MT_FRAME_RATE,
                        (UINT32)e->fps, 1);
    MFSetAttributeRatio((IMFAttributes *)t, &MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
    IMFMediaType_SetUINT32(t, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    /* Profile/level are proposed by the caller; set_output_type() walks the
     * ladder and records what actually stuck. Level -1 means "let the MFT
     * choose": needed above 1080p, where 4.2 is not a legal level. */
    IMFMediaType_SetUINT32(t, &MF_MT_MPEG2_PROFILE, profile);
    if (level > 0)
        IMFMediaType_SetUINT32(t, &MF_MT_MPEG2_LEVEL, (UINT32)level);
    IMFMediaType_SetUINT32(t, &MF_MT_ALL_SAMPLES_INDEPENDENT, FALSE);
    /* A-15: Intel QSV honours the media-type GOP, not the CodecAPI one. */
    IMFMediaType_SetUINT32(t, &MF_MT_MAX_KEYFRAME_SPACING, (UINT32)e->gop);

    hr = IMFTransform_SetOutputType(e->mft, e->out_id, t, 0);
    SAFE_RELEASE(t);
    return hr;
}

/* Profile ladder: Main first, High only as a fallback.
 *
 * Measured, not assumed: RTCRtpReceiver.getCapabilities('video') in Chrome on a
 * shop-floor PC lists exactly eight H.264 entries -- 42001f/42e01f/4d001f/f4001f
 * (x2) -- and NOT ONE 64xx. High is not something the receiver merely ranks low;
 * it does not announce it at all, so the hub (which only forwards RTP) answers
 * 415 h264_profile_mismatch and the viewer falls back to the soundless
 * MeshCentral path. Main is in that list, and for screen content the delta
 * against High (8x8 transform, CABAC weighting) is negligible at our bitrates.
 *
 * High stays as the second rung only so that an MFT that refuses Main still
 * opens instead of taking the agent down -- and it is LOUD, not silent:
 * oos_enc_profile() feeds the open log, and the SDP fmtp is derived from the
 * encoder's own SPS, so an SDP/encoder divergence cannot happen either way.
 *
 * Level: pinned first, then the smallest that can legally hold this frame size,
 * then the MFT's own choice. e->level/e->profile record what stuck. */
static HRESULT set_output_type(oos_enc *e)
{
    static const UINT32 profiles[] = { eAVEncH264VProfile_Main, eAVEncH264VProfile_High };
    static const int ladder[] = { 42, 50, 51, 52, -1 };
    HRESULT hr = E_FAIL;
    for (size_t p = 0; p < sizeof profiles / sizeof profiles[0]; p++) {
        for (size_t i = 0; i < sizeof ladder / sizeof ladder[0]; i++) {
            hr = set_output_type_level(e, profiles[p], ladder[i]);
            if (SUCCEEDED(hr)) {
                e->profile = (int32_t)profiles[p];
                e->level   = ladder[i];
                return hr;
            }
        }
    }
    return hr;
}

static HRESULT set_input_type(oos_enc *e)
{
    /* Walk the MFT's available input types and take the NV12 one, so we inherit
     * whatever else it wants; fall back to a hand-built type. */
    HRESULT hr = E_FAIL;
    for (DWORD i = 0; ; i++) {
        IMFMediaType *t = NULL;
        GUID sub;
        HRESULT h2 = IMFTransform_GetInputAvailableType(e->mft, e->in_id, i, &t);
        if (FAILED(h2)) break;
        if (SUCCEEDED(IMFMediaType_GetGUID(t, &MF_MT_SUBTYPE, &sub)) &&
            IsEqualGUID(&sub, &MFVideoFormat_NV12)) {
            MFSetAttributeSize((IMFAttributes *)t, &MF_MT_FRAME_SIZE,
                               (UINT32)e->width, (UINT32)e->height);
            MFSetAttributeRatio((IMFAttributes *)t, &MF_MT_FRAME_RATE,
                                (UINT32)e->fps, 1);
            MFSetAttributeRatio((IMFAttributes *)t, &MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
            IMFMediaType_SetUINT32(t, &MF_MT_INTERLACE_MODE,
                                   MFVideoInterlace_Progressive);
            hr = IMFTransform_SetInputType(e->mft, e->in_id, t, 0);
            SAFE_RELEASE(t);
            if (SUCCEEDED(hr)) return hr;
            continue;
        }
        SAFE_RELEASE(t);
    }

    IMFMediaType *t = NULL;
    hr = MFCreateMediaType(&t);
    if (FAILED(hr)) return hr;
    IMFMediaType_SetGUID(t, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(t, &MF_MT_SUBTYPE, &MFVideoFormat_NV12);
    MFSetAttributeSize((IMFAttributes *)t, &MF_MT_FRAME_SIZE,
                       (UINT32)e->width, (UINT32)e->height);
    MFSetAttributeRatio((IMFAttributes *)t, &MF_MT_FRAME_RATE, (UINT32)e->fps, 1);
    MFSetAttributeRatio((IMFAttributes *)t, &MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
    IMFMediaType_SetUINT32(t, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    hr = IMFTransform_SetInputType(e->mft, e->in_id, t, 0);
    SAFE_RELEASE(t);
    return hr;
}

/* Returns the ICodecAPI HRESULT so a caller that cares (oos_enc_set_bitrate)
 * can report it; configure_codecapi deliberately ignores it — best effort. */
static HRESULT set_codec_u32(oos_enc *e, const GUID *api, ULONG v)
{
    VARIANT var;
    VariantInit(&var);
    var.vt = VT_UI4;
    var.ulVal = v;
    HRESULT hr = e->codec ? ICodecAPI_SetValue(e->codec, api, &var) : E_POINTER;
    VariantClear(&var);
    return hr;
}

static HRESULT set_codec_bool(oos_enc *e, const GUID *api, int v)
{
    VARIANT var;
    VariantInit(&var);
    var.vt = VT_BOOL;
    var.boolVal = v ? VARIANT_TRUE : VARIANT_FALSE;
    HRESULT hr = e->codec ? ICodecAPI_SetValue(e->codec, api, &var) : E_POINTER;
    VariantClear(&var);
    return hr;
}

/* A-14: every ICodecAPI_SetValue used to be fire-and-forget, so nobody knew
 * whether LowLatency/rate control/B=0 were actually in effect on a given GPU.
 * Refusals are collected here and logged once by the Go side. */
static void note_cfg(oos_enc *e, const char *name, HRESULT hr)
{
    if (SUCCEEDED(hr)) return;
    size_t n = strlen(e->cfg_report);
    if (n + 1 >= sizeof e->cfg_report) return;
    _snprintf(e->cfg_report + n, sizeof e->cfg_report - n - 1,
              "%s%s=0x%08lX", n ? " " : "", name, (unsigned long)hr);
    e->cfg_report[sizeof e->cfg_report - 1] = 0;
}

/* Low-latency CBR, GOP = 2s, no B-frames (plan §5.1). Best effort: an MFT that
 * refuses one knob still encodes, and the gate measures the result. */
/* A-20/A-25: peak-constrained VBR instead of CBR. Under CBR a still desktop
 * still burns the whole budget (padding + noise re-encodes), and the IDR that
 * follows the first real change has no headroom -> mushy text. PCVBR spends
 * nothing on stillness and lets an IDR borrow up to MaxBitRate; BufferSize
 * (HRD) at half a second keeps that burst from becoming a latency spike. */
static ULONG peak_bps(int32_t mean)   { return (ULONG)(mean / 2 * 3); }
static ULONG hrd_bits(int32_t mean)   { return (ULONG)(mean / 2); }

static void configure_codecapi(oos_enc *e)
{
    e->cfg_report[0] = 0;
    note_cfg(e, "LowLatency",     set_codec_bool(e, &OOS_AVLowLatencyMode, 1));
    note_cfg(e, "RateControl",    set_codec_u32(e, &OOS_AVEncCommonRateControlMode,
                                                eAVEncCommonRateControlMode_PeakConstrainedVBR));
    note_cfg(e, "MeanBitRate",    set_codec_u32(e, &OOS_AVEncCommonMeanBitRate, (ULONG)e->bitrate));
    note_cfg(e, "MaxBitRate",     set_codec_u32(e, &OOS_AVEncCommonMaxBitRate, peak_bps(e->bitrate)));
    note_cfg(e, "BufferSize",     set_codec_u32(e, &OOS_AVEncCommonBufferSize, hrd_bits(e->bitrate)));
    note_cfg(e, "GOPSize",        set_codec_u32(e, &OOS_AVEncMPVGOPSize, (ULONG)e->gop));
    note_cfg(e, "BPictureCount",  set_codec_u32(e, &OOS_AVEncMPVDefaultBPictureCount, 0));
    /* A-25. 50 — це «однаково важливі якість і швидкість», тобто дефолт для
     * ВІДЕО. Наш вміст — текст робочого столу, де ціна помилки квантування
     * читається очима, а не міряється в кадрах; план просив 66-100.
     *
     * 80, а не 100: заміряно на цьому ж боксі (NVIDIA H.264 Encoder MFT,
     * 1920x1080, 60 к/с, PCVBR 8 Мбіт/с, encode-probe із рухом) — ProcessInput
     * p99 лишається на порядок нижчим за бюджет 8 мс, тобто запас є, а 100
     * віддавав би його весь заради різниці, якої на тексті вже не видно. */
    note_cfg(e, "QualityVsSpeed", set_codec_u32(e, &OOS_AVEncCommonQualityVsSpeed, 80));
    /* Один reference-кадр — найгірше саме для робочого столу: вікно, що
     * перекрило текст і поїхало далі, доводиться кодувати з нуля, бо кадру, де
     * той текст ще був, енкодер уже не памʼятає. Два дають йому цю памʼять і на
     * нашому вмісті радше ЗМЕНШУЮТЬ потік. Більше двох не беремо: без B-кадрів
     * і при GOP=2с виграш згасає, а пошук дорожчає. */
    note_cfg(e, "MaxNumRefFrame", set_codec_u32(e, &OOS_AVEncVideoMaxNumRefFrame, 2));

    if (!e->is_hardware) {
        /* ТЗ P8. Софтверний Microsoft H264 MFT без підказки бере ВСІ логічні
         * ядра: на офісному 4-ядерному ПК кожен кадр на мить забирає весь CPU
         * у застосунку, з яким людина працює (Excel/1С), і з нашим же
         * захопленням/readback. Лишаємо одне ядро вільним від 4 ядер; на 1-3
         * ядрах різати нема з чого — там бюджет тримає FPS-політика
         * (internal/swlimit). */
        SYSTEM_INFO si;
        GetSystemInfo(&si);
        ULONG n = (ULONG)si.dwNumberOfProcessors;
        if (n >= 4) {
            note_cfg(e, "NumWorkerThreads",
                     set_codec_u32(e, &OOS_AVEncNumWorkerThreads, n - 1));
        }
        /* Main-профіль дозволяє CABAC; на тексті він дає помітно менший потік
         * за той самий QP, тобто чіткіший текст у тому ж бюджеті. Явно, бо
         * дефолт софт-MFT документацією не зафіксований. */
        note_cfg(e, "CABAC", set_codec_bool(e, &OOS_AVEncH264CABACEnable, 1));
    }
}

const char *oos_enc_cfg_report(oos_enc *e) { return (e && e->cfg_report[0]) ? e->cfg_report : ""; }

/* Cache the SPS/PPS the MFT publishes on the negotiated output type. Used to
 * prefix IDR AUs when the MFT does not repeat headers inband (plan §5.2). */
static void cache_seq_header(oos_enc *e)
{
    IMFMediaType *t = NULL;
    if (FAILED(IMFTransform_GetOutputCurrentType(e->mft, e->out_id, &t))) return;
    /* The cache belongs to the CURRENT output type. After a renegotiation the
     * old SPS/PPS describes a stream that no longer exists, so drop it first
     * and only re-populate from the type we just read. */
    free(e->seq_hdr);
    e->seq_hdr = NULL;
    e->seq_hdr_len = 0;
    UINT32 n = 0;
    if (SUCCEEDED(IMFAttributes_GetBlobSize((IMFAttributes *)t,
                                            &MF_MT_MPEG_SEQUENCE_HEADER, &n)) && n > 0) {
        uint8_t *b = (uint8_t *)malloc(n);
        if (b && SUCCEEDED(IMFAttributes_GetBlob((IMFAttributes *)t,
                                                 &MF_MT_MPEG_SEQUENCE_HEADER,
                                                 b, n, &n))) {
            e->seq_hdr = b;
            e->seq_hdr_len = (int32_t)n;
        } else {
            free(b);
        }
    }
    SAFE_RELEASE(t);
}

/* ------------------------------------------------------------------- D3D pool */

static HRESULT make_pool(oos_enc *e)
{
    D3D11_TEXTURE2D_DESC d;
    memset(&d, 0, sizeof d);
    d.Width  = (UINT)((e->width + 1) & ~1);
    d.Height = (UINT)((e->height + 1) & ~1);
    d.MipLevels = 1;
    d.ArraySize = 1;
    d.Format = DXGI_FORMAT_NV12;
    d.SampleDesc.Count = 1;
    d.Usage = D3D11_USAGE_DEFAULT;
    d.BindFlags = D3D11_BIND_RENDER_TARGET | D3D11_BIND_SHADER_RESOURCE;

    for (int i = 0; i < POOL_SLOTS; i++) {
        HRESULT hr = ID3D11Device_CreateTexture2D(e->dev, &d, NULL, &e->pool[i]);
        if (FAILED(hr)) {
            /* Some drivers reject RT|SRV on NV12; retry with no bind flags. */
            d.BindFlags = 0;
            hr = ID3D11Device_CreateTexture2D(e->dev, &d, NULL, &e->pool[i]);
            if (FAILED(hr)) return hr;
        }
    }
    return S_OK;
}

/* Builds the NV12->NV12 rescaler used when the desktop is larger than the
 * encoded frame (this GPU's NVENC refuses anything above 1080p). */
static HRESULT make_scaler(oos_enc *e)
{
    HRESULT hr;
    D3D11_VIDEO_PROCESSOR_CONTENT_DESC cd;
    memset(&cd, 0, sizeof cd);
    cd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
    cd.InputWidth  = (UINT)e->src_w;
    cd.InputHeight = (UINT)e->src_h;
    cd.OutputWidth  = (UINT)e->width;
    cd.OutputHeight = (UINT)e->height;
    /* ТЗ P2/1.2: text on a downscaled desktop — ask the driver for its best
     * scaler rather than the playback default (the only quality knob D3D11
     * exposes for the resampling filter itself). */
    cd.Usage = D3D11_VIDEO_USAGE_OPTIMAL_QUALITY;

    hr = ID3D11Device_QueryInterface(e->dev, &IID_ID3D11VideoDevice, (void **)&e->vdev);
    if (FAILED(hr)) return hr;
    hr = ID3D11DeviceContext_QueryInterface(e->ctx, &IID_ID3D11VideoContext,
                                            (void **)&e->vctx);
    if (FAILED(hr)) return hr;
    hr = ID3D11VideoDevice_CreateVideoProcessorEnumerator(e->vdev, &cd, &e->venum);
    if (FAILED(hr)) return hr;
    hr = ID3D11VideoDevice_CreateVideoProcessor(e->vdev, e->venum, 0, &e->vproc);
    if (FAILED(hr)) return hr;

    for (int i = 0; i < POOL_SLOTS; i++) {
        D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd;
        memset(&ovd, 0, sizeof ovd);
        ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
        hr = ID3D11VideoDevice_CreateVideoProcessorOutputView(e->vdev,
                (ID3D11Resource *)e->pool[i], e->venum, &ovd, &e->pool_view[i]);
        if (FAILED(hr)) return hr;
    }
    ID3D11VideoContext_VideoProcessorSetStreamFrameFormat(e->vctx, e->vproc, 0,
            D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE);

    /* ТЗ P2: no driver "enhancements" on desktop text. Auto-processing
     * (denoise/edge/skin-tone/etc. the driver may enable on its own) off, and
     * every filter the processor advertises explicitly disabled. Void calls:
     * nothing to check; an unsupported filter is skipped via FilterCaps. */
    ID3D11VideoContext_VideoProcessorSetStreamAutoProcessingMode(e->vctx, e->vproc, 0, FALSE);
    {
        D3D11_VIDEO_PROCESSOR_CAPS caps;
        memset(&caps, 0, sizeof caps);
        if (SUCCEEDED(ID3D11VideoProcessorEnumerator_GetVideoProcessorCaps(e->venum, &caps))) {
            for (int f = D3D11_VIDEO_PROCESSOR_FILTER_BRIGHTNESS;
                 f <= D3D11_VIDEO_PROCESSOR_FILTER_STEREO_ADJUSTMENT; f++) {
                if (caps.FilterCaps & (1u << f))
                    ID3D11VideoContext_VideoProcessorSetStreamFilter(e->vctx, e->vproc, 0,
                            (D3D11_VIDEO_PROCESSOR_FILTER)f, FALSE, 0);
            }
        }
    }

    /* Colour: capture (agent/capture/dxgi.c) already emits studio-range BT.709
     * NV12, and the SPS VUI signals the same. Declare BT.709 limited on BOTH
     * sides so the scaler is a pure resample — with the defaults (stream
     * BT.601) the driver would re-matrix 709->601 and shift colours. */
    {
        D3D11_VIDEO_PROCESSOR_COLOR_SPACE cs;
        memset(&cs, 0, sizeof cs);
        cs.Usage = 0;          /* playback */
        cs.YCbCr_Matrix = 1;   /* BT.709 */
        cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
        ID3D11VideoContext_VideoProcessorSetStreamColorSpace(e->vctx, e->vproc, 0, &cs);
        ID3D11VideoContext_VideoProcessorSetOutputColorSpace(e->vctx, e->vproc, &cs);
    }
    return S_OK;
}

/* -------------------------------------------------------------- output queue */

static out_slot *outq_push(oos_enc *e)
{
    if (e->outq_len >= OUTQ_SLOTS) return NULL;
    int idx = (e->outq_head + e->outq_len) % OUTQ_SLOTS;
    e->outq_len++;
    return &e->outq[idx];
}

static out_slot *outq_pop(oos_enc *e)
{
    if (e->outq_len == 0) return NULL;
    out_slot *s = &e->outq[e->outq_head];
    e->outq_head = (e->outq_head + 1) % OUTQ_SLOTS;
    e->outq_len--;
    return s;
}

/* Pulls one IMFSample out of the MFT and appends it to the queue. */
static HRESULT drain_one_output(oos_enc *e, int *got)
{
    MFT_OUTPUT_DATA_BUFFER odb;
    IMFSample *sample = NULL;
    DWORD status = 0;
    HRESULT hr;

    *got = 0;
    memset(&odb, 0, sizeof odb);
    odb.dwStreamID = e->out_id;

    if (!e->provides_samples) {
        IMFMediaBuffer *mb = NULL;
        hr = MFCreateSample(&sample);
        if (FAILED(hr)) return hr;
        hr = MFCreateMemoryBuffer(e->out_buf_size ? e->out_buf_size
                                                  : (DWORD)(e->width * e->height * 2), &mb);
        if (FAILED(hr)) { SAFE_RELEASE(sample); return hr; }
        IMFSample_AddBuffer(sample, mb);
        SAFE_RELEASE(mb);
        odb.pSample = sample;
    }

    hr = IMFTransform_ProcessOutput(e->mft, 0, 1, &odb, &status);
    if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) {
        SAFE_RELEASE(sample);
        return S_OK;
    }
    if (hr == MF_E_TRANSFORM_STREAM_CHANGE) {
        /* Renegotiate the output type and try again next round. A failure here
         * used to be swallowed, which left the MFT with no valid output type
         * and the cached SPS/PPS describing the previous stream. */
        IMFMediaType *nt = NULL;
        HRESULT rhr = IMFTransform_GetOutputAvailableType(e->mft, e->out_id, 0, &nt);
        if (SUCCEEDED(rhr)) {
            rhr = IMFTransform_SetOutputType(e->mft, e->out_id, nt, 0);
            SAFE_RELEASE(nt);
        }
        SAFE_RELEASE(sample);
        if (FAILED(rhr)) return rhr;

        /* The new type carries its own SPS/PPS and its own buffer geometry. */
        cache_seq_header(e);
        {
            MFT_OUTPUT_STREAM_INFO si;
            memset(&si, 0, sizeof si);
            if (SUCCEEDED(IMFTransform_GetOutputStreamInfo(e->mft, e->out_id, &si))) {
                e->out_buf_size = si.cbSize;
                e->provides_samples = (si.dwFlags &
                    (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES |
                     MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES)) ? 1 : 0;
            }
        }
        return S_OK;
    }
    if (FAILED(hr)) { SAFE_RELEASE(sample); return hr; }

    IMFSample *res = odb.pSample;
    SAFE_RELEASE(odb.pEvents);
    if (!res) { SAFE_RELEASE(sample); return S_OK; }

    IMFMediaBuffer *mb = NULL;
    if (SUCCEEDED(IMFSample_ConvertToContiguousBuffer(res, &mb))) {
        BYTE *p = NULL; DWORD maxlen = 0, curlen = 0;
        if (SUCCEEDED(IMFMediaBuffer_Lock(mb, &p, &maxlen, &curlen))) {
            out_slot *s = outq_push(e);
            if (!s) {
                /* §5.5: a submitted frame is guaranteed to be delivered. We
                 * cannot block here (the AU is already out of the MFT and the
                 * caller is not polling), so record the drop — the next submit
                 * fails with it instead of silently breaking the reference
                 * chain. */
                e->outq_dropped++;
            } else {
                if (s->cap < (int32_t)curlen) {
                    free(s->buf);
                    s->buf = (uint8_t *)malloc(curlen ? curlen : 1);
                    s->cap = s->buf ? (int32_t)curlen : 0;
                }
                if (s->buf) {
                    memcpy(s->buf, p, curlen);
                    s->len = (int32_t)curlen;
                    LONGLONG ts = 0;
                    s->pts = SUCCEEDED(IMFSample_GetSampleTime(res, &ts))
                             ? (int64_t)ts : e->last_pts;
                    UINT32 cp = 0;
                    IMFAttributes_GetUINT32((IMFAttributes *)res,
                                            &MFSampleExtension_CleanPoint, &cp);
                    s->keyframe = cp ? 1 : 0;
                    *got = 1;
                    if (e->inflight > 0) e->inflight--;
                } else {
                    e->outq_len--; /* undo the push we cannot fill */
                }
            }
            IMFMediaBuffer_Unlock(mb);
        }
        SAFE_RELEASE(mb);
    }

    if (res != sample) SAFE_RELEASE(res);
    SAFE_RELEASE(sample);
    return S_OK;
}

/* -------------------------------------------------------------- event pumping */

/* Handles one MFT event. wait!=0 blocks in GetEvent. Returns:
 *   1  an event was handled
 *   0  no event pending (non-blocking only)
 *  -1  failure (err filled)
 *  -2  wait timed out: the transform is wedged (err filled; blocking only) */
/* A-12: an upper bound on how long a blocking pump may wait for the MFT.
 * A wedged transform (driver reset, device removed mid-encode, NVENC session
 * lost) never raises another event; GetEvent(0) then parked the frame loop
 * forever under Encoder.mu and the agent stayed "connected" while streaming
 * nothing. 500 ms is ~30 frames at 60 fps: far beyond any healthy credit wait
 * (measured sub-millisecond), far below what a human reads as a hang. */
#define OOS_EVENT_WAIT_US 500000

static int64_t now_us(oos_enc *e);

static int pump_event(oos_enc *e, int wait, char *err, int32_t err_len)
{
    IMFMediaEvent *ev = NULL;
    HRESULT hr;
    if (!wait) {
        hr = IMFMediaEventGenerator_GetEvent(e->evgen, MF_EVENT_FLAG_NO_WAIT, &ev);
    } else {
        /* ponytail: poll + SwitchToThread instead of an IMFAsyncCallback
         * vtable; the wait is sub-ms in the healthy case, so spinning is
         * cheaper than the plumbing. */
        int64_t t0 = now_us(e);
        for (;;) {
            hr = IMFMediaEventGenerator_GetEvent(e->evgen, MF_EVENT_FLAG_NO_WAIT, &ev);
            if (hr != MF_E_NO_EVENTS_AVAILABLE) break;
            if (now_us(e) - t0 > OOS_EVENT_WAIT_US) {
                set_msg(err, err_len, "MFT event wait timeout: encoder wedged, rebuild required");
                return -2; /* the caller reports OOS_ENC_WEDGED, not a plain error */
            }
            SwitchToThread();
        }
    }
    if (hr == MF_E_NO_EVENTS_AVAILABLE) return 0;
    if (FAILED(hr)) { set_err(err, err_len, "GetEvent", hr); return -1; }

    MediaEventType type = 0;
    HRESULT status = S_OK;
    IMFMediaEvent_GetType(ev, &type);
    /* A failing event status (and MEError in particular) is the MFT telling us
     * the stream is dead. Ignoring it meant every later wait for
     * METransformNeedInput blocked forever, since no further event ever came. */
    if (FAILED(IMFMediaEvent_GetStatus(ev, &status))) status = S_OK;
    SAFE_RELEASE(ev);

    if (type == MEError || FAILED(status)) {
        set_err(err, err_len,
                type == MEError ? "MEError from MFT" : "MFT event status",
                FAILED(status) ? status : E_FAIL);
        return -1;
    }

    switch (type) {
    case METransformNeedInput:
        e->need_input++;
        return 1;
    case METransformHaveOutput: {
        int got = 0;
        hr = drain_one_output(e, &got);
        if (FAILED(hr)) { set_err(err, err_len, "ProcessOutput", hr); return -1; }
        return 1;
    }
    case METransformDrainComplete:
        e->draining = 0;
        return 1;
    default:
        return 1;
    }
}

/* --------------------------------------------------------------------- submit */

static int64_t now_us(oos_enc *e)
{
    LARGE_INTEGER t;
    QueryPerformanceCounter(&t);
    return (int64_t)(t.QuadPart * 1000000 / e->qpf.QuadPart);
}

/* A-20: SampleDuration used to be a flat 1/fps. On a still desktop the
 * frames are keepalives a second apart; telling the rate control they were
 * 16 ms apart made it spend ~8 % of the budget on nothing. */
static LONGLONG sample_dur(oos_enc *e, int64_t pts_100ns)
{
    LONGLONG d = (e->last_pts > 0 && pts_100ns > e->last_pts)
                 ? (LONGLONG)(pts_100ns - e->last_pts)
                 : (LONGLONG)(10000000LL / e->fps);
    if (d > 10000000LL) d = 10000000LL;
    return d;
}

/* Refine frame: ask for a fixed QP on this sample. Best effort — an MFT that
 * ignores the attribute still has MaxQP clamped by oos_enc_set_refine_qp. */
static void apply_refine_qp(oos_enc *e, IMFSample *sample)
{
    if (e->refine_qp > 0)
        IMFSample_SetUINT64(sample,
                            &MFSampleExtension_VideoEncodeQP,
                            (UINT64)e->refine_qp);
}

static int submit_sample(oos_enc *e, IMFSample *sample, char *err, int32_t err_len)
{
    HRESULT hr;
    int64_t t0 = now_us(e), t1;

    if (e->outq_dropped) {
        /* Report once, then force an IDR so a caller that recovers from the
         * error gets a self-contained restart of the reference chain. */
        char m[128];
        _snprintf(m, sizeof m - 1,
                  "output queue overflow: %d access unit(s) dropped "
                  "(poll faster); reference chain broken",
                  (int)e->outq_dropped);
        m[sizeof m - 1] = 0;
        e->outq_dropped = 0;
        set_msg(err, err_len, m);
        oos_enc_force_idr(e, NULL, 0);
        return OOS_ENC_AGAIN; /* A-09: not fatal — caller drains and resubmits */
    }

    if (e->async) {
        /* Wait for an input credit; every event pumped on the way may also
         * produce output, which lands in the queue. */
        while (e->need_input == 0) {
            int r = pump_event(e, 1, err, err_len);
            if (r == -2) return OOS_ENC_WEDGED;
            if (r < 0) return OOS_ENC_ERROR;
        }
        e->need_input--;
    }
    t1 = now_us(e);
    e->last_wait_us = t1 - t0;

    hr = IMFTransform_ProcessInput(e->mft, e->in_id, sample, 0);
    e->last_process_us = now_us(e) - t1;
    if (SUCCEEDED(hr)) e->inflight++;
    if (hr == MF_E_NOTACCEPTING) {
        /* Give the credit back and let the caller drain output first. */
        if (e->async) e->need_input++;
        for (;;) {
            int got = 0;
            if (FAILED(drain_one_output(e, &got)) || !got) break;
        }
        return OOS_ENC_AGAIN;
    }
    if (FAILED(hr)) { set_err(err, err_len, "ProcessInput", hr); return OOS_ENC_ERROR; }

    if (!e->async) {
        /* Synchronous MFT: outputs are only available right after input. */
        for (;;) {
            int got = 0;
            if (FAILED(drain_one_output(e, &got)) || !got) break;
        }
    } else {
        /* Non-blocking sweep so queued HaveOutput events are picked up early. */
        for (;;) {
            int r = pump_event(e, 0, err, err_len);
            if (r <= 0) break;
        }
    }
    return OOS_ENC_OK;
}

/* A-18: LUID адаптера, на якому живе девайс капчера
 * (ID3D11Device -> IDXGIDevice -> GetAdapter -> GetDesc -> AdapterLuid).
 * 0 = питати нема в кого (девайс не передали) або драйвер не відповів; тоді
 * вибір MFT лишається старим «беремо перший». */
static int oos_adapter_luid(ID3D11Device *dev, LUID *out)
{
    IDXGIDevice      *dxgi = NULL;
    IDXGIAdapter     *ad   = NULL;
    DXGI_ADAPTER_DESC desc;
    int ok = 0;

    if (!dev || !out) return 0;
    if (SUCCEEDED(ID3D11Device_QueryInterface(dev, &IID_IDXGIDevice,
                                              (void **)&dxgi)) && dxgi) {
        if (SUCCEEDED(IDXGIDevice_GetAdapter(dxgi, &ad)) && ad) {
            if (SUCCEEDED(IDXGIAdapter_GetDesc(ad, &desc))) {
                *out = desc.AdapterLuid;
                ok = 1;
            }
            SAFE_RELEASE(ad);
        }
        SAFE_RELEASE(dxgi);
    }
    return ok;
}

/* oos_pick_by_luid — індекс апаратного MFT, що сидить на адаптері want.
 *
 *   >=0  цей MFT наш;
 *    -1  хоч один MFT назвав свій LUID, і жоден не збігся — апаратного шляху
 *        для НАШОГО захоплення нема, кличучий іде в софт;
 *     0  про LUID не сказав НІХТО (старий драйвер, Win до 1703) — лишаємо стару
 *        поведінку «беремо перший», з якою агент їздить на всьому флоті. Тихо
 *        зламати ці машини заради гібридних ноутбуків ми не маємо права. */
static int oos_pick_by_luid(IMFActivate **acts, UINT32 count, LUID want)
{
    int saw_luid = 0;

    for (UINT32 i = 0; i < count; i++) {
        LUID   got;
        UINT32 blen = 0;
        if (FAILED(IMFActivate_GetBlob(acts[i], &OOS_MFT_ENUM_ADAPTER_LUID,
                                       (UINT8 *)&got, sizeof got, &blen)) ||
            blen != sizeof got) {
            continue;
        }
        saw_luid = 1;
        if (got.LowPart == want.LowPart && got.HighPart == want.HighPart) {
            return (int)i;
        }
    }
    return saw_luid ? -1 : 0;
}

/* --------------------------------------------------------------------- public */

int oos_enc_open(const oos_enc_cfg *cfg, oos_enc **out, char *err, int32_t err_len)
{
    HRESULT hr;
    IMFActivate **acts = NULL;
    UINT32 count = 0;
    oos_enc *e = NULL;

    if (!cfg || !out) return OOS_ENC_ERROR;
    *out = NULL;

    /* CoInitializeEx(MTA) + MFStartup, on a dedicated anchor thread that owns
     * the matching CoUninitialize/MFShutdown. See "COM/MF apartment" above. */
    hr = oos_mf_acquire();
    if (FAILED(hr)) { set_err(err, err_len, "CoInitializeEx/MFStartup", hr);
                      return OOS_ENC_ERROR; }

    /* Enumeration, two-tier (plan §5.1 + software fallback for boxes with no
     * hardware encoder):
     *   1. MFTEnumEx(HARDWARE) — NVENC/QuickSync/AMF, asynchronous, zero-copy.
     *   2. On miss (or force_software), MFTEnumEx WITHOUT the HARDWARE flag
     *      (SYNC|ASYNC|LOCAL|TRANSCODE_ONLY) and take the built-in Microsoft
     *      H264 Video Encoder MFT (CLSID_CMSH264EncoderMFT): synchronous, CPU
     *      NV12 only, no D3D. */
    int32_t is_hw = 0;
    UINT32  pick = 0;
    int32_t luid_miss = 0;   /* A-18: hw MFT є, але всі на чужому адаптері */
    MFT_REGISTER_TYPE_INFO outinfo = { MFMediaType_Video, MFVideoFormat_H264 };

    if (!cfg->force_software) {
        hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                       MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
                       NULL, &outinfo, &acts, &count);
        if (SUCCEEDED(hr) && count > 0) {
            /* A-18: брати acts[0] наосліп можна лише на машині з одним GPU. На
             * гібридному ноутбуку робочий стіл (а з ним і наш капчер) малює
             * iGPU, тоді як першим у списку стоїть dGPU — і SET_D3D_MANAGER з
             * чужим девайсом падає, лишаючи ПК без картинки взагалі. Тому MFT
             * шукаємо за LUID адаптера, з якого ЙДЕ ЗАХОПЛЕННЯ. */
            LUID want;
            int  sel = 0;
            if (oos_adapter_luid((ID3D11Device *)(void *)cfg->d3d_device, &want)) {
                sel = oos_pick_by_luid(acts, count, want);
            }
            if (sel < 0) {
                /* Апаратні MFT є, LUID вони називають, і жоден із них не наш.
                 * Апаратного шляху для цього захоплення не існує — чесніше
                 * піти в софт нижче, ніж падати на SET_D3D_MANAGER. */
                for (UINT32 i = 0; i < count; i++) SAFE_RELEASE(acts[i]);
                CoTaskMemFree(acts); acts = NULL;
                count = 0;
                luid_miss = 1;
            } else {
                pick = (UINT32)sel;
                is_hw = 1;
            }
        } else {
            if (acts) { CoTaskMemFree(acts); acts = NULL; }
            count = 0;
        }
    }

    if (count == 0) {
        hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                       MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_ASYNCMFT |
                       MFT_ENUM_FLAG_LOCALMFT | MFT_ENUM_FLAG_TRANSCODE_ONLY |
                       MFT_ENUM_FLAG_SORTANDFILTER,
                       NULL, &outinfo, &acts, &count);
        if (FAILED(hr) || count == 0) {
            if (acts) CoTaskMemFree(acts);
            set_msg(err, err_len, cfg->force_software
                    ? "no software H.264 MFT registered (MFTEnumEx SOFTWARE)"
                    : "no H.264 MFT registered (neither hardware nor software)");
            oos_mf_release();
            return OOS_ENC_NOHW;
        }
        is_hw = 0;
        /* Prefer the Microsoft H264 Video Encoder MFT specifically. */
        for (UINT32 i = 0; i < count; i++) {
            GUID clsid;
            if (SUCCEEDED(IMFActivate_GetGUID(acts[i], &MFT_TRANSFORM_CLSID_Attribute,
                                              &clsid)) &&
                IsEqualGUID(&clsid, &OOS_CLSID_CMSH264EncoderMFT)) {
                pick = i;
                break;
            }
        }
    }

    e = (oos_enc *)calloc(1, sizeof *e);
    if (!e) { CoTaskMemFree(acts); oos_mf_release(); return OOS_ENC_ERROR; }
    e->mf_held = 1;   /* oos_enc_close now owns the anchor ref */
    e->is_hardware = is_hw;
    e->width   = cfg->width;
    e->height  = cfg->height;
    e->fps     = cfg->fps > 0 ? cfg->fps : 30;
    e->bitrate = cfg->bitrate_bps > 0 ? cfg->bitrate_bps : 8000000;
    e->gop     = cfg->gop > 0 ? cfg->gop : e->fps * 2;
    e->src_w   = cfg->src_width  > 0 ? cfg->src_width  : e->width;
    e->src_h   = cfg->src_height > 0 ? cfg->src_height : e->height;
    e->in_id = 0; e->out_id = 0;
    QueryPerformanceFrequency(&e->qpf);

    {   /* friendly name of the MFT we picked, for the gate report */
        WCHAR *w = NULL; UINT32 wl = 0;
        if (SUCCEEDED(IMFActivate_GetAllocatedString(acts[pick],
                &MFT_FRIENDLY_NAME_Attribute, &w, &wl)) && w) {
            WideCharToMultiByte(CP_UTF8, 0, w, -1, e->name, sizeof e->name - 1,
                                NULL, NULL);
            CoTaskMemFree(w);
        }
    }

    hr = IMFActivate_ActivateObject(acts[pick], &IID_IMFTransform, (void **)&e->mft);
    for (UINT32 i = 0; i < count; i++) SAFE_RELEASE(acts[i]);
    CoTaskMemFree(acts);
    if (FAILED(hr)) {
        set_err(err, err_len, "ActivateObject", hr);
        oos_enc_close(e);
        return OOS_ENC_ERROR;
    }

    /* Attributes: async unlock + low latency. */
    IMFAttributes *attrs = NULL;
    if (SUCCEEDED(IMFTransform_GetAttributes(e->mft, &attrs)) && attrs) {
        UINT32 v = 0;
        if (SUCCEEDED(IMFAttributes_GetUINT32(attrs, &MF_TRANSFORM_ASYNC, &v)) && v) {
            e->async = 1;
            IMFAttributes_SetUINT32(attrs, &MF_TRANSFORM_ASYNC_UNLOCK, TRUE);
        }
        IMFAttributes_SetUINT32(attrs, &MF_LOW_LATENCY, TRUE);
        SAFE_RELEASE(attrs);
    }

    /* Actual stream ids, if the MFT does not use 0/0. */
    {
        DWORD nin = 0, nout = 0;
        if (SUCCEEDED(IMFTransform_GetStreamCount(e->mft, &nin, &nout)) &&
            nin >= 1 && nout >= 1) {
            DWORD *ii = (DWORD *)calloc(nin, sizeof(DWORD));
            DWORD *oo = (DWORD *)calloc(nout, sizeof(DWORD));
            if (ii && oo &&
                SUCCEEDED(IMFTransform_GetStreamIDs(e->mft, nin, ii, nout, oo))) {
                e->in_id = ii[0];
                e->out_id = oo[0];
            }
            free(ii); free(oo);
        }
    }

    /* D3D11 path — hardware MFTs only. Zero-copy when the caller shares its
     * capture device; the MFT still needs a device manager even for CPU input
     * (NVIDIA's MFT refuses SetOutputType with MF_E_UNSUPPORTED_D3D_TYPE
     * without one), so make our own when none was handed in. The software MFT
     * is not D3D-aware and takes CPU NV12 only, so it skips all of this. */
    if (e->is_hardware) {
        UINT32 aware = 0;
        IMFAttributes *a2 = NULL;
        if (SUCCEEDED(IMFTransform_GetAttributes(e->mft, &a2)) && a2) {
            IMFAttributes_GetUINT32(a2, &MF_SA_D3D11_AWARE, &aware);
            SAFE_RELEASE(a2);
        }
        if (!aware) {
            /* A-19: це НЕ помилка відкриття, це «апаратного шляху тут нема».
             * OOS_ENC_ERROR ховав саме цей факт: Go-бік бачив звичайну відмову
             * і пробував софт лише там, де вистачало ядер
             * (softwareNativeAffordable) — тобто на слабкій машині ПК лишався
             * без картинки взагалі. NOHW читається однозначно, і фолбек
             * вмикається завжди. */
            set_msg(err, err_len, "hardware MFT is not MF_SA_D3D11_AWARE");
            oos_enc_close(e);
            return OOS_ENC_NOHW;
        }
        if (cfg->d3d_device) {
            e->dev = (ID3D11Device *)(void *)cfg->d3d_device;
            ID3D11Device_AddRef(e->dev);
        } else {
            D3D_FEATURE_LEVEL fl;
            hr = D3D11CreateDevice(NULL, D3D_DRIVER_TYPE_HARDWARE, NULL,
                                   D3D11_CREATE_DEVICE_VIDEO_SUPPORT |
                                   D3D11_CREATE_DEVICE_BGRA_SUPPORT,
                                   NULL, 0, D3D11_SDK_VERSION, &e->dev, &fl, NULL);
            if (FAILED(hr)) { set_err(err, err_len, "D3D11CreateDevice(internal)", hr);
                              oos_enc_close(e); return OOS_ENC_ERROR; }
        }
        ID3D11Device_GetImmediateContext(e->dev, &e->ctx);

        /* The MFT touches the device from its own threads. */
        ID3D10Multithread *mt = NULL;
        if (SUCCEEDED(ID3D11DeviceContext_QueryInterface(e->ctx,
                &IID_ID3D10Multithread, (void **)&mt)) && mt) {
            ID3D10Multithread_SetMultithreadProtected(mt, TRUE);
            SAFE_RELEASE(mt);
        }

        hr = MFCreateDXGIDeviceManager(&e->reset_token, &e->devmgr);
        if (FAILED(hr)) { set_err(err, err_len, "MFCreateDXGIDeviceManager", hr);
                          oos_enc_close(e); return OOS_ENC_ERROR; }
        hr = IMFDXGIDeviceManager_ResetDevice(e->devmgr, (IUnknown *)e->dev,
                                              e->reset_token);
        if (FAILED(hr)) { set_err(err, err_len, "ResetDevice", hr);
                          oos_enc_close(e); return OOS_ENC_ERROR; }
        hr = IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_SET_D3D_MANAGER,
                                         (ULONG_PTR)e->devmgr);
        if (FAILED(hr)) { set_err(err, err_len, "SET_D3D_MANAGER", hr);
                          oos_enc_close(e); return OOS_ENC_ERROR; }
        if (cfg->d3d_device) {
            hr = make_pool(e);
            if (FAILED(hr)) { set_err(err, err_len, "CreateTexture2D(pool)", hr);
                              oos_enc_close(e); return OOS_ENC_ERROR; }
            if (e->src_w != e->width || e->src_h != e->height) {
                hr = make_scaler(e);
                if (FAILED(hr)) { set_err(err, err_len, "VideoProcessor(scaler)", hr);
                                  oos_enc_close(e); return OOS_ENC_ERROR; }
            }
            e->d3d = 1;
        }
    }

    /* ТЗ P8: софтверний Microsoft H264 MFT частину ICodecAPI (rate control,
     * B-кадри, LowLatency, к-сть потоків) читає лише при узгодженні типу —
     * MSDN «H.264 Video Encoder»: ці властивості задаються ДО SetOutputType.
     * Тому для софту ставимо їх і до, і (як для всіх) після; повтор
     * безпечний. Звіт cfg_report лишається від другого, остаточного, проходу. */
    if (!e->is_hardware &&
        SUCCEEDED(IMFTransform_QueryInterface(e->mft, &IID_ICodecAPI,
                                              (void **)&e->codec))) {
        configure_codecapi(e);
        SAFE_RELEASE(e->codec);
    }

    /* Encoders want the output type first. */
    hr = set_output_type(e);
    if (FAILED(hr)) { set_err(err, err_len, "SetOutputType(H264)", hr);
                      oos_enc_close(e); return OOS_ENC_ERROR; }
    hr = set_input_type(e);
    if (FAILED(hr)) { set_err(err, err_len, "SetInputType(NV12)", hr);
                      oos_enc_close(e); return OOS_ENC_ERROR; }

    if (SUCCEEDED(IMFTransform_QueryInterface(e->mft, &IID_ICodecAPI,
                                              (void **)&e->codec))) {
        configure_codecapi(e);
    }
    /* A-18: причина, з якої машина з апаратним енкодером поїхала на софті,
     * мусить бути в лозі — інакше це шукатимуть замірами на живому парку.
     * Ставиться ПІСЛЯ configure_codecapi: та чистить звіт на початку. */
    if (luid_miss) {
        note_cfg(e, "hw-mft-on-other-adapter", E_FAIL);
    }
    cache_seq_header(e);

    {
        MFT_OUTPUT_STREAM_INFO si;
        memset(&si, 0, sizeof si);
        if (SUCCEEDED(IMFTransform_GetOutputStreamInfo(e->mft, e->out_id, &si))) {
            e->out_buf_size = si.cbSize;
            e->provides_samples = (si.dwFlags &
                (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES |
                 MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES)) ? 1 : 0;
        }
    }

    if (e->async) {
        hr = IMFTransform_QueryInterface(e->mft, &IID_IMFMediaEventGenerator,
                                         (void **)&e->evgen);
        if (FAILED(hr)) { set_err(err, err_len, "QI IMFMediaEventGenerator", hr);
                          oos_enc_close(e); return OOS_ENC_ERROR; }
    }

    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_COMMAND_FLUSH, 0);
    hr = IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    if (FAILED(hr)) { set_err(err, err_len, "BEGIN_STREAMING", hr);
                      oos_enc_close(e); return OOS_ENC_ERROR; }
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);

    *out = e;
    return OOS_ENC_OK;
}

int oos_enc_submit_cpu(oos_enc *e,
                       const uint8_t *y, int32_t y_pitch,
                       const uint8_t *uv, int32_t uv_pitch,
                       int64_t pts_100ns, char *err, int32_t err_len)
{
    if (!e || !y || !uv) return OOS_ENC_ERROR;

    const int32_t w = e->width, h = e->height;
    const int32_t chroma_rows = ((h + 1) & ~1) / 2;
    const DWORD total = (DWORD)(w * h + w * chroma_rows);

    IMFSample *sample = NULL;
    IMFMediaBuffer *mb = NULL;
    HRESULT hr = MFCreateSample(&sample);
    if (FAILED(hr)) { set_err(err, err_len, "MFCreateSample", hr); return OOS_ENC_ERROR; }
    hr = MFCreateMemoryBuffer(total, &mb);
    if (FAILED(hr)) { SAFE_RELEASE(sample);
                      set_err(err, err_len, "MFCreateMemoryBuffer", hr);
                      return OOS_ENC_ERROR; }

    BYTE *dst = NULL; DWORD maxlen = 0, curlen = 0;
    hr = IMFMediaBuffer_Lock(mb, &dst, &maxlen, &curlen);
    if (FAILED(hr)) { SAFE_RELEASE(mb); SAFE_RELEASE(sample);
                      set_err(err, err_len, "Lock input buffer", hr);
                      return OOS_ENC_ERROR; }
    for (int32_t r = 0; r < h; r++)
        memcpy(dst + (size_t)r * w, y + (size_t)r * y_pitch, (size_t)w);
    BYTE *duv = dst + (size_t)w * h;
    for (int32_t r = 0; r < chroma_rows; r++)
        memcpy(duv + (size_t)r * w, uv + (size_t)r * uv_pitch, (size_t)w);
    IMFMediaBuffer_Unlock(mb);
    IMFMediaBuffer_SetCurrentLength(mb, total);

    IMFSample_AddBuffer(sample, mb);
    SAFE_RELEASE(mb);
    IMFSample_SetSampleTime(sample, (LONGLONG)pts_100ns);
    IMFSample_SetSampleDuration(sample, sample_dur(e, pts_100ns));
    e->last_pts = pts_100ns;
    apply_refine_qp(e, sample);

    int rc = submit_sample(e, sample, err, err_len);
    SAFE_RELEASE(sample);
    return rc;
}

int oos_enc_submit_texture(oos_enc *e, uintptr_t tex, uint64_t gen,
                           int64_t pts_100ns, char *err, int32_t err_len)
{
    if (!e || !tex) return OOS_ENC_ERROR;
    if (!e->d3d) { set_msg(err, err_len, "encoder was opened without a D3D device");
                   return OOS_ENC_ERROR; }

    /* A-16: the pool is a ring of POOL_SLOTS textures and the async MFT reads
     * them on its own schedule. Reusing a slot the encoder is still reading =
     * a torn frame. Fence: never more than POOL_SLOTS frames in flight; give
     * the MFT up to 50 ms to return one before falling back to the old
     * (racy) behaviour rather than stalling the capture loop. */
    if (e->async && e->inflight >= POOL_SLOTS) {
        int64_t t0 = now_us(e);
        while (e->inflight >= POOL_SLOTS && now_us(e) - t0 < 50000) {
            int r = pump_event(e, 0, err, err_len);
            if (r < 0) return OOS_ENC_ERROR;
            if (r == 0) SwitchToThread();
        }
    }
    int slot_idx = e->pool_next;
    ID3D11Texture2D *slot = e->pool[slot_idx];
    e->pool_next = (e->pool_next + 1) % POOL_SLOTS;

    if (e->vproc) {
        /* Rescale on the GPU. The source texture is stable across frames, so
         * the input view is built once. */
        ID3D11Texture2D *src = (ID3D11Texture2D *)(void *)tex;
        /* A-06: (pointer, generation) is the identity, never the pointer on its
         * own — a rebuilt capture pipeline can reuse the freed address. */
        if (!e->vin_valid || e->vin_tex != src || e->vin_gen != gen) {
            SAFE_RELEASE(e->vin);
            D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC ivd;
            memset(&ivd, 0, sizeof ivd);
            ivd.FourCC = 0;
            ivd.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
            HRESULT vhr = ID3D11VideoDevice_CreateVideoProcessorInputView(e->vdev,
                    (ID3D11Resource *)src, e->venum, &ivd, &e->vin);
            if (FAILED(vhr)) { set_err(err, err_len, "CreateVideoProcessorInputView", vhr);
                               return OOS_ENC_ERROR; }
            e->vin_tex = src;
            e->vin_gen = gen;
            e->vin_valid = 1;
        }
        D3D11_VIDEO_PROCESSOR_STREAM st;
        memset(&st, 0, sizeof st);
        st.Enable = TRUE;
        st.pInputSurface = e->vin;
        HRESULT vhr = ID3D11VideoContext_VideoProcessorBlt(e->vctx, e->vproc,
                e->pool_view[slot_idx], 0, 1, &st);
        if (FAILED(vhr)) { set_err(err, err_len, "VideoProcessorBlt(scale)", vhr);
                           return OOS_ENC_ERROR; }
    } else {
        /* Same size: a plain GPU->GPU copy is enough. The caller's capture
         * pipeline overwrites its own NV12 target every frame, and the MFT
         * holds the buffer past ProcessInput. Still no CPU map. */
        ID3D11DeviceContext_CopyResource(e->ctx, (ID3D11Resource *)slot,
                                         (ID3D11Resource *)(void *)tex);
    }

    IMFMediaBuffer *mb = NULL;
    HRESULT hr = MFCreateDXGISurfaceBuffer(&IID_ID3D11Texture2D,
                                           (IUnknown *)slot, 0, FALSE, &mb);
    if (FAILED(hr)) { set_err(err, err_len, "MFCreateDXGISurfaceBuffer", hr);
                      return OOS_ENC_ERROR; }

    /* Length must be set explicitly for DXGI buffers. */
    IMF2DBuffer *b2d = NULL;
    if (SUCCEEDED(IMFMediaBuffer_QueryInterface(mb, &IID_IMF2DBuffer,
                                                (void **)&b2d)) && b2d) {
        DWORD len = 0;
        if (SUCCEEDED(IMF2DBuffer_GetContiguousLength(b2d, &len)))
            IMFMediaBuffer_SetCurrentLength(mb, len);
        SAFE_RELEASE(b2d);
    }

    IMFSample *sample = NULL;
    hr = MFCreateSample(&sample);
    if (FAILED(hr)) { SAFE_RELEASE(mb);
                      set_err(err, err_len, "MFCreateSample", hr);
                      return OOS_ENC_ERROR; }
    IMFSample_AddBuffer(sample, mb);
    SAFE_RELEASE(mb);
    IMFSample_SetSampleTime(sample, (LONGLONG)pts_100ns);
    IMFSample_SetSampleDuration(sample, sample_dur(e, pts_100ns));
    e->last_pts = pts_100ns;
    apply_refine_qp(e, sample);

    int rc = submit_sample(e, sample, err, err_len);
    SAFE_RELEASE(sample);
    return rc;
}

int oos_enc_poll(oos_enc *e, oos_enc_au *au, uint32_t timeout_ms,
                 char *err, int32_t err_len)
{
    if (!e || !au) return OOS_ENC_ERROR;

    if (e->outq_len == 0 && e->async) {
        ULONGLONG deadline = GetTickCount64() + timeout_ms;
        for (;;) {
            int r = pump_event(e, 0, err, err_len);
            if (r < 0) return OOS_ENC_ERROR;
            if (e->outq_len > 0) break;
            if (r == 0) {
                if (GetTickCount64() >= deadline) break;
                Sleep(0);
            }
        }
    }

    out_slot *s = outq_pop(e);
    if (!s) return e->draining ? OOS_ENC_AGAIN : OOS_ENC_AGAIN;

    /* Hand ownership of the bytes to the caller until release. */
    free(e->held);
    e->held = s->buf;
    s->buf = NULL; s->cap = 0;

    au->data     = e->held;
    au->len      = s->len;
    au->keyframe = s->keyframe;
    au->pts_100ns = s->pts;
    return OOS_ENC_OK;
}

void oos_enc_release_au(oos_enc *e)
{
    if (!e) return;
    free(e->held);
    e->held = NULL;
}

int oos_enc_force_idr(oos_enc *e, char *err, int32_t err_len)
{
    if (!e) return OOS_ENC_ERROR;
    if (!e->codec) { set_msg(err, err_len, "MFT has no ICodecAPI"); return OOS_ENC_ERROR; }
    VARIANT var;
    VariantInit(&var);
    var.vt = VT_UI4;
    var.ulVal = 1;
    HRESULT hr = ICodecAPI_SetValue(e->codec, &OOS_AVEncVideoForceKeyFrame, &var);
    VariantClear(&var);
    if (FAILED(hr)) { set_err(err, err_len, "AVEncVideoForceKeyFrame", hr);
                      return OOS_ENC_ERROR; }
    return OOS_ENC_OK;
}

int oos_enc_set_bitrate(oos_enc *e, int32_t bps, char *err, int32_t err_len)
{
    if (!e) return OOS_ENC_ERROR;
    if (bps <= 0) { set_msg(err, err_len, "bitrate must be positive"); return OOS_ENC_ERROR; }
    if (!e->codec) { set_msg(err, err_len, "MFT has no ICodecAPI"); return OOS_ENC_ERROR; }
    HRESULT hr = set_codec_u32(e, &OOS_AVEncCommonMeanBitRate, (ULONG)bps);
    if (FAILED(hr)) { set_err(err, err_len, "AVEncCommonMeanBitRate", hr);
                      return OOS_ENC_ERROR; }
    /* Peak and HRD follow the mean (best effort, as at open). */
    set_codec_u32(e, &OOS_AVEncCommonMaxBitRate, peak_bps(bps));
    set_codec_u32(e, &OOS_AVEncCommonBufferSize, hrd_bits(bps));
    /* Keep the cached target in sync: set_output_type_level() stamps it into
     * MF_MT_AVG_BITRATE on every output-type renegotiation, which would
     * otherwise resurrect the value from open time. */
    e->bitrate = bps;
    return OOS_ENC_OK;
}

int oos_enc_set_refine_qp(oos_enc *e, int32_t qp, char *err, int32_t err_len)
{
    if (!e) return OOS_ENC_ERROR;
    if (qp < 0 || qp > 51) { set_msg(err, err_len, "refine qp out of range"); return OOS_ENC_ERROR; }
    e->refine_qp = qp;
    if (!e->codec) return OOS_ENC_OK; /* sample attribute alone */
    /* MaxQP caps the rate controller from above, so the frame cannot come out
     * blurrier than qp even if the per-sample QP is ignored. 51 = no cap.
     * MaxBitRate/BufferSize stay untouched: the HRD still bounds the burst. */
    HRESULT hr = set_codec_u32(e, &OOS_AVEncVideoMaxQP, (ULONG)(qp > 0 ? qp : 51));
    if (FAILED(hr)) { set_err(err, err_len, "AVEncVideoMaxQP", hr);
                      return OOS_ENC_ERROR; }
    return OOS_ENC_OK;
}

int oos_enc_flush(oos_enc *e, char *err, int32_t err_len)
{
    if (!e) return OOS_ENC_ERROR;

    /* Drain: let the MFT finish what it holds, then throw the state away and
     * restart streaming. The next frame is an IDR (plan §5.5). */
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_COMMAND_DRAIN, 0);
    if (e->async) {
        e->draining = 1;
        ULONGLONG deadline = GetTickCount64() + 500;
        while (e->draining && GetTickCount64() < deadline) {
            int r = pump_event(e, 0, err, err_len);
            if (r < 0) break;
            if (r == 0) Sleep(0);
        }
        e->draining = 0;
    } else {
        for (;;) { int got = 0; if (FAILED(drain_one_output(e, &got)) || !got) break; }
    }

    /* Discard everything we buffered — the epoch is over. */
    while (e->outq_len) { out_slot *s = outq_pop(e); free(s->buf); s->buf = NULL; s->cap = 0; s->len = 0; }
    e->need_input = 0;
    e->outq_dropped = 0;   /* the epoch is over; the drop no longer matters */

    HRESULT hr = IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_COMMAND_FLUSH, 0);
    if (FAILED(hr)) { set_err(err, err_len, "COMMAND_FLUSH", hr); return OOS_ENC_ERROR; }
    hr = IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    if (FAILED(hr)) { set_err(err, err_len, "BEGIN_STREAMING(restart)", hr);
                      return OOS_ENC_ERROR; }
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
    return oos_enc_force_idr(e, err, err_len);
}

void oos_enc_close(oos_enc *e)
{
    if (!e) return;
    if (e->mft) {
        IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
        IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_COMMAND_FLUSH, 0);
        IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
        if (e->d3d) IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_SET_D3D_MANAGER, 0);
    }
    /* ONE cleanup path for the queue: the all-slots sweep below owns every
     * buffer. Popping without NULLing used to free the same pointer twice
     * (pop-free, then slot-free). */
    e->outq_head = e->outq_len = 0;
    for (int i = 0; i < OUTQ_SLOTS; i++) {
        free(e->outq[i].buf);
        e->outq[i].buf = NULL;
        e->outq[i].cap = e->outq[i].len = 0;
    }
    free(e->held);
    free(e->seq_hdr);
    SAFE_RELEASE(e->codec);
    SAFE_RELEASE(e->evgen);
    SAFE_RELEASE(e->mft);
    SAFE_RELEASE(e->vin);
    SAFE_RELEASE(e->vproc);
    SAFE_RELEASE(e->venum);
    SAFE_RELEASE(e->vctx);
    SAFE_RELEASE(e->vdev);
    for (int i = 0; i < POOL_SLOTS; i++) SAFE_RELEASE(e->pool_view[i]);
    for (int i = 0; i < POOL_SLOTS; i++) SAFE_RELEASE(e->pool[i]);
    SAFE_RELEASE(e->devmgr);
    SAFE_RELEASE(e->ctx);
    SAFE_RELEASE(e->dev);
    {
        int32_t held = e->mf_held;
        free(e);
        /* MFShutdown/CoUninitialize run on the anchor thread that called the
         * matching MFStartup/CoInitializeEx, and only for the last encoder. */
        if (held) oos_mf_release();
    }
}

void oos_enc_headers(oos_enc *e, const uint8_t **out, int32_t *len)
{
    if (!e) { *out = NULL; *len = 0; return; }
    *out = e->seq_hdr;
    *len = e->seq_hdr_len;
}

const char *oos_enc_name(oos_enc *e)     { return e ? e->name : ""; }
int32_t oos_enc_is_async(oos_enc *e)     { return e ? e->async : 0; }
int32_t oos_enc_is_hardware(oos_enc *e)  { return e ? e->is_hardware : 0; }
int32_t oos_enc_is_d3d(oos_enc *e)       { return e ? e->d3d : 0; }
int32_t oos_enc_cpu_maps(oos_enc *e)     { return e ? e->cpu_maps : 0; }
int32_t oos_enc_level(oos_enc *e)        { return e ? e->level : 0; }
int32_t oos_enc_profile(oos_enc *e)      { return e ? e->profile : 0; }

void oos_enc_last_timing(oos_enc *e, int64_t *wait_us, int64_t *process_us)
{
    *wait_us    = e ? e->last_wait_us : 0;
    *process_us = e ? e->last_process_us : 0;
}
