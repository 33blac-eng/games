/* dxgi.h — C layer for DXGI Desktop Duplication + GPU BGRA->NV12 (Ф0).
 *
 * Contract with the Go wrapper (capture_windows.go):
 *   oos_open()        create D3D11 device + duplication + persistent NV12 pipeline
 *   oos_next()        AcquireNextFrame -> composite cursor -> VideoProcessorBlt -> readback
 *   oos_release()     unmap the staging texture after Go copied the planes
 *   oos_close()       tear everything down
 *
 * Every status is an oos_status; a nonzero one leaves *frame untouched.
 */
#ifndef OOS_DXGI_H
#define OOS_DXGI_H

/* This file is literally named dxgi.h, so it shadows the Windows SDK/mingw
 * <dxgi.h> for every header that pulls DXGI in (d3d11.h does). Chain to the
 * real one first so those headers still see DXGI_FORMAT and friends. */
#ifdef _WIN32
#include_next <dxgi.h>
#endif

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct oos_cap oos_cap;

/* Status codes returned by oos_open / oos_next. */
enum {
    OOS_OK           = 0, /* a frame is available in *frame */
    OOS_TIMEOUT      = 1, /* DXGI_ERROR_WAIT_TIMEOUT: not a frame, not an error */
    OOS_ACCESS_LOST  = 2, /* ACCESS_LOST / DEVICE_REMOVED / DEVICE_RESET: recreate */
    OOS_ERROR        = 3, /* real failure, see err buffer / oos_last_hr */
    /* A-07: DXGI_ERROR_INVALID_CALL used to be folded into OOS_ACCESS_LOST.
     * It is recovered the same way (recreate), but it is NOT the same event:
     * ACCESS_LOST is the desktop switching under us, INVALID_CALL is us
     * calling DXGI wrong (a frame acquired twice without a release, a stale
     * duplication). Losing that distinction in the log turned our own bug
     * into "Windows took the desktop away". */
    OOS_INVALID_CALL = 4
};

/* Cursor shape kinds mirrored from DXGI_OUTDUPL_POINTER_SHAPE_TYPE. */
enum {
    OOS_CUR_NONE         = 0,
    OOS_CUR_MONOCHROME   = 1,
    OOS_CUR_COLOR        = 2,
    OOS_CUR_MASKED_COLOR = 4
};

typedef struct {
    /* Mapped NV12 staging planes. Valid only until oos_release(). */
    const uint8_t *y;
    const uint8_t *uv;
    int32_t y_pitch;
    int32_t uv_pitch;

    int32_t width;
    int32_t height;

    /* Cursor state that was composited into this frame. */
    int32_t cursor_visible;     /* 1 if the pointer is on this output */
    int32_t cursor_composited;  /* 1 if we actually drew it into the BGRA texture */
    int32_t cursor_shape_type;  /* OOS_CUR_* of the cached shape */
    int32_t cursor_x;
    int32_t cursor_y;

    /* 1 when only the pointer moved (LastPresentTime == 0): desktop image is
     * identical to the previous frame, but the composited cursor moved. */
    int32_t mouse_only;
    uint32_t accumulated_frames;
} oos_frame;

/* Creates the pipeline for output `output_idx` of adapter 0.
 * `err`/`err_len` receive a human-readable reason on OOS_ERROR. */
int oos_open(int32_t output_idx, oos_cap **out, char *err, int32_t err_len);

/* Blocks up to timeout_ms inside AcquireNextFrame. */
int oos_next(oos_cap *c, uint32_t timeout_ms, oos_frame *frame,
             char *err, int32_t err_len);

/* Releases the map taken by a successful oos_next. Safe to call twice. */
void oos_release(oos_cap *c);

/* Grabs the CURRENT desktop with GDI (BitBlt) instead of waiting for a change,
 * and runs it through the same BGRA->NV12 conversion as oos_next. Desktop
 * Duplication reports nothing at all while the screen is still, so this is the
 * only way to get a first frame out of a session that starts on a static
 * desktop. Costs a full-screen blit (~50ms at 2560x1440) — a fallback, not the
 * steady path. The pointer is not in the image: BitBlt does not draw it. */
int oos_gdi_next(oos_cap *c, oos_frame *frame, char *err, int32_t err_len);

void oos_close(oos_cap *c);

/* Dimensions of the duplicated output (texture orientation, not rotated). */
int32_t oos_width(oos_cap *c);
int32_t oos_height(oos_cap *c);

/* Last HRESULT seen, for logging. */
int32_t oos_last_hr(oos_cap *c);

/* Zero-copy handoff to the encoder (agent/encode).
 *
 * oos_set_readback(c, 0) turns the CPU staging copy+Map off: oos_next then
 * leaves frame->y/uv NULL and the NV12 stays on the GPU. The consumer takes
 * oos_nv12_texture() and wraps it with MFCreateDXGISurfaceBuffer on the device
 * from oos_device(). Readback is ON by default, so capture-probe is unaffected.
 *
 * The returned texture is the persistent Blt target: it is overwritten by the
 * next oos_next(), so a consumer that holds frames must copy it GPU-side. */
void   *oos_device(oos_cap *c);
void   *oos_nv12_texture(oos_cap *c);
void oos_suspend(oos_cap *c);   /* A-17: drop only the duplication; oos_next re-duplicates lazily */
void    oos_set_readback(oos_cap *c, int32_t enable);
int64_t oos_cpu_maps(oos_cap *c);

/* Number of outputs on adapter 0; <0 on failure. */
int32_t oos_output_count(void);

/* Size (desktop coordinates) and primary-flag of output `idx` on adapter 0,
 * without opening a duplication. OOS_OK or OOS_ERROR. */
int oos_output_info(int32_t idx, int32_t *width, int32_t *height, int32_t *primary);

#ifdef __cplusplus
}
#endif
#endif /* OOS_DXGI_H */
