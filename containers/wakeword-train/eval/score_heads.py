"""Definitive wake-head scorer: known-text 16 kHz clips, per-head target mapping.

Pipeline mirrors android/.../wake/OwwPipeline.kt exactly (1280-sample chunks, 480 left context,
mel x/10+2, 76-frame mel window -> 96-d embedding, 16-embedding window -> head), buffers prefilled
from the models' silence outputs as reset() does.

Two corrections over the earlier harness:
  1. Every clip is 16 kHz. The previous set mixed in 22050 Hz files, which the pipeline fed through
     without resampling -- that made a KNOWN-GOOD model score 0.097 on its own phrase.
  2. Targets are declared per head, so "loudest non-target" is that head's own worst confusion.

"Adjacent" clips (those that CONTAIN the target phrase, e.g. "hey live ninjas" for "hey live
ninja") are reported separately and excluded from the bar -- firing on those is not a defect, and
including them would not be comparable to the numbers already recorded in plan.md.
"""
import glob
import os
import sys
import wave

import numpy as np
import onnxruntime as ort

SP = os.path.dirname(os.path.abspath(__file__))
ASSETS = r"c:/dev/live-ninja/android/app/src/main/assets/wakeword"

CHUNK, CTX, MEL_BINS, MEL_WIN, EMB_DIM, EMB_WIN = 1280, 480, 32, 76, 96, 16
BAR_TARGET, BAR_NEG = 0.8, 0.4

# head-name prefix -> (target slug, adjacent slugs excluded from the bar)
HEADS = {
    "liveninja": ("tgt_hey_live_ninja",
                  ("nm_hey_live_ninjas", "nm_live_ninja", "nm_okay_live_ninja", "nm_the_live_ninja")),
    "sunshine": ("tgt_hey_sunshine", ()),
    "automatica": ("tgt_hey_automatica", ()),
    "hey-jarvis": ("tgt_hey_jarvis", ()),
}


def head_spec(name):
    for k, v in HEADS.items():
        if name.startswith(k):
            return v
    return (None, ())


so = ort.SessionOptions()
so.intra_op_num_threads = 1
so.inter_op_num_threads = 1
mel_s = ort.InferenceSession(os.path.join(ASSETS, "melspectrogram.onnx"), so, providers=["CPUExecutionProvider"])
emb_s = ort.InferenceSession(os.path.join(ASSETS, "embedding_model.onnx"), so, providers=["CPUExecutionProvider"])


def run_mel(samples):
    out = mel_s.run(None, {mel_s.get_inputs()[0].name: samples.reshape(1, -1).astype(np.float32)})[0]
    return out.reshape(-1, MEL_BINS) / 10.0 + 2.0


def run_emb(win):
    x = win.reshape(1, MEL_WIN, MEL_BINS, 1).astype(np.float32)
    return emb_s.run(None, {emb_s.get_inputs()[0].name: x})[0].reshape(-1)[:EMB_DIM]


def score_clip(head_s, path):
    return score_clip_pcm(head_s, load_pcm(path))


def score_clip_pcm(head_s, pcm):
    silent_mel = run_mel(np.zeros(CTX + CHUNK, dtype=np.float32))[-1]
    mel_buf = np.tile(silent_mel, (MEL_WIN, 1))
    emb_buf = np.tile(run_emb(mel_buf), (EMB_WIN, 1))

    tail = np.zeros(CTX, dtype=np.float32)
    best = 0.0
    for i in range(0, len(pcm) - CHUNK + 1, CHUNK):
        inp = np.concatenate([tail, pcm[i:i + CHUNK]])
        tail = inp[CHUNK:].copy()
        frames = run_mel(inp)[-8:]
        mel_buf = np.vstack([mel_buf[len(frames):], frames])
        emb_buf = np.vstack([emb_buf[1:], run_emb(mel_buf).reshape(1, -1)])
        x = emb_buf.reshape(1, EMB_WIN, EMB_DIM).astype(np.float32)
        best = max(best, float(head_s.run(None, {head_s.get_inputs()[0].name: x})[0].reshape(-1)[0]))
    return best


clips = []  # (path, voice, slug)
for p in sorted(glob.glob(os.path.join(SP, "clips", "*.wav"))):
    base = os.path.basename(p)[:-4]
    voice, slug = base.split("__", 1)
    clips.append((p, voice, slug))

if not clips:
    sys.exit("no clips found in ./clips — run gen_clips.ps1 first (see README.md)")

heads = {"hey-jarvis": os.path.join(ASSETS, "hey_jarvis_v0.1.onnx")}
for p in sorted(glob.glob(os.path.join(SP, "heads", "*.onnx"))):
    heads[os.path.basename(p)[:-5]] = p

def warp(pcm, f):
    """Resample by factor f (>1 = faster/higher), the axis train.py's time_warp augments."""
    n_out = max(1, int(len(pcm) / f))
    return np.interp(
        np.linspace(0, len(pcm) - 1, n_out), np.arange(len(pcm)), pcm
    ).astype(np.float32)


def load_pcm(path):
    with wave.open(path, "rb") as w:
        assert w.getframerate() == 16000, f"{path} is {w.getframerate()}Hz, pipeline needs 16000"
        return np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16).astype(np.float32)


WARP_FACTORS = (0.85, 0.95, 1.00, 1.05, 1.10, 1.15, 1.20, 1.25, 1.30, 1.40)
# A head must hold up across the rates real speakers actually use. A single score at 1.00x cannot
# tell a robust head from one whose acceptance region happens to contain the test clip: on
# 2026-08-09 hey-live-ninja scored 0.001 at native and 0.775 at 1.30x, i.e. it HAD learned the
# phrase but into a manifold too thin to land in.
WARP_BAND = (0.90, 1.30)


def warp_report(heads_sel):
    print("factor:" + "".join(f"{f:>7.2f}" for f in WARP_FACTORS))
    for name, path in heads_sel:
        tgt, _ = head_spec(name)
        hs = ort.InferenceSession(path, so, providers=["CPUExecutionProvider"])
        for p, v, s in clips:
            if s != tgt:
                continue
            pcm = load_pcm(p)
            row = [score_clip_pcm(hs, warp(pcm, f)) for f in WARP_FACTORS]
            inband = [r for r, f in zip(row, WARP_FACTORS) if WARP_BAND[0] <= f <= WARP_BAND[1]]
            ok = min(inband) >= BAR_TARGET
            print(f"{name}/{v:<6s}" + "".join(f"{r:7.3f}" for r in row)
                  + f"   in-band min {min(inband):.3f} {'OK' if ok else 'THIN'}")


only = [a for a in sys.argv[1:] if not a.startswith("-")] or None
want_warp = "--warp" in sys.argv
summary = []

for name, path in heads.items():
    if only and not any(o in name for o in only):
        continue
    tgt, adjacent = head_spec(name)
    hs = ort.InferenceSession(path, so, providers=["CPUExecutionProvider"])
    rows = [(score_clip(hs, p), v, s) for p, v, s in clips]

    tvals = [(b, v) for b, v, s in rows if s == tgt]
    advals = [(b, v, s) for b, v, s in rows if s in adjacent]
    nvals = [(b, v, s) for b, v, s in rows if s != tgt and s not in adjacent]

    print(f"\n=== {name} ===  target='{tgt}'")
    for b, v in sorted(tvals, reverse=True):
        print(f"  TARGET     {v:6s} {tgt:24s} {b:.3f}")
    print("  -- loudest non-targets --")
    for b, v, s in sorted(nvals, reverse=True)[:6]:
        print(f"             {v:6s} {s:24s} {b:.3f}")
    if advals:
        print("  -- adjacent (contain the phrase; excluded from bar) --")
        for b, v, s in sorted(advals, reverse=True)[:4]:
            print(f"             {v:6s} {s:24s} {b:.3f}")

    worst_t = min(b for b, _ in tvals)
    mean_t = sum(b for b, _ in tvals) / len(tvals)
    top_n, top_v, top_s = max(nvals)
    ok = worst_t >= BAR_TARGET and top_n <= BAR_NEG
    print(f"  -> weakest target {worst_t:.3f} (mean {mean_t:.3f})   loudest non-target {top_n:.3f} ({top_v} {top_s})")
    print(f"  -> BAR target>={BAR_TARGET} & non-target<={BAR_NEG}: {'PASS' if ok else 'FAIL'}")
    summary.append((name, worst_t, mean_t, top_n, top_s, ok))

if want_warp:
    print("\n\n============== RATE TOLERANCE (peak score vs resampling factor) ==============")
    warp_report([(n, p) for n, p in heads.items() if not only or any(o in n for o in only)])
    print(f"in-band = {WARP_BAND[0]}x..{WARP_BAND[1]}x; THIN means the head only works at rates "
          f"a real speaker may not hit")

print("\n\n===================== SUMMARY =====================")
print(f"{'model':22s} {'tgt(min)':>8s} {'tgt(avg)':>8s} {'loud-neg':>8s}  {'worst confusion':22s} verdict")
for name, wt, mt, tn, ts, ok in summary:
    print(f"{name:22s} {wt:8.3f} {mt:8.3f} {tn:8.3f}  {ts:22s} {'PASS' if ok else 'FAIL'}")
