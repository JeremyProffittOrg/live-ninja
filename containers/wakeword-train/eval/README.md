# Wake-model evaluation harness

Scores a trained detector head against recorded speech **on the PC, in about two minutes, with no
device involved**. This is the definition-of-done check for a training round (plan.md §7.4).

The bar, per phrase:

```
weakest target >= 0.8   AND   loudest non-target <= 0.4
```

`hey-jarvis`, upstream openWakeWord's own model, scores **0.998 / 0.221** and is the reference.

## Running it

```powershell
# once — writes clips/ and clips.tsv next to the scripts (needs Windows SAPI)
powershell -ExecutionPolicy Bypass -File gen_clips.ps1

# fetch the heads to score
aws s3 cp s3://live-ninja-wakewords-759775734231/wakewords/<wwId>/android/model.onnx heads/<name>.onnx

python score_heads.py            # every head in heads/
python score_heads.py liveninja  # only heads whose name contains "liveninja"
```

`score_heads.py` reads the mel + embedding models from
`android/app/src/main/assets/wakeword/`, so it replicates what the phone actually runs.

## Why it is written the way it is

It mirrors `android/.../wake/OwwPipeline.kt` exactly — 1280-sample chunks with 480 samples of left
context, mel post-transform `x/10+2`, a 76-frame mel window to one 96-d embedding, a 16-embedding
window to the head, and buffers prefilled from the models' own silence outputs as `reset()` does.
A change to `OwwPipeline.kt` must be mirrored here or the numbers stop meaning anything.

Three traps this harness exists to close, each of which produced a confidently wrong answer on
2026-08-08/09:

1. **Sample rate.** The pipeline reads raw PCM and never resamples, so a 22050 Hz clip is fed at
   the wrong rate and scores near zero. That made a known-working model look broken. `score_heads.py`
   asserts 16 kHz rather than trusting the filename.
2. **Weak negatives.** A clip set with no phonetically close near-miss passes a bad model.
   `hey-automatica` measured 0.343 against a set whose hardest negative was "hey banana"; against
   "hey america" it fires at **0.996**. `gen_clips.ps1` generates near-misses that vary one *sound*,
   not one word.
3. **Per-head targets.** Treating every `pos_*` clip as a target hides a model firing hard on
   another phrase's positive. Targets are declared per head in `HEADS`.

Clips that *contain* the target phrase ("hey live ninjas" for "hey live ninja") are reported
separately as `adjacent` and excluded from the bar — firing on those is not a defect.

## The out-of-distribution caveat, and its control

These clips are Windows SAPI while the models train on piper, so they are out of distribution and
absolute target numbers understate real performance. **Before blaming OOD for a low target, check
what a known-good head from the same trainer scores on the same clips.** `hey-automatica` scores
0.991 here, so a model this pipeline produces *can* score 0.99 on SAPI audio — which is how
round 2's 0.001 was shown to be a real failure to learn rather than a measurement artifact.

False-positive *direction* is reliable regardless.

## Adding a phrase

Add it to `$phrases` in `gen_clips.ps1` and, if it is a new target, to `HEADS` in
`score_heads.py`. Keep the `tgt_` / `nm_` / `neg_` / `dx_` prefixes: `tgt_` is a target, `nm_` a
near-miss, `neg_` an ordinary negative, `dx_` a one-off diagnostic.
