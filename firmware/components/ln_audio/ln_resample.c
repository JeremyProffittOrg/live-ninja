#include "ln_resample.h"

#include <string.h>

/* 33-tap Hamming windowed-sinc lowpass, fc = 6.8 kHz @ 48 kHz, Q15, DC gain 1. */
static const int16_t s_dec3_coef[LN_DEC3_TAPS] = {
    52,    44,   -9,   -106, -177, -96,  189,  514,  522,  -49,   -1005,
    -1600, -916, 1453, 4914, 8020, 9268, 8020, 4914, 1453, -916,  -1600,
    -1005, -49,  522,  514,  189,  -96,  -177, -106, -9,   44,    52,
};

/* 31-tap windowed-sinc lowpass, fc = 10.5 kHz @ 48 kHz, Q15, gain 2 (for x2
 * zero-stuffed interpolation the passband gain must equal the upsample factor). */
static const int16_t s_itp2_coef[LN_ITP2_TAPS] = {
    109,   51,    -159, -206,  244,   596,   -180,  -1280, -341, 2187, 1781,
    -3120, -5264, 3825, 20216, 28617, 20216, 3825,  -5264, -3120, 1781, 2187,
    -341,  -1280, -180, 596,   244,   -206,  -159,  51,    109,
};

static inline int16_t sat16(int32_t v)
{
    if (v > 32767) {
        return 32767;
    }
    if (v < -32768) {
        return -32768;
    }
    return (int16_t)v;
}

void ln_dec3_reset(ln_dec3_t *s)
{
    memset(s, 0, sizeof(*s));
}

size_t ln_dec3_process(ln_dec3_t *s, const int16_t *in, size_t in_samples, int16_t *out)
{
    /* Work over a contiguous [history | input] view. */
    size_t out_n = 0;
    const int hist_n = LN_DEC3_TAPS - 1;

    /* For each input sample index i (global position hist_n + i), when the
     * post-decimation phase hits 0 emit a filtered sample centred on it. */
    for (size_t i = 0; i < in_samples; i++) {
        s->phase++;
        if (s->phase < 3) {
            continue;
        }
        s->phase = 0;
        /* newest sample involved: in[i]; need LN_DEC3_TAPS samples back. */
        int32_t acc = 0;
        for (int t = 0; t < LN_DEC3_TAPS; t++) {
            int32_t idx = (int32_t)i - t; /* index into `in`; negative -> history */
            int16_t x;
            if (idx >= 0) {
                x = in[idx];
            } else {
                x = s->hist[hist_n + idx]; /* idx in [-hist_n, -1] */
            }
            acc += (int32_t)x * s_dec3_coef[t];
        }
        out[out_n++] = sat16(acc >> 15);
    }

    /* Update history with the last hist_n samples of [old_hist | in]. */
    if (in_samples >= (size_t)hist_n) {
        memcpy(s->hist, &in[in_samples - hist_n], hist_n * sizeof(int16_t));
    } else {
        size_t keep = hist_n - in_samples;
        memmove(s->hist, &s->hist[in_samples], keep * sizeof(int16_t));
        memcpy(&s->hist[keep], in, in_samples * sizeof(int16_t));
    }
    return out_n;
}

void ln_itp2_reset(ln_itp2_t *s)
{
    memset(s, 0, sizeof(*s));
}

size_t ln_itp2_process(ln_itp2_t *s, const int16_t *in, size_t in_samples, int16_t *out)
{
    /* Polyphase x2: zero-stuffed stream convolved with s_itp2_coef.
     * Output sample 2i   uses taps 0,2,4,... (even phase)
     * Output sample 2i+1 uses taps 1,3,5,... (odd phase)
     * against input samples ending at in[i]. */
    const int hist_n = LN_ITP2_TAPS - 1;
    size_t out_n = 0;

    for (size_t i = 0; i < in_samples; i++) {
        for (int ph = 0; ph < 2; ph++) {
            int32_t acc = 0;
            for (int t = ph; t < LN_ITP2_TAPS; t += 2) {
                /* y[2i+ph] = sum_k h[2k+ph] * in[i-k]  (t = 2k+ph) */
                int32_t idx = (int32_t)i - (t - ph) / 2;
                int16_t x;
                if (idx >= 0) {
                    x = in[idx];
                } else if (idx >= -hist_n) {
                    x = s->hist[hist_n + idx];
                } else {
                    x = 0;
                }
                acc += (int32_t)x * s_itp2_coef[t];
            }
            out[out_n++] = sat16(acc >> 15);
        }
    }

    if (in_samples >= (size_t)hist_n) {
        memcpy(s->hist, &in[in_samples - hist_n], hist_n * sizeof(int16_t));
    } else {
        size_t keep = hist_n - in_samples;
        memmove(s->hist, &s->hist[in_samples], keep * sizeof(int16_t));
        memcpy(&s->hist[keep], in, in_samples * sizeof(int16_t));
    }
    return out_n;
}
