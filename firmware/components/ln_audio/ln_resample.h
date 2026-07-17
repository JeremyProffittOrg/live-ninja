/*
 * Live Ninja — small fixed-rate FIR resamplers (Q15).
 *  - x3 decimator  48 kHz -> 16 kHz (33-tap Hamming windowed-sinc, fc ~6.8 kHz)
 *  - x2 interpolator 24 kHz -> 48 kHz (31-tap windowed-sinc, fc ~10.5 kHz)
 * Streaming: each state keeps its own history so arbitrary block sizes work.
 */
#pragma once

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define LN_DEC3_TAPS 33
#define LN_ITP2_TAPS 31

typedef struct {
    int16_t hist[LN_DEC3_TAPS - 1];
    int phase; /* samples consumed modulo 3 */
} ln_dec3_t;

typedef struct {
    int16_t hist[LN_ITP2_TAPS - 1];
} ln_itp2_t;

void ln_dec3_reset(ln_dec3_t *s);
/**
 * Decimate 48k mono by 3. Returns number of output samples written
 * (<= in_samples/3 + 1). out must hold in_samples/3 + 1 samples.
 */
size_t ln_dec3_process(ln_dec3_t *s, const int16_t *in, size_t in_samples, int16_t *out);

void ln_itp2_reset(ln_itp2_t *s);
/**
 * Interpolate 24k mono by 2. Writes exactly 2*in_samples samples to out.
 */
size_t ln_itp2_process(ln_itp2_t *s, const int16_t *in, size_t in_samples, int16_t *out);

#ifdef __cplusplus
}
#endif
