# stt-nemotron: the streaming speech-to-text server behind live meeting
# captions (README.md is the protocol; docs/design/meetings.md "Serving
# Nemotron" is the design). Published as
# ghcr.io/<owner>/muster-stt-nemotron:<version> by .github/workflows/ci.yml on
# a release tag. linux/amd64 only: the CUDA PyTorch base has no arm64 build.
#
#   docker build -t muster-stt-nemotron images/stt-nemotron
#   docker run --gpus all -e STT_API_TOKEN=… -v stt-nemotron-models:/models \
#     -p 8000:8000 muster-stt-nemotron
#
# 🔴 THE WEIGHTS ARE NOT IN THE IMAGE. They are fetched on FIRST START from
# Hugging Face (nvidia/nemotron-speech-streaming-en-0.6b, about 2.5 GB, not
# gated) at the commit pinned in nemo_engine.py, checked against the pinned
# sha256, and cached in HF_HOME=/models: mount a volume there, or every start
# downloads again. Not baked in because the image is public and the weights
# are under the NVIDIA Open Model License, which this image does not
# redistribute; the first start needs outbound HTTPS to huggingface.co.
#
# Needs an NVIDIA GPU with about 2 GiB free VRAM at 8 streams (measured,
# README.md).
FROM pytorch/pytorch:2.11.0-cuda12.8-cudnn9-runtime@sha256:eee11b3b3872a8c838e35ef48f08b2d5def2080902c7f666831310ca1a0ef2be

WORKDIR /app
# Torch is preinstalled in the base image's system python; install into that
# same interpreter.
COPY requirements.txt .
RUN python -m pip install --break-system-packages --no-cache-dir -r requirements.txt
COPY app.py nemo_engine.py ./

ENV HF_HOME=/models
VOLUME /models
EXPOSE 8000
# /readyz: 200 once the model is loaded and STT_API_TOKEN is set, so a
# compose `--wait` fails on a missing token. The server listens only after the
# model loads, and the first start includes the download: hence 30 minutes.
HEALTHCHECK --interval=15s --timeout=10s --start-period=30m --retries=5 \
    CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/readyz', timeout=5).status == 200 else 1)"]
CMD ["python", "app.py"]
