//go:build windows

#define COBJMACROS
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <audioclient.h>
#include <mmdeviceapi.h>
#include <mmreg.h>

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "wasapi.h"

#define OOS_QUEUE_LIMIT 64

static const GUID oos_clsid_mmdevice_enumerator = {
    0xbcde0395, 0xe52f, 0x467c, {0x8e, 0x3d, 0xc4, 0x57, 0x92, 0x91, 0x69, 0x2e}
};
static const GUID oos_iid_mmdevice_enumerator = {
    0xa95664d2, 0x9614, 0x4f35, {0xa7, 0x46, 0xde, 0x8d, 0xb6, 0x36, 0x17, 0xe6}
};
static const GUID oos_iid_audio_client = {
    0x1cb9ad4c, 0xdbfa, 0x4c32, {0xb1, 0x78, 0xc2, 0xf5, 0x68, 0xa7, 0x03, 0xb2}
};
static const GUID oos_iid_audio_capture_client = {
    0xc8adbd64, 0xe71e, 0x48a0, {0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17}
};
static const GUID oos_iid_audio_render_client = {
    0xf294acfc, 0x3146, 0x4483, {0xa7, 0xbf, 0xad, 0xdc, 0xa7, 0xc2, 0x60, 0xe2}
};
static const GUID oos_iid_notification_client = {
    0x7991eec9, 0x7e89, 0x4d85, {0x83, 0x90, 0x6c, 0x70, 0x3c, 0xec, 0x60, 0xc0}
};
static const GUID oos_iid_iunknown = {
    0x00000000, 0x0000, 0x0000, {0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}
};
static const GUID oos_subtype_pcm = {
    0x00000001, 0x0000, 0x0010, {0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}
};
static const GUID oos_subtype_float = {
    0x00000003, 0x0000, 0x0010, {0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}
};

typedef struct oos_queued_packet {
    struct oos_queued_packet *next;
    oos_audio_packet packet;
} oos_queued_packet;

typedef struct oos_notification {
    IMMNotificationClient iface;
    LONG refs;
    HANDLE changed_event;
} oos_notification;

struct oos_audio {
    HANDLE thread;
    HANDLE ready_event;
    HANDLE stop_event;
    HANDLE changed_event;
    HANDLE sample_event;
    HANDLE keepalive_event;
    HANDLE data_event;

    CRITICAL_SECTION lock;
    int state;
    int status;
    char error[512];

    IMMDeviceEnumerator *enumerator;
    int notification_registered;
    oos_notification notification;

    IAudioClient *client;
    IAudioCaptureClient *capture;
    IAudioClient *keepalive_client;
    IAudioRenderClient *keepalive_render;
    UINT32 keepalive_buffer_frames;
    oos_audio_format format;

    oos_queued_packet *head;
    oos_queued_packet *tail;
    unsigned int queued;
};

enum {
    OOS_STATE_STARTING = 0,
    OOS_STATE_RUNNING = 1,
    OOS_STATE_FAILED = 2,
    OOS_STATE_CLOSED = 3
};

static void copy_error(char *dst, size_t dst_len, const char *src) {
    if (!dst || dst_len == 0) return;
    if (!src) src = "unknown error";
    _snprintf(dst, dst_len, "%s", src);
    dst[dst_len - 1] = '\0';
}

static void hresult_text(char *dst, size_t dst_len, const char *where, HRESULT hr) {
    char system[256] = {0};
    DWORD n = FormatMessageA(FORMAT_MESSAGE_FROM_SYSTEM | FORMAT_MESSAGE_IGNORE_INSERTS,
                             NULL, (DWORD)hr, 0, system, (DWORD)sizeof(system), NULL);
    while (n > 0 && (system[n - 1] == '\r' || system[n - 1] == '\n' || system[n - 1] == ' ')) {
        system[--n] = '\0';
    }
    if (n > 0) {
        _snprintf(dst, dst_len, "%s failed: HRESULT 0x%08lX (%s)",
                  where, (unsigned long)hr, system);
    } else {
        _snprintf(dst, dst_len, "%s failed: HRESULT 0x%08lX",
                  where, (unsigned long)hr);
    }
    dst[dst_len - 1] = '\0';
}

static int status_for_hresult(HRESULT hr) {
    return hr == HRESULT_FROM_WIN32(ERROR_NOT_FOUND)
        ? OOS_AUDIO_NO_DEVICE : OOS_AUDIO_ERROR;
}

static HRESULT STDMETHODCALLTYPE notification_query_interface(
        IMMNotificationClient *iface, REFIID iid, void **object) {
    oos_notification *notification = CONTAINING_RECORD(iface, oos_notification, iface);
    if (!object) return E_POINTER;
    *object = NULL;
    if (IsEqualGUID(iid, &oos_iid_iunknown) ||
        IsEqualGUID(iid, &oos_iid_notification_client)) {
        *object = iface;
        InterlockedIncrement(&notification->refs);
        return S_OK;
    }
    return E_NOINTERFACE;
}

static ULONG STDMETHODCALLTYPE notification_add_ref(IMMNotificationClient *iface) {
    oos_notification *notification = CONTAINING_RECORD(iface, oos_notification, iface);
    return (ULONG)InterlockedIncrement(&notification->refs);
}

static ULONG STDMETHODCALLTYPE notification_release(IMMNotificationClient *iface) {
    oos_notification *notification = CONTAINING_RECORD(iface, oos_notification, iface);
    return (ULONG)InterlockedDecrement(&notification->refs);
}

static HRESULT STDMETHODCALLTYPE notification_state_changed(
        IMMNotificationClient *iface, LPCWSTR device_id, DWORD new_state) {
    (void)iface; (void)device_id; (void)new_state;
    return S_OK;
}

static HRESULT STDMETHODCALLTYPE notification_added(
        IMMNotificationClient *iface, LPCWSTR device_id) {
    (void)iface; (void)device_id;
    return S_OK;
}

static HRESULT STDMETHODCALLTYPE notification_removed(
        IMMNotificationClient *iface, LPCWSTR device_id) {
    (void)iface; (void)device_id;
    return S_OK;
}

static HRESULT STDMETHODCALLTYPE notification_default_changed(
        IMMNotificationClient *iface, EDataFlow flow, ERole role, LPCWSTR device_id) {
    oos_notification *notification = CONTAINING_RECORD(iface, oos_notification, iface);
    (void)role; (void)device_id;
    if (flow == eRender) SetEvent(notification->changed_event);
    return S_OK;
}

static HRESULT STDMETHODCALLTYPE notification_property_changed(
        IMMNotificationClient *iface, LPCWSTR device_id, const PROPERTYKEY key) {
    (void)iface; (void)device_id; (void)key;
    return S_OK;
}

static IMMNotificationClientVtbl notification_vtable = {
    notification_query_interface,
    notification_add_ref,
    notification_release,
    notification_state_changed,
    notification_added,
    notification_removed,
    notification_default_changed,
    notification_property_changed
};

static int same_subtype(const GUID *left, const GUID *right) {
    return IsEqualGUID(left, right);
}

int oos_audio_convert_format(const void *input, uint32_t input_len,
                             void *output, uint32_t output_capacity,
                             uint32_t *output_len, oos_audio_format *format,
                             char *err, size_t err_len) {
    WAVEFORMATEXTENSIBLE local;
    WAVEFORMATEX *wave = (WAVEFORMATEX *)&local;
    uint32_t size;
    uint16_t sample_format = OOS_SAMPLE_UNKNOWN;
    uint16_t valid_bits;
    uint32_t channel_mask = 0;

    if (!input || input_len < sizeof(WAVEFORMATEX)) {
        copy_error(err, err_len, "mix format is shorter than WAVEFORMATEX");
        return OOS_AUDIO_ERROR;
    }
    memset(&local, 0, sizeof(local));
    memcpy(&local, input, input_len < sizeof(local) ? input_len : sizeof(local));
    size = (uint32_t)sizeof(WAVEFORMATEX) + wave->cbSize;
    if (size > input_len || size > output_capacity || size > sizeof(local)) {
        copy_error(err, err_len, "mix format extension is invalid or too large");
        return OOS_AUDIO_ERROR;
    }
    if (wave->nChannels == 0 || wave->wBitsPerSample == 0 ||
        wave->wBitsPerSample % 8 != 0) {
        copy_error(err, err_len, "mix format has invalid channels or bit depth");
        return OOS_AUDIO_ERROR;
    }
    if (wave->nBlockAlign != wave->nChannels * (wave->wBitsPerSample / 8)) {
        copy_error(err, err_len, "mix format has inconsistent block alignment");
        return OOS_AUDIO_ERROR;
    }

    valid_bits = wave->wBitsPerSample;
    if (wave->wFormatTag == WAVE_FORMAT_PCM) {
        sample_format = OOS_SAMPLE_PCM;
    } else if (wave->wFormatTag == WAVE_FORMAT_IEEE_FLOAT) {
        sample_format = OOS_SAMPLE_FLOAT;
    } else if (wave->wFormatTag == WAVE_FORMAT_EXTENSIBLE &&
               wave->cbSize >= 22 && size >= sizeof(WAVEFORMATEXTENSIBLE)) {
        WAVEFORMATEXTENSIBLE *extensible = &local;
        valid_bits = extensible->Samples.wValidBitsPerSample;
        if (valid_bits == 0) valid_bits = wave->wBitsPerSample;
        channel_mask = extensible->dwChannelMask;
        if (same_subtype(&extensible->SubFormat, &oos_subtype_pcm)) {
            sample_format = OOS_SAMPLE_PCM;
        } else if (same_subtype(&extensible->SubFormat, &oos_subtype_float)) {
            sample_format = OOS_SAMPLE_FLOAT;
        }
    }
    if (sample_format == OOS_SAMPLE_UNKNOWN) {
        copy_error(err, err_len, "mix format sample type is unsupported");
        return OOS_AUDIO_ERROR;
    }
    if (valid_bits > wave->wBitsPerSample) {
        copy_error(err, err_len, "mix format valid bits exceed container bits");
        return OOS_AUDIO_ERROR;
    }
    if ((sample_format == OOS_SAMPLE_FLOAT &&
         wave->wBitsPerSample != 32 && wave->wBitsPerSample != 64) ||
        (sample_format == OOS_SAMPLE_PCM &&
         wave->wBitsPerSample != 8 && wave->wBitsPerSample != 16 &&
         wave->wBitsPerSample != 24 && wave->wBitsPerSample != 32)) {
        copy_error(err, err_len, "mix format bit depth is unsupported");
        return OOS_AUDIO_ERROR;
    }

    memcpy(output, input, size);
    wave = (WAVEFORMATEX *)output;
    wave->nSamplesPerSec = 48000;
    wave->nAvgBytesPerSec = 48000 * wave->nBlockAlign;

    if (output_len) *output_len = size;
    if (format) {
        memset(format, 0, sizeof(*format));
        format->sample_rate = 48000;
        format->channels = wave->nChannels;
        format->sample_format = sample_format;
        format->bits_per_sample = wave->wBitsPerSample;
        format->valid_bits_per_sample = valid_bits;
        format->channel_mask = channel_mask;
        format->bytes_per_frame = wave->nBlockAlign;
    }
    return OOS_AUDIO_OK;
}

static void free_queued_packet(oos_queued_packet *queued) {
    if (!queued) return;
    free(queued->packet.data);
    free(queued);
}

static void clear_queue_locked(oos_audio *audio) {
    oos_queued_packet *queued = audio->head;
    while (queued) {
        oos_queued_packet *next = queued->next;
        free_queued_packet(queued);
        queued = next;
    }
    audio->head = audio->tail = NULL;
    audio->queued = 0;
    ResetEvent(audio->data_event);
}

static HRESULT enqueue_packet(oos_audio *audio, const BYTE *data,
                              UINT32 frames, DWORD flags, UINT64 qpc_100ns) {
    oos_queued_packet *queued;
    size_t bytes = (size_t)frames * audio->format.bytes_per_frame;
    queued = (oos_queued_packet *)calloc(1, sizeof(*queued));
    if (!queued) return E_OUTOFMEMORY;
    queued->packet.data = (uint8_t *)malloc(bytes ? bytes : 1);
    if (!queued->packet.data) {
        free(queued);
        return E_OUTOFMEMORY;
    }
    if ((flags & AUDCLNT_BUFFERFLAGS_SILENT) || !data) {
        memset(queued->packet.data, 0, bytes);
    } else {
        memcpy(queued->packet.data, data, bytes);
    }
    queued->packet.bytes = (uint32_t)bytes;
    queued->packet.frames = frames;
    queued->packet.qpc_100ns = qpc_100ns;
    queued->packet.timestamp_valid =
        (flags & AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR) == 0;
    queued->packet.format = audio->format;

    EnterCriticalSection(&audio->lock);
    while (audio->queued >= OOS_QUEUE_LIMIT && audio->head) {
        oos_queued_packet *drop = audio->head;
        audio->head = drop->next;
        if (!audio->head) audio->tail = NULL;
        audio->queued--;
        free_queued_packet(drop);
    }
    if (audio->tail) audio->tail->next = queued;
    else audio->head = queued;
    audio->tail = queued;
    audio->queued++;
    SetEvent(audio->data_event);
    LeaveCriticalSection(&audio->lock);
    return S_OK;
}

static void close_client(oos_audio *audio) {
    if (audio->client) IAudioClient_Stop(audio->client);
    if (audio->keepalive_client) IAudioClient_Stop(audio->keepalive_client);
    if (audio->capture) {
        IAudioCaptureClient_Release(audio->capture);
        audio->capture = NULL;
    }
    if (audio->client) {
        IAudioClient_Release(audio->client);
        audio->client = NULL;
    }
    if (audio->keepalive_render) {
        IAudioRenderClient_Release(audio->keepalive_render);
        audio->keepalive_render = NULL;
    }
    if (audio->keepalive_client) {
        IAudioClient_Release(audio->keepalive_client);
        audio->keepalive_client = NULL;
    }
    audio->keepalive_buffer_frames = 0;
    ResetEvent(audio->sample_event);
    ResetEvent(audio->keepalive_event);
}

static HRESULT refill_keepalive(oos_audio *audio) {
    UINT32 padding = 0;
    UINT32 available;
    BYTE *data = NULL;
    HRESULT hr = IAudioClient_GetCurrentPadding(audio->keepalive_client, &padding);
    if (FAILED(hr)) return hr;
    available = audio->keepalive_buffer_frames - padding;
    if (available == 0) return S_OK;
    hr = IAudioRenderClient_GetBuffer(audio->keepalive_render, available, &data);
    if (FAILED(hr)) return hr;
    return IAudioRenderClient_ReleaseBuffer(
        audio->keepalive_render, available, AUDCLNT_BUFFERFLAGS_SILENT);
}

static HRESULT open_client(oos_audio *audio, int *status,
                           char *err, size_t err_len) {
    IMMDevice *device = NULL;
    IAudioClient *client = NULL;
    IAudioCaptureClient *capture = NULL;
    IAudioClient *keepalive_client = NULL;
    IAudioRenderClient *keepalive_render = NULL;
    UINT32 keepalive_buffer_frames = 0;
    int capture_started = 0;
    int keepalive_started = 0;
    WAVEFORMATEX *mix = NULL;
    BYTE converted[sizeof(WAVEFORMATEXTENSIBLE)];
    uint32_t converted_len = 0;
    oos_audio_format format;
    HRESULT hr;
    DWORD flags = AUDCLNT_STREAMFLAGS_LOOPBACK |
                  AUDCLNT_STREAMFLAGS_EVENTCALLBACK |
                  AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM |
                  AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY;

    *status = OOS_AUDIO_ERROR;
    hr = IMMDeviceEnumerator_GetDefaultAudioEndpoint(
        audio->enumerator, eRender, eConsole, &device);
    if (FAILED(hr)) {
        *status = status_for_hresult(hr);
        hresult_text(err, err_len, "GetDefaultAudioEndpoint", hr);
        goto done;
    }
    hr = IMMDevice_Activate(device, &oos_iid_audio_client,
                            CLSCTX_ALL, NULL, (void **)&client);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IMMDevice::Activate(IAudioClient)", hr);
        goto done;
    }
    hr = IAudioClient_GetMixFormat(client, &mix);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::GetMixFormat", hr);
        goto done;
    }
    if (oos_audio_convert_format(
            mix, (uint32_t)sizeof(WAVEFORMATEX) + mix->cbSize,
            converted, (uint32_t)sizeof(converted), &converted_len,
            &format, err, err_len) != OOS_AUDIO_OK) {
        hr = AUDCLNT_E_UNSUPPORTED_FORMAT;
        goto done;
    }
    (void)converted_len;
    hr = IAudioClient_Initialize(client, AUDCLNT_SHAREMODE_SHARED, flags,
                                 0, 0, (WAVEFORMATEX *)converted, NULL);
    if (FAILED(hr) && mix->nSamplesPerSec != 48000 &&
        sizeof(WAVEFORMATEX) + mix->cbSize <= sizeof(converted)) {
        /* Windows 7: no AUTOCONVERTPCM, a 48 kHz request on a 44.1 kHz device
         * is AUDCLNT_E_UNSUPPORTED_FORMAT. Capture the device's own mix format;
         * the Go side resamples to 48 kHz (agent wrapResample). */
        uint32_t keep_rate = mix->nSamplesPerSec;
        IAudioClient_Release(client);
        client = NULL;
        hr = IMMDevice_Activate(device, &oos_iid_audio_client, CLSCTX_ALL, NULL, (void **)&client);
        if (SUCCEEDED(hr)) {
            memcpy(converted, mix, sizeof(WAVEFORMATEX) + mix->cbSize);
            hr = IAudioClient_Initialize(client, AUDCLNT_SHAREMODE_SHARED,
                                         AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK,
                                         0, 0, (WAVEFORMATEX *)converted, NULL);
            if (SUCCEEDED(hr)) format.sample_rate = keep_rate;
        }
    }
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::Initialize(loopback 48kHz)", hr);
        goto done;
    }
    hr = IAudioClient_SetEventHandle(client, audio->sample_event);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::SetEventHandle", hr);
        goto done;
    }
    hr = IAudioClient_GetService(client, &oos_iid_audio_capture_client,
                                 (void **)&capture);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::GetService(IAudioCaptureClient)", hr);
        goto done;
    }

    hr = IMMDevice_Activate(device, &oos_iid_audio_client,
                            CLSCTX_ALL, NULL, (void **)&keepalive_client);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IMMDevice::Activate(keepalive IAudioClient)", hr);
        goto done;
    }
    hr = IAudioClient_Initialize(
        keepalive_client, AUDCLNT_SHAREMODE_SHARED,
        AUDCLNT_STREAMFLAGS_EVENTCALLBACK |
        AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM |
        AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY |
        AUDCLNT_STREAMFLAGS_NOPERSIST,
        0, 0, (WAVEFORMATEX *)converted, NULL);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::Initialize(silent keepalive)", hr);
        goto done;
    }
    hr = IAudioClient_SetEventHandle(keepalive_client, audio->keepalive_event);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::SetEventHandle(keepalive)", hr);
        goto done;
    }
    hr = IAudioClient_GetBufferSize(keepalive_client, &keepalive_buffer_frames);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::GetBufferSize(keepalive)", hr);
        goto done;
    }
    hr = IAudioClient_GetService(keepalive_client, &oos_iid_audio_render_client,
                                 (void **)&keepalive_render);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::GetService(IAudioRenderClient)", hr);
        goto done;
    }
    {
        BYTE *silent = NULL;
        hr = IAudioRenderClient_GetBuffer(
            keepalive_render, keepalive_buffer_frames, &silent);
        if (FAILED(hr)) {
            hresult_text(err, err_len, "IAudioRenderClient::GetBuffer(keepalive)", hr);
            goto done;
        }
        hr = IAudioRenderClient_ReleaseBuffer(
            keepalive_render, keepalive_buffer_frames,
            AUDCLNT_BUFFERFLAGS_SILENT);
        if (FAILED(hr)) {
            hresult_text(err, err_len, "IAudioRenderClient::ReleaseBuffer(keepalive)", hr);
            goto done;
        }
    }
    hr = IAudioClient_Start(client);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::Start", hr);
        goto done;
    }
    capture_started = 1;
    hr = IAudioClient_Start(keepalive_client);
    if (FAILED(hr)) {
        hresult_text(err, err_len, "IAudioClient::Start(keepalive)", hr);
        goto done;
    }
    keepalive_started = 1;

    audio->client = client;
    audio->capture = capture;
    audio->keepalive_client = keepalive_client;
    audio->keepalive_render = keepalive_render;
    audio->keepalive_buffer_frames = keepalive_buffer_frames;
    audio->format = format;
    capture_started = 0;
    keepalive_started = 0;
    client = NULL;
    capture = NULL;
    keepalive_client = NULL;
    keepalive_render = NULL;
    *status = OOS_AUDIO_OK;

done:
    if (mix) CoTaskMemFree(mix);
    if (keepalive_started) IAudioClient_Stop(keepalive_client);
    if (capture_started) IAudioClient_Stop(client);
    if (keepalive_render) IAudioRenderClient_Release(keepalive_render);
    if (keepalive_client) IAudioClient_Release(keepalive_client);
    if (capture) IAudioCaptureClient_Release(capture);
    if (client) IAudioClient_Release(client);
    if (device) IMMDevice_Release(device);
    return hr;
}

static HRESULT drain_packets(oos_audio *audio) {
    HRESULT hr;
    UINT32 available = 0;
    for (;;) {
        hr = IAudioCaptureClient_GetNextPacketSize(audio->capture, &available);
        if (FAILED(hr) || available == 0) return hr;

        BYTE *data = NULL;
        UINT32 frames = 0;
        DWORD flags = 0;
        UINT64 device_position = 0;
        UINT64 qpc_100ns = 0;
        hr = IAudioCaptureClient_GetBuffer(audio->capture, &data, &frames, &flags,
                                           &device_position, &qpc_100ns);
        (void)device_position;
        if (FAILED(hr)) return hr;
        hr = enqueue_packet(audio, data, frames, flags, qpc_100ns);
        IAudioCaptureClient_ReleaseBuffer(audio->capture, frames);
        if (FAILED(hr)) return hr;
    }
}

static void fail_capture(oos_audio *audio, int status,
                         const char *message) {
    EnterCriticalSection(&audio->lock);
    audio->state = OOS_STATE_FAILED;
    audio->status = status;
    copy_error(audio->error, sizeof(audio->error), message);
    SetEvent(audio->data_event);
    LeaveCriticalSection(&audio->lock);
}

static DWORD WINAPI capture_thread(void *context) {
    oos_audio *audio = (oos_audio *)context;
    HRESULT hr;
    int status = OOS_AUDIO_ERROR;
    char error[512] = {0};
    HANDLE waits[4] = {audio->stop_event, audio->changed_event,
                       audio->sample_event, audio->keepalive_event};

    hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (FAILED(hr)) {
        hresult_text(error, sizeof(error), "CoInitializeEx", hr);
        fail_capture(audio, OOS_AUDIO_ERROR, error);
        SetEvent(audio->ready_event);
        return 0;
    }
    hr = CoCreateInstance(&oos_clsid_mmdevice_enumerator, NULL, CLSCTX_ALL,
                          &oos_iid_mmdevice_enumerator,
                          (void **)&audio->enumerator);
    if (FAILED(hr)) {
        hresult_text(error, sizeof(error), "CoCreateInstance(MMDeviceEnumerator)", hr);
        fail_capture(audio, OOS_AUDIO_ERROR, error);
        SetEvent(audio->ready_event);
        CoUninitialize();
        return 0;
    }

    audio->notification.iface.lpVtbl = &notification_vtable;
    audio->notification.refs = 1;
    audio->notification.changed_event = audio->changed_event;
    hr = IMMDeviceEnumerator_RegisterEndpointNotificationCallback(
        audio->enumerator, &audio->notification.iface);
    if (FAILED(hr)) {
        hresult_text(error, sizeof(error),
                     "RegisterEndpointNotificationCallback", hr);
        fail_capture(audio, OOS_AUDIO_ERROR, error);
        SetEvent(audio->ready_event);
        IMMDeviceEnumerator_Release(audio->enumerator);
        audio->enumerator = NULL;
        CoUninitialize();
        return 0;
    }
    audio->notification_registered = 1;

    hr = open_client(audio, &status, error, sizeof(error));
    if (FAILED(hr)) {
        fail_capture(audio, status, error);
        SetEvent(audio->ready_event);
        goto done;
    }
    EnterCriticalSection(&audio->lock);
    audio->state = OOS_STATE_RUNNING;
    audio->status = OOS_AUDIO_OK;
    LeaveCriticalSection(&audio->lock);
    SetEvent(audio->ready_event);

    for (;;) {
        DWORD wait = WaitForMultipleObjects(4, waits, FALSE, INFINITE);
        if (wait == WAIT_OBJECT_0) break;
        if (wait == WAIT_OBJECT_0 + 1) {
            close_client(audio);
            EnterCriticalSection(&audio->lock);
            clear_queue_locked(audio);
            LeaveCriticalSection(&audio->lock);
            hr = open_client(audio, &status, error, sizeof(error));
            if (FAILED(hr)) {
                fail_capture(audio, status, error);
                break;
            }
            continue;
        }
        if (wait == WAIT_OBJECT_0 + 2) {
            hr = drain_packets(audio);
            if (FAILED(hr)) {
                close_client(audio);
                EnterCriticalSection(&audio->lock);
                clear_queue_locked(audio);
                LeaveCriticalSection(&audio->lock);
                hr = open_client(audio, &status, error, sizeof(error));
                if (FAILED(hr)) {
                    fail_capture(audio, status, error);
                    break;
                }
            }
            continue;
        }
        if (wait == WAIT_OBJECT_0 + 3) {
            hr = refill_keepalive(audio);
            if (FAILED(hr)) {
                close_client(audio);
                EnterCriticalSection(&audio->lock);
                clear_queue_locked(audio);
                LeaveCriticalSection(&audio->lock);
                hr = open_client(audio, &status, error, sizeof(error));
                if (FAILED(hr)) {
                    fail_capture(audio, status, error);
                    break;
                }
            }
            continue;
        }
        fail_capture(audio, OOS_AUDIO_ERROR, "WaitForMultipleObjects failed");
        break;
    }

done:
    close_client(audio);
    if (audio->notification_registered) {
        IMMDeviceEnumerator_UnregisterEndpointNotificationCallback(
            audio->enumerator, &audio->notification.iface);
        audio->notification_registered = 0;
    }
    if (audio->enumerator) {
        IMMDeviceEnumerator_Release(audio->enumerator);
        audio->enumerator = NULL;
    }
    CoUninitialize();
    return 0;
}

static void destroy_audio(oos_audio *audio) {
    if (!audio) return;
    if (audio->thread) {
        SetEvent(audio->stop_event);
        SetEvent(audio->data_event);
        WaitForSingleObject(audio->thread, INFINITE);
        CloseHandle(audio->thread);
    }
    EnterCriticalSection(&audio->lock);
    clear_queue_locked(audio);
    audio->state = OOS_STATE_CLOSED;
    LeaveCriticalSection(&audio->lock);
    if (audio->ready_event) CloseHandle(audio->ready_event);
    if (audio->stop_event) CloseHandle(audio->stop_event);
    if (audio->changed_event) CloseHandle(audio->changed_event);
    if (audio->sample_event) CloseHandle(audio->sample_event);
    if (audio->keepalive_event) CloseHandle(audio->keepalive_event);
    if (audio->data_event) CloseHandle(audio->data_event);
    DeleteCriticalSection(&audio->lock);
    free(audio);
}

int oos_audio_open(oos_audio **out, char *err, size_t err_len) {
    oos_audio *audio;
    int state;
    int status;
    if (!out) return OOS_AUDIO_ERROR;
    *out = NULL;
    audio = (oos_audio *)calloc(1, sizeof(*audio));
    if (!audio) {
        copy_error(err, err_len, "out of memory");
        return OOS_AUDIO_ERROR;
    }
    InitializeCriticalSection(&audio->lock);
    audio->state = OOS_STATE_STARTING;
    audio->ready_event = CreateEventW(NULL, TRUE, FALSE, NULL);
    audio->stop_event = CreateEventW(NULL, TRUE, FALSE, NULL);
    audio->changed_event = CreateEventW(NULL, FALSE, FALSE, NULL);
    audio->sample_event = CreateEventW(NULL, FALSE, FALSE, NULL);
    audio->keepalive_event = CreateEventW(NULL, FALSE, FALSE, NULL);
    audio->data_event = CreateEventW(NULL, TRUE, FALSE, NULL);
    if (!audio->ready_event || !audio->stop_event || !audio->changed_event ||
        !audio->sample_event || !audio->keepalive_event || !audio->data_event) {
        copy_error(err, err_len, "CreateEvent failed");
        destroy_audio(audio);
        return OOS_AUDIO_ERROR;
    }
    audio->thread = CreateThread(NULL, 0, capture_thread, audio, 0, NULL);
    if (!audio->thread) {
        copy_error(err, err_len, "CreateThread failed");
        destroy_audio(audio);
        return OOS_AUDIO_ERROR;
    }
    WaitForSingleObject(audio->ready_event, INFINITE);
    EnterCriticalSection(&audio->lock);
    state = audio->state;
    status = audio->status;
    copy_error(err, err_len, audio->error);
    LeaveCriticalSection(&audio->lock);
    if (state != OOS_STATE_RUNNING) {
        destroy_audio(audio);
        return status;
    }
    *out = audio;
    return OOS_AUDIO_OK;
}

int oos_audio_read(oos_audio *audio, uint32_t timeout_ms,
                   oos_audio_packet *packet, char *err, size_t err_len) {
    HANDLE waits[2];
    DWORD wait;
    oos_queued_packet *queued = NULL;
    int state;
    int status;
    if (!audio || !packet) return OOS_AUDIO_ERROR;
    memset(packet, 0, sizeof(*packet));
    waits[0] = audio->stop_event;
    waits[1] = audio->data_event;
    wait = WaitForMultipleObjects(2, waits, FALSE, timeout_ms);
    if (wait == WAIT_TIMEOUT) return OOS_AUDIO_TIMEOUT;

    EnterCriticalSection(&audio->lock);
    if (wait == WAIT_OBJECT_0 + 1 && audio->head) {
        queued = audio->head;
        audio->head = queued->next;
        if (!audio->head) audio->tail = NULL;
        audio->queued--;
        if (!audio->head) ResetEvent(audio->data_event);
    }
    state = audio->state;
    status = audio->status;
    copy_error(err, err_len, audio->error);
    LeaveCriticalSection(&audio->lock);

    if (queued) {
        *packet = queued->packet;
        free(queued);
        return OOS_AUDIO_OK;
    }
    if (state == OOS_STATE_FAILED) return status;
    if (wait == WAIT_OBJECT_0 || state == OOS_STATE_CLOSED) return OOS_AUDIO_CLOSED;
    return OOS_AUDIO_TIMEOUT;
}

void oos_audio_packet_release(oos_audio_packet *packet) {
    if (!packet) return;
    free(packet->data);
    memset(packet, 0, sizeof(*packet));
}

void oos_audio_get_format(oos_audio *audio, oos_audio_format *format) {
    if (!audio || !format) return;
    EnterCriticalSection(&audio->lock);
    *format = audio->format;
    LeaveCriticalSection(&audio->lock);
}

uint64_t oos_audio_clock_100ns(void) {
    LARGE_INTEGER counter;
    LARGE_INTEGER frequency;
    uint64_t seconds;
    uint64_t remainder;
    QueryPerformanceCounter(&counter);
    QueryPerformanceFrequency(&frequency);
    seconds = (uint64_t)(counter.QuadPart / frequency.QuadPart);
    remainder = (uint64_t)(counter.QuadPart % frequency.QuadPart);
    return seconds * 10000000ULL +
           (remainder * 10000000ULL) / (uint64_t)frequency.QuadPart;
}

void oos_audio_close(oos_audio *audio) {
    destroy_audio(audio);
}
