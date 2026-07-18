#!/usr/bin/env python3
"""live-ninja wake-word training job (plan.md M6, FR-K03).

Runs inside the containers/wakeword-train image on AWS Batch / Fargate ARM64.
Trains a small openWakeWord-style detector head for a user-supplied phrase and
publishes per-platform artifacts to the wake-words S3 bucket.

Pipeline
--------
1. Synthesize positive clips of the target phrase with piper-sample-generator
   (multi-speaker LibriTTS-R checkpoint, speaker SLERP mixing for diversity).
2. Synthesize adversarial negative clips (bundled everyday phrases plus
   near-misses derived from the target phrase) with the same TTS, and generate
   procedural noise/silence negatives.
3. Augment (gain, synthetic room impulse response, additive noise at random
   SNR) and place every clip on a fixed 2.0 s canvas: 32 000 samples @ 16 kHz
   -> exactly 197 mel frames -> exactly 16 speech-embedding frames, matching
   the [1, 16, 96] detector input the web runtime feeds
   (web/static/js/wakeword.mjs) and openWakeWord's own models.
4. Compute embeddings with openWakeWord's bundled melspectrogram +
   speech-embedding ONNX models (AudioFeatures, ONNX inference path only).
5. Train a small dense detector head (PyTorch), pick a decision threshold from
   held-out validation scores.
6. Export ONNX fp32, dynamically quantize to int8 (onnxruntime), verify the
   int8 model against the torch scores with onnxruntime.
7. Upload per platform to  s3://$WAKEWORDS_BUCKET/wakewords/<wwId>/<platform>/
       model.onnx        (int8 — the artifact clients download, per the M6
                          locked decision: int8 .onnx for BOTH web and android;
                          contracts/wakeword-manifest.md lists .tflite for
                          android, but the android ModelManager consumes onnx —
                          the android format tag "oww-onnx-android-v1" is a new
                          additive (engine, platform, format) combination)
       model_fp32.onnx   (debug/QA copy)
       manifest.json     {phrase, engine, files:{onnx:{key,sha256,sizeBytes}},
                          trainedAt, ...}  — uploaded LAST, so its presence
                          implies the model objects exist.

Failure contract
----------------
Any failure (including the 18-minute self-deadline and SIGTERM from the Batch
20-minute timeout) uploads  wakewords/<wwId>/failed.json  with a reason and
exits 1. The Batch state-change watcher / status Lambda owns flipping the
WAKEWORD#<wwId> DynamoDB item to failed|ready — this container is S3-only and
never touches DynamoDB.

Modes
-----
--self-check   No TTS, no S3: verifies every import (torch, piper stack,
               openwakeword), that the baked model files exist, and runs
               train -> export -> quantize -> onnxruntime verify on synthetic
               feature data. Fast enough for QEMU in GitHub Actions.
--smoke-test   Full pipeline with tiny counts; uploads only if
               WAKEWORDS_BUCKET is set. Run on a native arm64 host.
"""

import argparse
import hashlib
import json
import logging
import os
import random
import re
import shutil
import signal
import sys
import tempfile
import time
import wave
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

log = logging.getLogger("wakeword-train")

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
SAMPLE_RATE = 16000
# 2.0 s canvas: ceil(32000/160 - 3) = 197 mel frames; embedding windows of 76
# mel frames with hop 8 -> (197-76)//8 + 1 = 16 embedding frames exactly.
TARGET_SAMPLES = 32000
EMB_FRAMES = 16
EMB_DIM = 96

# Locked M6 decision: openWakeWord is the only server-side training path and
# custom models ship as int8 onnx for web AND android (no esp32 custom models —
# esp32 selects among curated builtin WakeNet models only).
SUPPORTED_PLATFORMS = ("web", "android")
FORMAT_TAGS = {"web": "oww-onnx-web-v1", "android": "oww-onnx-android-v1"}

PSG_DIR = Path(os.environ.get("PSG_DIR", "/opt/piper-sample-generator"))
PSG_MODEL = Path(
    os.environ.get("PIPER_MODEL", str(PSG_DIR / "models" / "en_US-libritts_r-medium.pt"))
)
NEGATIVE_PHRASES_FILE = Path(__file__).parent / "negative_phrases.txt"

WWID_RE = re.compile(r"^[A-Za-z0-9_-]{1,64}$")
PHRASE_RE = re.compile(r"^[a-z0-9' -]{2,64}$")


def env_int(name: str, default: int) -> int:
    raw = os.environ.get(name, "")
    return int(raw) if raw.strip() else default


class DeadlineExceeded(Exception):
    pass


class Terminated(Exception):
    pass


class Deadline:
    """Hard self-deadline (default 18 min, < the Batch 20-min job timeout) so
    the job always gets to upload its failed.json marker instead of being
    killed opaquely."""

    def __init__(self, seconds: float):
        self.t0 = time.monotonic()
        self.limit = seconds
        self.phase = "init"

    def remaining(self) -> float:
        return self.limit - (time.monotonic() - self.t0)

    def check(self) -> None:
        if self.remaining() <= 0:
            raise DeadlineExceeded(self.phase)


# ---------------------------------------------------------------------------
# Audio helpers (all numpy, 16 kHz mono)
# ---------------------------------------------------------------------------
def load_wav_int16(path: Path):
    with wave.open(str(path), "rb") as w:
        if w.getframerate() != SAMPLE_RATE or w.getnchannels() != 1 or w.getsampwidth() != 2:
            raise ValueError(f"unexpected wav format in {path}")
        return np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16)


def white_noise(n, rng):
    return rng.standard_normal(n).astype(np.float32)


def pink_noise(n, rng):
    # Spectral shaping: scale FFT magnitudes by 1/sqrt(f).
    spec = np.fft.rfft(rng.standard_normal(n))
    f = np.arange(len(spec), dtype=np.float64)
    f[0] = 1.0
    x = np.fft.irfft(spec / np.sqrt(f), n=n)
    return (x / (np.abs(x).max() + 1e-9)).astype(np.float32)


def brown_noise(n, rng):
    x = np.cumsum(rng.standard_normal(n))
    x -= x.mean()
    return (x / (np.abs(x).max() + 1e-9)).astype(np.float32)


def band_noise(n, rng):
    # Speech-band-ish noise: pink noise crudely band-limited via FFT mask.
    spec = np.fft.rfft(rng.standard_normal(n))
    freqs = np.fft.rfftfreq(n, d=1.0 / SAMPLE_RATE)
    mask = ((freqs > 150) & (freqs < 4000)).astype(np.float64)
    f = np.maximum(freqs / 100.0, 1.0)
    x = np.fft.irfft(spec * mask / np.sqrt(f), n=n)
    return (x / (np.abs(x).max() + 1e-9)).astype(np.float32)


NOISE_FNS = (white_noise, pink_noise, brown_noise, band_noise)


def synthetic_rir(rng):
    """Cheap synthetic room impulse response: exponentially decaying noise tail."""
    n = int(0.15 * SAMPLE_RATE)
    t = np.arange(n, dtype=np.float32)
    rir = rng.standard_normal(n).astype(np.float32) * np.exp(-t / (0.03 * SAMPLE_RATE))
    rir[0] = 1.0
    return rir / (np.abs(rir).sum() * 0.05 + 1e-9)


def place_on_canvas(clip_f32, rng):
    """End-align the clip on the fixed canvas with small jitter, so the phrase
    finishes inside the final embedding window (streaming clients score the
    most recent 16 frames)."""
    canvas = np.zeros(TARGET_SAMPLES, dtype=np.float32)
    clip = clip_f32[-TARGET_SAMPLES:]
    jitter = int(rng.integers(0, 1600))  # up to 100 ms from the end
    start = max(0, TARGET_SAMPLES - len(clip) - jitter)
    canvas[start:start + len(clip)] = clip[: TARGET_SAMPLES - start]
    return canvas


def augment(canvas_f32, rng):
    from scipy.signal import fftconvolve  # local import keeps --help/arg errors fast

    y = canvas_f32 * float(rng.uniform(0.4, 1.1))
    if rng.random() < 0.35:
        rir = synthetic_rir(rng)
        y = fftconvolve(y, rir)[: len(canvas_f32)].astype(np.float32)
        peak = np.abs(y).max()
        if peak > 1.0:
            y = y / peak
    snr_db = float(rng.uniform(5.0, 25.0))
    noise = NOISE_FNS[int(rng.integers(0, len(NOISE_FNS)))](len(y), rng)
    sig_rms = float(np.sqrt(np.mean(y**2)) + 1e-9)
    noise_rms = float(np.sqrt(np.mean(noise**2)) + 1e-9)
    y = y + noise * (sig_rms / noise_rms) * (10.0 ** (-snr_db / 20.0))
    return np.clip(y, -1.0, 1.0)


def to_int16(x_f32):
    return (np.clip(x_f32, -1.0, 1.0) * 32767.0).astype(np.int16)


# ---------------------------------------------------------------------------
# TTS synthesis (piper-sample-generator v2.0.0 — signature verified at pin)
# ---------------------------------------------------------------------------
def tts_generate(texts, total, outdir, deadline, reserve_s, tts_batch, chunk, min_required):
    """Generate up to `total` clips, in chunks so we can stop early if the time
    budget shrinks; returns float32 clips in [-1, 1]. generate_samples writes
    VAD-trimmed 16 kHz mono int16 wavs named <idx>.wav per call, so each chunk
    gets its own directory."""
    sys.path.insert(0, str(PSG_DIR))
    from generate_samples import generate_samples  # noqa: E402 (image PYTHONPATH)

    clips = []
    produced = 0
    chunk_no = 0
    while produced < total:
        deadline.check()
        if deadline.remaining() < reserve_s:
            log.warning(
                "time budget low (%.0fs left); stopping TTS at %d/%d clips",
                deadline.remaining(), produced, total,
            )
            break
        k = min(chunk, total - produced)
        d = Path(outdir) / f"chunk{chunk_no}"
        d.mkdir(parents=True, exist_ok=True)
        batch_texts = [texts[(produced + i) % len(texts)] for i in range(k)]
        generate_samples(
            text=batch_texts,
            output_dir=str(d),
            max_samples=k,
            model=str(PSG_MODEL),
            batch_size=tts_batch,
            length_scales=[0.7, 1.0, 1.3],
            noise_scales=[0.667],
            noise_scale_ws=[0.8],
        )
        for f in sorted(d.glob("*.wav")):
            pcm = load_wav_int16(f)
            if len(pcm) >= 4000:  # drop clips VAD-trimmed to near-nothing
                clips.append(pcm.astype(np.float32) / 32768.0)
        produced += k
        chunk_no += 1
    if len(clips) < min_required:
        raise RuntimeError(
            f"TTS produced only {len(clips)} usable clips (< {min_required} minimum)"
        )
    return clips


def near_miss_phrases(phrase):
    """Hard negatives derived from the target phrase: single words, the phrase
    minus its last word, and a shuffled word order."""
    words = phrase.split()
    out = [w for w in words if len(w) >= 3]
    if len(words) >= 2:
        out.append(" ".join(words[:-1]))
        out.append(" ".join(reversed(words)))
    return out


def load_negative_phrases():
    lines = NEGATIVE_PHRASES_FILE.read_text(encoding="utf-8").splitlines()
    return [ln.strip() for ln in lines if ln.strip() and not ln.strip().startswith("#")]


# ---------------------------------------------------------------------------
# Features
# ---------------------------------------------------------------------------
def compute_embeddings(int16_canvases, deadline):
    """openWakeWord melspectrogram + speech-embedding, ONNX path only (the
    image carries no tflite runtime). Input (N, 32000) int16 -> (N, 16, 96)."""
    from openwakeword.utils import AudioFeatures  # noqa: E402

    deadline.check()
    feats = AudioFeatures(inference_framework="onnx", ncpu=max(1, (os.cpu_count() or 2) - 1))
    x = np.stack(int16_canvases)
    emb = feats.embed_clips(x, batch_size=64)
    if emb.shape[1] < EMB_FRAMES or emb.shape[2] != EMB_DIM:
        raise RuntimeError(f"unexpected embedding shape {emb.shape}")
    return emb[:, -EMB_FRAMES:, :].astype(np.float32)


# ---------------------------------------------------------------------------
# Model
# ---------------------------------------------------------------------------
def build_head():
    import torch.nn as nn  # noqa: E402

    # Same family as openWakeWord's shipped DNN detectors: a small dense head
    # over the [16, 96] embedding window, sigmoid score. wakeword.mjs feeds
    # float32 [1, 16, 96] and reads output[0] — input/output names are looked
    # up dynamically, so only the shapes are contractual.
    return nn.Sequential(
        nn.Flatten(),
        nn.Linear(EMB_FRAMES * EMB_DIM, 128),
        nn.ReLU(),
        nn.Linear(128, 64),
        nn.ReLU(),
        nn.Linear(64, 1),
        nn.Sigmoid(),
    )


def train_head(X, y, epochs, deadline, seed=1234):
    import torch  # noqa: E402

    torch.manual_seed(seed)
    rng = np.random.default_rng(seed)
    n = len(X)
    idx = rng.permutation(n)
    n_val = max(8, int(n * 0.15))
    val_idx, tr_idx = idx[:n_val], idx[n_val:]

    Xt = torch.from_numpy(X[tr_idx])
    yt = torch.from_numpy(y[tr_idx]).float().unsqueeze(1)
    Xv = torch.from_numpy(X[val_idx])
    yv = y[val_idx]

    model = build_head()
    opt = torch.optim.Adam(model.parameters(), lr=1e-3)
    # Class-imbalance weight: negatives usually outnumber positives.
    n_pos = max(1.0, float(y[tr_idx].sum()))
    n_neg = max(1.0, float(len(tr_idx) - y[tr_idx].sum()))
    w_pos = (n_neg / n_pos) ** 0.5

    best_val = float("inf")
    best_state = None
    stale = 0
    bs = 256
    for epoch in range(epochs):
        deadline.check()
        model.train()
        perm = torch.randperm(len(Xt))
        for i in range(0, len(Xt), bs):
            b = perm[i:i + bs]
            opt.zero_grad()
            out = model(Xt[b])
            w = torch.where(yt[b] > 0.5, torch.full_like(out, w_pos), torch.ones_like(out))
            loss = torch.nn.functional.binary_cross_entropy(out, yt[b], weight=w)
            loss.backward()
            opt.step()
        model.eval()
        with torch.no_grad():
            val_out = model(Xv).squeeze(1)
            vw = torch.where(
                torch.from_numpy(yv).float() > 0.5,
                torch.full_like(val_out, w_pos),
                torch.ones_like(val_out),
            )
            val_loss = float(
                torch.nn.functional.binary_cross_entropy(
                    val_out, torch.from_numpy(yv).float(), weight=vw
                )
            )
        if val_loss < best_val - 1e-4:
            best_val = val_loss
            best_state = {k: v.clone() for k, v in model.state_dict().items()}
            stale = 0
        else:
            stale += 1
            if stale >= 8:
                log.info("early stop at epoch %d (best val loss %.4f)", epoch, best_val)
                break
    if best_state is not None:
        model.load_state_dict(best_state)
    model.eval()

    with torch.no_grad():
        val_scores = model(Xv).squeeze(1).numpy()
    pos_scores = val_scores[yv > 0.5]
    neg_scores = val_scores[yv < 0.5]

    # Threshold: sit above nearly all validation negatives, clamped to the
    # range clients expect (openWakeWord default is 0.5).
    if len(neg_scores):
        threshold = float(np.clip(np.percentile(neg_scores, 99.5) + 0.07, 0.5, 0.9))
    else:
        threshold = 0.5
    metrics = {
        "valRecallAtThreshold": round(float(np.mean(pos_scores >= threshold)), 4)
        if len(pos_scores) else None,
        "valFalsePositiveRate": round(float(np.mean(neg_scores >= threshold)), 4)
        if len(neg_scores) else None,
        "valPositives": int(len(pos_scores)),
        "valNegatives": int(len(neg_scores)),
    }
    return model, threshold, metrics


def gemm_to_matmul(model_path):
    """Rewrite Gemm nodes as MatMul + Add in place.

    torch exports nn.Linear as Gemm, but onnxruntime's DYNAMIC quantization
    registry (IntegerOpsRegistry, onnxruntime 1.18.1) only covers
    Conv/MatMul/Attention/LSTM — Gemm nodes would silently stay fp32 and
    quantize_dynamic would be a no-op. MatMul (with an initializer weight)
    becomes DynamicQuantizeLinear + MatMulInteger, which is what we want.
    """
    import onnx  # noqa: E402
    from onnx import helper, numpy_helper  # noqa: E402

    m = onnx.load(str(model_path))
    inits = {i.name: i for i in m.graph.initializer}
    new_nodes = []
    for node in m.graph.node:
        if node.op_type != "Gemm":
            new_nodes.append(node)
            continue
        attrs = {a.name: helper.get_attribute_value(a) for a in node.attribute}
        if (attrs.get("alpha", 1.0) != 1.0 or attrs.get("beta", 1.0) != 1.0
                or attrs.get("transA", 0) != 0):
            raise RuntimeError(f"unexpected Gemm attributes {attrs} in {node.name}")
        a_name, w_name = node.input[0], node.input[1]
        if w_name not in inits:
            raise RuntimeError(f"Gemm weight {w_name} is not an initializer")
        if attrs.get("transB", 0) == 1:
            w = numpy_helper.to_array(inits[w_name]).T.copy()
            w_name_t = w_name + "_T"
            m.graph.initializer.append(numpy_helper.from_array(w, w_name_t))
            w_name = w_name_t
        mm_out = node.output[0] + "_matmul"
        new_nodes.append(helper.make_node("MatMul", [a_name, w_name], [mm_out],
                                          name=node.name + "_matmul"))
        if len(node.input) > 2:  # bias
            new_nodes.append(helper.make_node("Add", [mm_out, node.input[2]],
                                              [node.output[0]], name=node.name + "_bias"))
        else:
            new_nodes[-1].output[0] = node.output[0]
    del m.graph.node[:]
    m.graph.node.extend(new_nodes)
    onnx.checker.check_model(m)
    onnx.save(m, str(model_path))


def export_onnx(model, outdir, deadline):
    """fp32 export -> int8 dynamic quantization -> onnxruntime verification.
    Returns (fp32_path, int8_path)."""
    import onnx  # noqa: E402
    import torch  # noqa: E402
    import onnxruntime as ort  # noqa: E402
    from onnxruntime.quantization import QuantType, quantize_dynamic  # noqa: E402

    deadline.check()
    fp32 = Path(outdir) / "model_fp32.onnx"
    int8 = Path(outdir) / "model.onnx"
    dummy = torch.zeros(1, EMB_FRAMES, EMB_DIM)
    # opset 13: only Gemm/MatMul/Add/Relu/Sigmoid/Flatten — maximally
    # compatible with onnxruntime-web (WASM) and onnxruntime-android.
    torch.onnx.export(
        model, dummy, str(fp32),
        input_names=["embeddings"], output_names=["score"],
        opset_version=13, do_constant_folding=True,
    )
    gemm_to_matmul(fp32)
    quantize_dynamic(str(fp32), str(int8), weight_type=QuantType.QInt8)
    # Prove quantization actually happened (see gemm_to_matmul docstring).
    q = onnx.load(str(int8))
    if not any(n.op_type == "MatMulInteger" for n in q.graph.node):
        raise RuntimeError("quantize_dynamic produced no MatMulInteger nodes")

    # Verify: the int8 model must load in vanilla onnxruntime, produce a [1,1]
    # score in [0,1], and track the torch model closely on random probes.
    sess = ort.InferenceSession(str(int8), providers=["CPUExecutionProvider"])
    in_name = sess.get_inputs()[0].name
    probes = np.random.default_rng(7).standard_normal((32, EMB_FRAMES, EMB_DIM)).astype(np.float32)
    with torch.no_grad():
        torch_scores = model(torch.from_numpy(probes)).squeeze(1).numpy()
    ort_scores = np.array(
        [sess.run(None, {in_name: p[None]})[0].reshape(-1)[0] for p in probes]
    )
    if not (np.all(ort_scores >= -1e-6) and np.all(ort_scores <= 1 + 1e-6)):
        raise RuntimeError("int8 model produced scores outside [0, 1]")
    drift = float(np.abs(ort_scores - torch_scores).mean())
    if drift > 0.2:
        raise RuntimeError(f"int8 quantization drift too high: {drift:.3f}")
    log.info("int8 verification OK (mean |drift| %.4f)", drift)
    return fp32, int8


# ---------------------------------------------------------------------------
# S3
# ---------------------------------------------------------------------------
def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def iso_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def upload_artifacts(bucket, ww_id, user_id, phrase, platforms, fp32, int8, threshold, metrics):
    import boto3  # noqa: E402

    s3 = boto3.client("s3")
    int8_sha, int8_size = sha256_file(int8), int8.stat().st_size
    fp32_sha, fp32_size = sha256_file(fp32), fp32.stat().st_size
    trained_at = iso_now()
    for platform in platforms:
        base = f"wakewords/{ww_id}/{platform}"
        s3.upload_file(
            str(int8), bucket, f"{base}/model.onnx",
            ExtraArgs={"ContentType": "application/octet-stream"},
        )
        s3.upload_file(
            str(fp32), bucket, f"{base}/model_fp32.onnx",
            ExtraArgs={"ContentType": "application/octet-stream"},
        )
        manifest = {
            # Required keys (M6 contract; consumed by the wake-word model
            # manifest endpoint — contracts/wakeword-manifest.md):
            "phrase": phrase,
            "engine": "openwakeword",
            "files": {
                "onnx": {"key": f"{base}/model.onnx", "sha256": int8_sha, "sizeBytes": int8_size},
                # Additive extra: fp32 debug copy.
                "onnxFp32": {
                    "key": f"{base}/model_fp32.onnx", "sha256": fp32_sha, "sizeBytes": fp32_size,
                },
            },
            "trainedAt": trained_at,
            # Additive extras for the serving Lambda / clients:
            "wwId": ww_id,
            "userId": user_id,
            "platform": platform,
            "format": FORMAT_TAGS[platform],
            "recommendedThreshold": threshold,
            "metrics": metrics,
        }
        # Manifest last: its presence implies the model objects above exist.
        s3.put_object(
            Bucket=bucket,
            Key=f"{base}/manifest.json",
            Body=json.dumps(manifest, indent=2).encode(),
            ContentType="application/json",
        )
        log.info("uploaded s3://%s/%s/{model.onnx,model_fp32.onnx,manifest.json}", bucket, base)
    # Clear any stale failure marker from a previous attempt of this wwId.
    try:
        s3.delete_object(Bucket=bucket, Key=f"wakewords/{ww_id}/failed.json")
    except Exception:  # noqa: BLE001 — best-effort cleanup only
        pass


def upload_failure_marker(bucket, ww_id, user_id, phrase, reason, phase):
    if not bucket:
        log.error("no WAKEWORDS_BUCKET set; failure marker not uploaded (%s)", reason)
        return
    try:
        import boto3  # noqa: E402

        boto3.client("s3").put_object(
            Bucket=bucket,
            Key=f"wakewords/{ww_id}/failed.json",
            Body=json.dumps(
                {
                    "wwId": ww_id,
                    "userId": user_id,
                    "phrase": phrase,
                    "reason": reason,
                    "phase": phase,
                    "failedAt": iso_now(),
                },
                indent=2,
            ).encode(),
            ContentType="application/json",
        )
        log.info("uploaded failure marker wakewords/%s/failed.json (%s)", ww_id, reason)
    except Exception:  # noqa: BLE001
        log.exception("failed to upload failure marker")


# ---------------------------------------------------------------------------
# Modes
# ---------------------------------------------------------------------------
def run_self_check(deadline):
    """Build-time verification: every runtime import resolves, the baked model
    files exist, and train -> export -> quantize -> ort-verify works on
    synthetic feature data. No TTS, no S3 — QEMU-friendly."""
    log.info("self-check: verifying imports and baked assets")
    import torch  # noqa: F401,E402
    import openwakeword  # noqa: E402

    res = Path(openwakeword.__file__).parent / "resources" / "models"
    for f in ("melspectrogram.onnx", "embedding_model.onnx"):
        if not (res / f).exists():
            raise RuntimeError(f"baked openWakeWord feature model missing: {f}")
    sys.path.insert(0, str(PSG_DIR))
    import generate_samples  # noqa: F401,E402 — pulls torch/torchaudio/webrtcvad/piper stack

    if not PSG_MODEL.exists():
        raise RuntimeError(f"baked TTS checkpoint missing: {PSG_MODEL}")
    if not (Path(str(PSG_MODEL) + ".json")).exists():
        raise RuntimeError("TTS checkpoint config (.pt.json) missing")

    log.info("self-check: training on synthetic separable features")
    rng = np.random.default_rng(0)
    n = 256
    X = rng.standard_normal((n, EMB_FRAMES, EMB_DIM)).astype(np.float32)
    y = (np.arange(n) % 2).astype(np.float32)
    X[y > 0.5] += 0.75  # separable shift
    model, threshold, metrics = train_head(X, y, epochs=10, deadline=deadline)
    with tempfile.TemporaryDirectory() as td:
        export_onnx(model, td, deadline)
    log.info("self-check OK (threshold=%.2f metrics=%s)", threshold, metrics)
    print("SELF-CHECK OK")


def run_training(args, deadline):
    bucket = os.environ.get("WAKEWORDS_BUCKET", "")
    if not args.smoke_test and not bucket:
        raise RuntimeError("WAKEWORDS_BUCKET env var is required")

    smoke = args.smoke_test
    n_pos = 12 if smoke else env_int("N_POSITIVE", 240)
    n_neg_tts = 16 if smoke else env_int("N_NEGATIVE_TTS", 160)
    n_noise = 8 if smoke else env_int("N_NOISE", 48)
    aug_per_pos = 1 if smoke else env_int("AUG_PER_POS", 2)
    epochs = 8 if smoke else env_int("EPOCHS", 60)
    tts_batch = 4 if smoke else env_int("TTS_BATCH", 16)
    tts_chunk = 8 if smoke else env_int("TTS_CHUNK", 80)
    # Time reserved after TTS for features + training + export + upload.
    reserve_s = float(env_int("RESERVE_SECONDS", 240))

    rng = np.random.default_rng(20260717)
    workdir = Path(tempfile.mkdtemp(prefix="wakeword-train-"))
    try:
        deadline.phase = "tts-positives"
        log.info("phase %s: %d clips of %r", deadline.phase, n_pos, args.phrase)
        pos_clips = tts_generate(
            [args.phrase], n_pos, workdir / "pos", deadline, reserve_s,
            tts_batch, tts_chunk, min_required=8 if smoke else 40,
        )

        deadline.phase = "tts-negatives"
        neg_texts = load_negative_phrases() + near_miss_phrases(args.phrase)
        rng.shuffle(neg_texts)
        log.info("phase %s: %d clips from %d phrases", deadline.phase, n_neg_tts, len(neg_texts))
        neg_clips = tts_generate(
            neg_texts, n_neg_tts, workdir / "neg", deadline, reserve_s,
            tts_batch, tts_chunk, min_required=8 if smoke else 40,
        )

        deadline.phase = "augment"
        canvases, labels = [], []
        for clip in pos_clips:
            base = place_on_canvas(clip, rng)
            canvases.append(to_int16(base))
            labels.append(1.0)
            for _ in range(aug_per_pos):
                canvases.append(to_int16(augment(base, rng)))
                labels.append(1.0)
        for clip in neg_clips:
            base = place_on_canvas(clip, rng)
            canvases.append(to_int16(base))
            labels.append(0.0)
            canvases.append(to_int16(augment(base, rng)))
            labels.append(0.0)
        for i in range(n_noise):
            fn = NOISE_FNS[i % len(NOISE_FNS)]
            level = float(rng.uniform(0.001, 0.5))  # from near-silence to loud
            canvases.append(to_int16(fn(TARGET_SAMPLES, rng) * level))
            labels.append(0.0)
        y = np.array(labels, dtype=np.float32)
        log.info("dataset: %d clips (%d positive / %d negative)",
                 len(canvases), int(y.sum()), int(len(y) - y.sum()))

        deadline.phase = "features"
        X = compute_embeddings(canvases, deadline)

        deadline.phase = "train"
        model, threshold, metrics = train_head(X, y, epochs, deadline)
        log.info("trained: threshold=%.2f metrics=%s", threshold, metrics)

        deadline.phase = "export"
        fp32, int8 = export_onnx(model, workdir, deadline)

        deadline.phase = "upload"
        if bucket:
            upload_artifacts(
                bucket, args.ww_id, args.user_id, args.phrase,
                args.platforms, fp32, int8, threshold, metrics,
            )
        else:
            log.info("smoke test without WAKEWORDS_BUCKET: skipping upload "
                     "(artifacts in %s)", workdir)
            print(f"SMOKE-TEST OK model={int8} threshold={threshold}")
            return  # keep workdir for inspection
        log.info("done in %.0fs", time.monotonic() - deadline.t0)
    finally:
        if bucket:
            shutil.rmtree(workdir, ignore_errors=True)


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
def parse_args(argv):
    p = argparse.ArgumentParser(description="live-ninja wake-word trainer (openWakeWord head)")
    p.add_argument("--phrase", required=True, help="target wake phrase, e.g. 'hey ninja'")
    p.add_argument("--ww-id", required=True, dest="ww_id", help="wake-word id (S3 key segment)")
    p.add_argument("--user-id", required=True, dest="user_id", help="owning user id")
    p.add_argument(
        "--platforms", default="web,android",
        help="comma-separated subset of: web,android (esp32 custom models are "
             "unsupported — curated builtin WakeNet only, per M6 locked decision)",
    )
    p.add_argument("--self-check", action="store_true", help="verify image without TTS/S3")
    p.add_argument("--smoke-test", action="store_true", help="tiny full-pipeline run")
    args = p.parse_args(argv)

    if not WWID_RE.match(args.ww_id):
        p.error("--ww-id must match ^[A-Za-z0-9_-]{1,64}$")
    phrase = " ".join(args.phrase.lower().split())
    if not PHRASE_RE.match(phrase) or not (1 <= len(phrase.split()) <= 8):
        p.error("--phrase must be 1-8 words of [a-z0-9'-] (semantic validation — "
                "profanity/collision — is the API's pre-submit job)")
    args.phrase = phrase
    platforms = tuple(s.strip() for s in args.platforms.split(",") if s.strip())
    bad = [s for s in platforms if s not in SUPPORTED_PLATFORMS]
    if bad or not platforms:
        p.error(f"--platforms entries must be in {SUPPORTED_PLATFORMS}; got {bad or 'none'}")
    args.platforms = platforms
    return args


def main(argv=None):
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        stream=sys.stdout,
    )
    args = parse_args(argv if argv is not None else sys.argv[1:])
    random.seed(20260717)

    deadline = Deadline(float(env_int("DEADLINE_SECONDS", 18 * 60)))
    if hasattr(signal, "SIGALRM"):  # POSIX (the container); absent on Windows dev boxes
        def on_alarm(_sig, _frm):
            raise DeadlineExceeded(deadline.phase)

        def on_term(_sig, _frm):
            raise Terminated(deadline.phase)

        signal.signal(signal.SIGALRM, on_alarm)
        signal.signal(signal.SIGTERM, on_term)
        signal.alarm(int(deadline.limit))

    bucket = os.environ.get("WAKEWORDS_BUCKET", "")
    try:
        if args.self_check:
            run_self_check(deadline)
        else:
            run_training(args, deadline)
        return 0
    except DeadlineExceeded as e:
        log.error("self-deadline exceeded during phase %s", e)
        if not args.self_check:
            upload_failure_marker(bucket, args.ww_id, args.user_id, args.phrase,
                                  "deadline_exceeded", str(e))
        return 1
    except Terminated as e:
        log.error("SIGTERM during phase %s (Batch timeout/eviction)", e)
        if not args.self_check:
            upload_failure_marker(bucket, args.ww_id, args.user_id, args.phrase,
                                  "terminated", str(e))
        return 1
    except Exception as e:  # noqa: BLE001 — single failure funnel: marker + exit 1
        log.exception("training failed during phase %s", deadline.phase)
        if not args.self_check:
            upload_failure_marker(bucket, args.ww_id, args.user_id, args.phrase,
                                  f"{type(e).__name__}: {e}"[:512], deadline.phase)
        return 1


if __name__ == "__main__":
    sys.exit(main())
