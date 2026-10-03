#ifndef OOS_WASAPI_H
#define OOS_WASAPI_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct oos_audio oos_audio;

enum {
    OOS_AUDIO_OK = 1,
    OOS_AUDIO_TIMEOUT = 0,
    OOS_AUDIO_ERROR = -1,
    OOS_AUDIO_NO_DEVICE = -2,
    OOS_AUDIO_CLOSED = -3
};

enum {
    OOS_SAMPLE_UNKNOWN = 0,
    OOS_SAMPLE_PCM = 1,
    OOS_SAMPLE_FLOAT = 2
};

typedef struct oos_audio_format {
    uint32_t sample_rate;
    uint16_t channels;
    uint16_t sample_format;
    uint16_t bits_per_sample;
    uint16_t valid_bits_per_sample;
    uint32_t channel_mask;
    uint16_t bytes_per_frame;
} oos_audio_format;

typedef struct oos_audio_packet {
    uint8_t *data;
    uint32_t bytes;
    uint32_t frames;
    uint64_t qpc_100ns;
    uint8_t timestamp_valid;
    oos_audio_format format;
} oos_audio_packet;

int oos_audio_open(oos_audio **out, char *err, size_t err_len);
int oos_audio_read(oos_audio *audio, uint32_t timeout_ms,
                   oos_audio_packet *packet, char *err, size_t err_len);
void oos_audio_packet_release(oos_audio_packet *packet);
void oos_audio_get_format(oos_audio *audio, oos_audio_format *format);
uint64_t oos_audio_clock_100ns(void);
void oos_audio_close(oos_audio *audio);

/* Shared by the live client and hardware-independent format tests. */
int oos_audio_convert_format(const void *input, uint32_t input_len,
                             void *output, uint32_t output_capacity,
                             uint32_t *output_len,
                             oos_audio_format *format,
                             char *err, size_t err_len);

#ifdef __cplusplus
}
#endif

#endif
