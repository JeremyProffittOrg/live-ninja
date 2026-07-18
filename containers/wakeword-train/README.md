# wakeword-train — custom wake-word training container (M6, FR-K03)

Trains an openWakeWord-style custom detector head for a user-supplied phrase and
publishes per-platform model artifacts to the wake-words S3 bucket. Runs on
**AWS Batch / Fargate ARM64** (concurrency ≤ 2 via the compute environment,
20-minute Batch job timeout, ≤ 3 jobs/day/user enforced pre-submit by the API).
The container adds its own **18-minute self-deadline** so it always gets to
upload a graceful failure marker instead of being killed opaquely.

openWakeWord is the only server-side training engine (locked M6 decision —
Porcupine needs a Picovoice account and is not implemented server-side; the
catalog just flags engine availability).

## How it works

1. **Synthetic positives** — [piper-sample-generator](https://github.com/rhasspy/piper-sample-generator)
   (pinned tag `v2.0.0`, multi-speaker LibriTTS-R medium checkpoint, speaker
   SLERP mixing) speaks the target phrase a few hundred times.
2. **Negatives** — the same TTS speaks `negative_phrases.txt` (67 everyday
   phrases) plus near-misses derived from the target phrase (single words,
   dropped last word, shuffled order), and procedural white/pink/brown/band
   noise + near-silence clips are added.
3. **Augment + canvas** — random gain, synthetic room impulse response,
   additive noise at 5–25 dB SNR; every clip is end-aligned (with jitter) on a
   fixed 2.0 s canvas. 32 000 samples @ 16 kHz → 197 mel frames → **exactly 16
   speech-embedding frames**, the `[1, 16, 96]` detector input that
   `web/static/js/wakeword.mjs` (and openWakeWord's own models) use.
4. **Features** — openWakeWord's bundled melspectrogram + Google
   speech-embedding ONNX models (`AudioFeatures`, ONNX inference path only;
   the image carries no tflite runtime).
5. **Train** — small dense head (Flatten → 128 → 64 → 1, sigmoid) in PyTorch,
   class-weighted BCE, early stopping, 15 % held-out validation. The decision
   threshold is picked to sit above ~all validation negatives (clamped to
   0.5–0.9) and reported in the manifest as `recommendedThreshold`.
6. **Export** — ONNX fp32 (opset 13; Gemm nodes rewritten to MatMul+Add — see
   *int8 details* below), dynamic int8 quantization via onnxruntime, then a
   hard verification: the int8 model must load in vanilla onnxruntime, contain
   real `MatMulInteger` nodes, keep scores in [0, 1], and track the torch
   scores within 0.2 mean absolute drift.
7. **Upload** — per platform (`web`, `android`) to
   `s3://$WAKEWORDS_BUCKET/wakewords/<wwId>/<platform>/`:

   | object | notes |
   |---|---|
   | `model.onnx` | int8 — the artifact clients download (~210 KB) |
   | `model_fp32.onnx` | debug/QA copy (~1.6 MB) |
   | `manifest.json` | uploaded **last**, so its presence implies the models exist |

   `manifest.json` (required keys per the M6 contract, plus additive extras):

   ```jsonc
   {
     "phrase": "hey ninja",
     "engine": "openwakeword",
     "files": {
       "onnx":     { "key": "wakewords/<wwId>/web/model.onnx", "sha256": "…", "sizeBytes": 214445 },
       "onnxFp32": { "key": "wakewords/<wwId>/web/model_fp32.onnx", "sha256": "…", "sizeBytes": 1646433 }
     },
     "trainedAt": "2026-07-17T21:00:00Z",
     "wwId": "…", "userId": "…", "platform": "web",
     "format": "oww-onnx-web-v1",
     "recommendedThreshold": 0.62,
     "metrics": { "valRecallAtThreshold": 0.97, "valFalsePositiveRate": 0.0, "valPositives": 108, "valNegatives": 55 }
   }
   ```

**Failure contract** — any failure (including deadline and SIGTERM) uploads
`wakewords/<wwId>/failed.json` `{wwId, userId, phrase, reason, phase, failedAt}`
and exits 1; a later successful retry deletes the stale marker. The Batch
state-change watcher owns flipping the `WAKEWORD#<wwId>` DynamoDB item to
`ready`/`failed` and sending the SES "ready" email — **this container is
S3-only and never touches DynamoDB.**

### Platform notes (deliberate deviations)

- **android gets int8 `.onnx`, not `.tflite`** (locked M6 decision; the Android
  `ModelManager` consumes onnx). `contracts/wakeword-manifest.md` lists
  `oww-tflite-android-v1` — the tag used here, `oww-onnx-android-v1`, is a new
  *additive* (engine, platform, format) combination, which the contract
  explicitly permits.
- **esp32 is rejected** at argument parsing: custom models on esp32 are
  unsupported; the device selects among curated builtin WakeNet models via the
  config shadow (honest capability flag, not a fake).

## Job interface

```
python train.py --phrase "hey ninja" --ww-id <wwId> --user-id <uid> [--platforms web,android]
```

Batch job definition command:
`["--phrase","Ref::phrase","--ww-id","Ref::wwId","--user-id","Ref::userId","--platforms","Ref::platforms"]`

Environment:

| var | default | meaning |
|---|---|---|
| `WAKEWORDS_BUCKET` | *(required)* | destination bucket (`live-ninja-wakewords-…`) |
| `DEADLINE_SECONDS` | `1080` (18 min) | hard self-deadline (keep < the 20-min Batch timeout) |
| `RESERVE_SECONDS` | `240` | time reserved after TTS for features/train/export/upload; TTS stops early (gracefully, if ≥ 40 clips exist) when the remaining budget drops below this |
| `N_POSITIVE` | `240` | positive TTS clips |
| `N_NEGATIVE_TTS` | `160` | adversarial TTS negative clips |
| `N_NOISE` | `48` | procedural noise/silence negatives |
| `AUG_PER_POS` | `2` | augmented copies per positive (dataset ≈ `N_POSITIVE×(1+AUG)` pos / `N_NEGATIVE_TTS×2 + N_NOISE` neg) |
| `EPOCHS` | `60` | max training epochs (early stopping usually ends sooner) |
| `TTS_BATCH` | `16` | TTS batch size (memory-bound; 16 fits 8 GB comfortably) |
| `TTS_CHUNK` | `80` | clips per `generate_samples` call (deadline checks between chunks; each chunk reloads the ~³⁰⁰ MB checkpoint, ~10 s) |

AWS credentials come from the Fargate task role (needs `s3:PutObject` +
`s3:DeleteObject` on `wakewords/*` in the bucket). Nothing is baked into the
image.

## Size & time expectations

- **Image**: target < 1.5 GB. Rough layer budget: python-slim base ~130 MB,
  torch cpu aarch64 ~450 MB, scipy/sklearn/numpy ~180 MB, onnxruntime ~40 MB,
  LibriTTS-R checkpoint ~300 MB, everything else small. Verify on first CI
  build with `docker images` and trim (`N_POSITIVE` of layers: the checkpoint
  is the only real lever) if over.
- **Job** (Fargate 4 vCPU / 8 GB ARM64): TTS ≈ 6–10 min (dominant), feature
  extraction ≈ 1–2 min, training < 1 min, export/verify/upload ≈ seconds.
  Comfortably inside the 18-minute self-deadline; if TTS runs slow the job
  degrades gracefully (fewer clips) rather than dying.

## Local build & test

Docker is required to build (it was **not available on the authoring machine** —
see *Verification status*). The image is linux/arm64 only:

```sh
docker buildx build --platform linux/arm64 -t wakeword-train containers/wakeword-train

# 1) Image self-check — no TTS, no S3; verifies every import, the baked model
#    files, and the full train→export→quantize→onnxruntime-verify path on
#    synthetic features. Works (slowly) under QEMU on an x86 host:
docker run --rm wakeword-train --self-check --phrase "hey ninja" --ww-id t --user-id t
#    → prints "SELF-CHECK OK", exit 0

# 2) Full-pipeline smoke test — tiny clip counts, real TTS. Run on a NATIVE
#    arm64 host (QEMU makes the TTS painfully slow). Without WAKEWORDS_BUCKET
#    it skips upload and prints the local model path:
docker run --rm wakeword-train --smoke-test --phrase "hey ninja" --ww-id t --user-id t

# 3) Real run (needs AWS creds; on Batch these come from the task role):
docker run --rm -e WAKEWORDS_BUCKET=live-ninja-wakewords-759775734231 \
  -e AWS_REGION=us-east-1 \
  -v ~/.aws:/root/.aws:ro \
  wakeword-train --phrase "hey ninja" --ww-id ww123 --user-id u1
```

## CI (GitHub Actions) — the sanctioned build path

Per deploy.md there are no local deploys: a **separate GitHub Actions job,
path-filtered to `containers/**`**, builds and pushes this image to the ECR
repository created in `template.yaml`. Recommended job shape:

1. `docker/setup-qemu-action` + `docker/setup-buildx-action`
2. `docker/build-push-action` with `platforms: linux/arm64`, pushing to ECR
   (OIDC role, `aws-actions/amazon-ecr-login`)
3. **Gate the push on the self-check**: build to the local docker store first,
   `docker run --rm <image> --self-check --phrase "hey ninja" --ww-id ci --user-id ci`
   (a few minutes under QEMU — it imports the full torch/piper/openwakeword
   stack and runs the real export path), then push.
4. Optional deeper gate: run step 3's smoke-test variant on an
   `ubuntu-24.04-arm` runner for native-speed TTS.

## int8 details (why the Gemm rewrite exists)

torch's (pinned 2.3.1) exporter emits `nn.Linear` as ONNX `Gemm`, but
onnxruntime 1.18.1's **dynamic** quantization registry (`IntegerOpsRegistry`)
only covers `Conv/MatMul/Attention/LSTM` — `quantize_dynamic` on a Gemm graph
is a silent no-op. `train.py:gemm_to_matmul()` rewrites `Gemm(transB=1)` →
`MatMul(x, Wᵀ) + Add(bias)` in the exported graph (numerically exact —
verified), after which quantization produces real
`DynamicQuantizeLinear + MatMulInteger` nodes (~210 KB vs ~1.6 MB fp32). The
export then *asserts* `MatMulInteger` exists, so a future dependency bump that
breaks quantization fails the job loudly instead of shipping fp32 as "int8".
All ops used (`MatMulInteger`, `DynamicQuantizeLinear`, `Reshape/Relu/Sigmoid`)
are supported by onnxruntime-web's WASM build and onnxruntime-android.

## Verification status (authored without local Docker)

Docker was not available on the authoring machine, so instead of a local image
build the following were verified directly:

- **Pinned wheels**: every dependency in `requirements.txt` was checked against
  PyPI for a prebuilt `cp311` manylinux **aarch64** wheel (listed in that
  file) — the image needs no compiler.
- **Upstream APIs at the pinned versions**: `generate_samples()`'s signature,
  its 302-redirect model URL, output naming/VAD-trimming behavior, and its
  vendored `piper_train.vits` (no pytorch-lightning on the inference path)
  were read from the actual `v2.0.0` tag; `openwakeword==0.6.0`'s
  `AudioFeatures` (int16 input, `(N, frames, 96)` output, ONNX path,
  import-time deps) and `download_models()` were read from the actual wheel.
- **The trainable core ran for real**: `train_head` → `export_onnx` →
  `gemm_to_matmul` → `quantize_dynamic` → onnxruntime verification executed
  locally end-to-end on synthetic features (pos mean score 0.999 / neg 0.132
  through the int8 model, input `[1,16,96]`), plus the audio/augment helpers
  and argument validation.
- **Not yet exercised** (first CI build will): the Dockerfile itself, TTS
  inside the container, and `AudioFeatures` feature extraction — all pinned
  and API-verified as above; `--self-check` + `--smoke-test` in the CI job are
  the designed gate for exactly these.
