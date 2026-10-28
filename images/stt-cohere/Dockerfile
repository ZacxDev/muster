# stt-cohere: the GPU speech-to-text server behind muster's dictation
# (docs/dictation.md). Published as ghcr.io/<owner>/muster-stt-cohere:<version>
# by .github/workflows/ci.yml on a release tag. linux/amd64 only: the CUDA
# PyTorch base has no arm64 build.
#
#   docker build -t muster-stt-cohere images/stt-cohere
#   docker run --gpus all -e STT_API_TOKEN=… -e HF_TOKEN=… -p 8000:8000 muster-stt-cohere
#
# Needs an NVIDIA GPU with about 6 GB free VRAM and HF_TOKEN from a Hugging Face
# account that accepted the model's license (the model is gated).
FROM pytorch/pytorch:2.11.0-cuda12.8-cudnn9-runtime@sha256:eee11b3b3872a8c838e35ef48f08b2d5def2080902c7f666831310ca1a0ef2be

# 🔴 ffmpeg IS A HARD RUNTIME DEPENDENCY: app.py's decode_audio shells out to it
# for every request, and the base image ships none.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
# Torch is preinstalled in the base image's system python; install into that
# same interpreter.
COPY requirements.txt .
RUN python -m pip install --break-system-packages --no-cache-dir -r requirements.txt
COPY app.py .

EXPOSE 8000
HEALTHCHECK --interval=15s --timeout=10s --start-period=30m --retries=5 \
    CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/healthz', timeout=5).status == 200 else 1)"]
CMD ["python", "-m", "uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
