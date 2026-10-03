/* mft.h — C layer for the hardware Media Foundation H.264 encoder (Ф0, plan §5.1).
 *
 * Contract with the Go wrapper (encode_windows.go):
 *   oos_enc_open()      MFTEnumEx(HARDWARE) -> first H.264 MFT, low-latency CBR
 *   oos_enc_submit_*()  hand one NV12 frame in (CPU bytes or D3D11 texture)
 *   oos_enc_poll()      pop one finished Annex-B access unit
 *   oos_enc_force_idr() next submitted frame becomes an IDR
 *   oos_enc_flush()     MFT_MESSAGE_COMMAND_FLUSH + restart (plan §5.5)
 *   oos_enc_close()     tear everything down
 *
 * Every status is an oos_enc_status; a nonzero one leaves the out-params alone.
 *
 * Threading: one Go goroutine at a time (the Go side holds a mutex). The MFT is
 * asynchronous, so submit/poll internally pump IMFMediaEventGenerator.
 */
#ifndef OOS_ENC_MFT_H
#define OOS_ENC_MFT_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct oos_enc oos_enc;

enum {
    OOS_ENC_OK        = 0, /* success; for poll: an AU is in *au */
    OOS_ENC_AGAIN     = 1, /* poll: nothing ready yet, feed more input */
    OOS_ENC_EOS       = 2, /* poll after drain: stream finished */
    OOS_ENC_NOHW      = 3, /* no hardware H.264 MFT on this machine */
    OOS_ENC_ERROR     = 4  /* real failure, see err buffer */
};

typedef struct {
    int32_t width;
    int32_t height;
    int32_t fps;
    int32_t bitrate_bps;
    int32_t gop;              /* 0 -> 2*fps */

    /* Optional ID3D11Device* shared with the capture pipeline. When non-NULL the
     * encoder runs the DXGI path: IMFDXGIDeviceManager + MFCreateDXGISurfaceBuffer,
     * and NV12 never touches the CPU. NULL -> CPU byte path (Ф0 fallback). */
    uintptr_t d3d_device;

    /* Size of the incoming NV12 textures, when it differs from width/height.
     * The encoder then rescales NV12->NV12 on the GPU (VideoProcessorBlt) —
     * needed where the desktop is bigger than what the encoder accepts. Zero
     * means "same as width/height". Ignored on the CPU path. */
    int32_t src_width;
    int32_t src_height;

    /* Skip the hardware MFTEnumEx and go straight to the built-in software
     * Microsoft H264 Video Encoder MFT (CLSID_CMSH264EncoderMFT). Used by the
     * gate to exercise the software sync path on a machine that also has
     * hardware. Zero -> hardware first, software only as an automatic fallback. */
    int32_t force_software;
} oos_enc_cfg;

typedef struct {
    const uint8_t *data;   /* Annex-B bytes, valid until oos_enc_release_au() */
    int32_t        len;
    int32_t        keyframe;
    int64_t        pts_100ns;
} oos_enc_au;

/* Opens the first hardware H.264 MFT. OOS_ENC_NOHW when none exists. */
int oos_enc_open(const oos_enc_cfg *cfg, oos_enc **out, char *err, int32_t err_len);

/* Submits one NV12 frame from CPU memory (copied into an IMFMediaBuffer). */
int oos_enc_submit_cpu(oos_enc *e,
                       const uint8_t *y, int32_t y_pitch,
                       const uint8_t *uv, int32_t uv_pitch,
                       int64_t pts_100ns, char *err, int32_t err_len);

/* Submits one NV12 ID3D11Texture2D*. Requires cfg.d3d_device. The texture is
 * GPU-copied into an encoder-owned pool slot (the caller reuses its own texture
 * every frame), then wrapped with MFCreateDXGISurfaceBuffer. No CPU map.
 *
 * A-06: `gen` is the caller's pipeline generation for `tex` — it must change
 * whenever the texture behind that address is rebuilt. The rescale path caches
 * a VideoProcessorInputView per source texture and used to key that cache on
 * the POINTER alone; a capturer rebuilt on the same device can hand back the
 * same address for a brand-new texture, and the stale view then fed the encoder
 * a freed surface. Pass 0 if you never rebuild. */
int oos_enc_submit_texture(oos_enc *e, uintptr_t tex, uint64_t gen,
                           int64_t pts_100ns, char *err, int32_t err_len);

/* Pops one finished AU. OOS_ENC_AGAIN when the MFT has nothing yet. */
int oos_enc_poll(oos_enc *e, oos_enc_au *au, uint32_t timeout_ms,
                 char *err, int32_t err_len);

/* Frees the buffer handed out by the last successful poll. */
void oos_enc_release_au(oos_enc *e);

/* Requests an IDR on the next submitted frame (CODECAPI_AVEncVideoForceKeyFrame). */
int oos_enc_force_idr(oos_enc *e, char *err, int32_t err_len);

/* Retargets the live encoder's CBR bitrate (CODECAPI_AVEncCommonMeanBitRate).
 * Takes effect without reopening the MFT; the rate control mode is untouched.
 * The HRESULT from ICodecAPI is reported through err, never swallowed. */
int oos_enc_set_bitrate(oos_enc *e, int32_t bps, char *err, int32_t err_len);

/* Drain + MFT_MESSAGE_COMMAND_FLUSH + restart streaming; next frame is an IDR. */
int oos_enc_flush(oos_enc *e, char *err, int32_t err_len);

void oos_enc_close(oos_enc *e);

/* Cached SPS/PPS (MF_MT_MPEG_SEQUENCE_HEADER of the negotiated output type).
 * Annex-B with start codes; len 0 when the MFT did not publish one. */
void oos_enc_headers(oos_enc *e, const uint8_t **out, int32_t *len);

/* Diagnostics for the gate report. */
const char *oos_enc_name(oos_enc *e);     /* MFT friendly name */
const char *oos_enc_cfg_report(oos_enc *e); /* A-14: knobs the MFT refused ("" = none) */
int32_t oos_enc_is_async(oos_enc *e);     /* 1 when async MFT */
int32_t oos_enc_is_hardware(oos_enc *e);  /* 1 when a hardware MFT (MFTEnumEx HARDWARE) */
int32_t oos_enc_is_d3d(oos_enc *e);       /* 1 when the DXGI zero-copy path is live */
int32_t oos_enc_cpu_maps(oos_enc *e);     /* count of CPU maps of NV12 done by us */
int32_t oos_enc_level(oos_enc *e);
int32_t oos_enc_profile(oos_enc *e);  /* eAVEncH264VProfile_* that stuck: 77=Main, 100=High */

/* Timing of the most recent submit, in microseconds: how long we waited for the
 * MFT to ask for input (back-pressure) and how long ProcessInput itself took
 * (the cost the capture loop actually pays). */
void oos_enc_last_timing(oos_enc *e, int64_t *wait_us, int64_t *process_us);        /* H.264 level we managed to pin, -1 = MFT chose */

#ifdef __cplusplus
}
#endif
#endif /* OOS_ENC_MFT_H */
