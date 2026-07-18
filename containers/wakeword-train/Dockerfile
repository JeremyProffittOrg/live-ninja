# syntax=docker/dockerfile:1
#
# live-ninja wake-word training container (plan.md M6, FR-K03).
#
# linux/arm64 ONLY — runs on AWS Batch / Fargate ARM64 (concurrency <= 2, 20-min
# job timeout enforced by the compute environment / job definition; train.py adds
# its own 18-minute self-deadline with a graceful failed.json marker).
#
# Built and pushed to ECR by a dedicated GitHub Actions job path-filtered to
# containers/** (deploy.md: no local deploys). Local builds are for testing only:
#   docker buildx build --platform linux/arm64 -t wakeword-train .
#
# Image size budget: < 1.5 GB (see README.md for the breakdown; the TTS
# checkpoint is the single biggest layer).
FROM --platform=linux/arm64 python:3.11-slim-bookworm

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# ca-certificates: TLS to GitHub (build time) and S3 (run time). libgomp1:
# the OpenMP runtime torch/torchaudio's aarch64 wheels dlopen at import
# (slim-bookworm doesn't ship it — its absence killed the first real
# training run with "libgomp.so.1: cannot open shared object file").
# No compiler toolchain — every Python dep pins a prebuilt manylinux
# aarch64 wheel (verified per-package; see requirements.txt).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates libgomp1 \
 && rm -rf /var/lib/apt/lists/*

COPY requirements.txt /opt/wakeword-train/requirements.txt
RUN pip install -r /opt/wakeword-train/requirements.txt \
 # openwakeword 0.6.0's metadata hard-requires tflite-runtime on Linux, but
 # train.py only uses the ONNX path (AudioFeatures(inference_framework="onnx")).
 # Install without deps; its real import-time deps (numpy, scipy, scikit-learn,
 # onnxruntime, tqdm, requests) are already pinned in requirements.txt.
 && pip install --no-deps openwakeword==0.6.0

# piper-sample-generator, pinned at tag v2.0.0. The repo vendors piper_train.vits
# (the VITS classes needed to unpickle the TTS checkpoint below) — the inference
# path needs NO pytorch-lightning. Fetched via python/urllib because it follows
# GitHub's 302 redirect to the release CDN unconditionally (unlike some ADD
# implementations).
RUN python -c "import urllib.request; urllib.request.urlretrieve('https://github.com/rhasspy/piper-sample-generator/archive/refs/tags/v2.0.0.tar.gz', '/tmp/psg.tar.gz')" \
 && tar -xzf /tmp/psg.tar.gz -C /opt \
 && mv /opt/piper-sample-generator-2.0.0 /opt/piper-sample-generator \
 && rm /tmp/psg.tar.gz

# Multi-speaker TTS checkpoint (LibriTTS-R medium, 904 speakers) used to
# synthesize positive/negative training clips. Its .pt.json config ships in the
# repo tarball above. Baked into the image so a Batch job never downloads at
# run time (the 18-minute budget is for compute, not network).
RUN python -c "import urllib.request; urllib.request.urlretrieve('https://github.com/rhasspy/piper-sample-generator/releases/download/v2.0.0/en_US-libritts_r-medium.pt', '/opt/piper-sample-generator/models/en_US-libritts_r-medium.pt')"

# Bake the openWakeWord feature models (melspectrogram + Google speech-embedding
# ONNX, plus silero VAD) into site-packages. download_models() always fetches the
# feature models; the named model just satisfies its API. The .tflite copies are
# deleted — train.py runs the ONNX inference path exclusively.
RUN python -c "from openwakeword.utils import download_models; download_models(['hey_jarvis_v0.1'])" \
 && find /usr/local/lib/python3.11/site-packages/openwakeword/resources -name '*.tflite' -delete

COPY negative_phrases.txt train.py /opt/wakeword-train/

# generate_samples.py + vendored piper_train live at the repo root (not a
# package), so the repo dir itself goes on PYTHONPATH.
ENV PYTHONPATH=/opt/piper-sample-generator
WORKDIR /opt/wakeword-train

# The Batch job definition appends the job arguments, e.g.:
#   --phrase "Ref::phrase" --ww-id "Ref::wwId" --user-id "Ref::userId" \
#   --platforms "Ref::platforms"
# Config via env: WAKEWORDS_BUCKET (required for upload), AWS_REGION, plus the
# tuning knobs documented in README.md. AWS credentials come from the Fargate
# task role — never from the image.
ENTRYPOINT ["python", "/opt/wakeword-train/train.py"]
