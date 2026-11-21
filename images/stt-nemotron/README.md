# stt-nemotron

A streaming speech-to-text server for live meeting captions: one websocket per
audio track, NVIDIA **Nemotron Speech Streaming en 0.6B** (a cache-aware
FastConformer RNNT) run by NeMo in **bf16**, with every live stream decoded in
**one batched model step** per 560 ms chunk. The design is
[docs/design/meetings.md, "Serving Nemotron"](../../docs/design/meetings.md#serving-nemotron);
this file is the **protocol contract** the meetings plugin's `nemotron` engine
is built against.

| file | what |
|---|---|
| `app.py` | the websocket server, auth, capacity, the protocol, the decode loop (model-agnostic) |
| `nemo_engine.py` | the model: NeMo's batched cache-aware streaming pipeline, the pinned weights |
| `test_app.py` | the protocol tests, on CPU with a stub model (`python3 test_app.py`) |
| `measure.py` | the GPU measurement and live checks against a running server |

## Protocol

### Endpoint and authentication

```
GET /v1/stream          (websocket upgrade)
Authorization: Bearer <STT_API_TOKEN>
```

Every refusal is an **HTTP status on the upgrade request**, never an accepted
socket that is then closed. A client therefore tells "refused" (an HTTP status,
no websocket) from "broken" (a close after the upgrade). Checked in this order:

| status | when | body |
|---|---|---|
| `503` | the server has no `STT_API_TOKEN` configured | `{"error": "..."}` |
| `401` | `Authorization` is missing, is not `Bearer`, or the token differs (compared in constant time) | `{"error": "..."}` |
| `503` + `Retry-After: <STT_RETRY_AFTER>` | the model is not loaded yet | `{"error": "..."}` |
| `503` + `Retry-After: <STT_RETRY_AFTER>` | **at capacity**: `STT_MAX_STREAMS` streams are open | `{"error": "at capacity (N streams)"}` |
| `101` | otherwise: the stream is open and holds one of the `STT_MAX_STREAMS` slots until it closes | |

An unauthenticated client at capacity gets `401`, not `503`: the capacity of
the server is not shown to a client without the token. A refused upgrade takes
no slot. A slot is freed when its socket closes, however it closes. Streams
over the cap are refused, never queued. `Retry-After` is in seconds (default
30); the plugin waits `max(30 s, Retry-After)` before trying again.

### Client → server

- **Audio:** binary frames, **16 kHz mono signed 16-bit little-endian PCM,
  exactly 80 ms each: 1280 samples, 2560 bytes.** The last frame of a stream is
  zero-padded to 1280 samples. Audio must be sent at (about) real time or
  faster; there is no timestamp in a frame: the server counts samples.
- **`{"type": "flush"}`** (a text frame): the end of an utterance (the client's
  VAD decided). The server answers with one `final`.
- **`{"type": "end", "samples": N}`** (a text frame): the track ended. `N` is
  the **true** number of samples sent, padding excluded, so it must satisfy
  `total - 1280 < N <= total` where `total` is 1280 × the frames sent (or
  `N = 0` with no frames). The server answers with one `final` and closes.

### Server → client (text frames, JSON)

```json
{"type": "partial", "text": "so the next thing", "start_ms": 1120, "end_ms": 3360}
{"type": "final",   "text": "so the next thing is the budget.", "start_ms": 1120, "end_ms": 4900}
{"type": "error",   "code": "bad_frame", "message": "..."}
```

- **Times are audio time**: milliseconds of samples from the start of the
  stream (`samples × 1000 // 16000`), never wall time. A stall in delivery
  does not move them.
- **A segment** is the audio from one `flush` (or the stream start) to the
  next `flush` (or `end`). `start_ms` is where the segment began; it includes
  any leading silence (the server does not trim to the first word).
- **`partial`**: sent after a model step when the segment's text changed, and
  only when it is non-empty. `text` is the **whole segment so far** (it
  replaces the previous partial; it is not a delta). `end_ms` is how far the
  model has decoded, a multiple of 560 ms, so up to ~560 ms behind the audio
  sent. Partials are never sent for a segment whose samples are all one value
  (below). The `end_ms` of a partial sent just before `end` may include up to
  80 ms of the last frame's padding; a `final`'s never does.
- **`final`**: exactly **one per `flush` and one per `end`**, in order, even
  when `text` is empty (so a client pairs them by count). `text` is everything
  decoded for the segment. For a `flush`, `end_ms` is the sample count received
  when the flush arrived; for `end`, it is `N`. The model decodes in 560 ms
  chunks, so the last **< 560 ms before a `flush`** is decoded after it, and
  words in that tail land in the **next** segment. The plugin flushes about
  700 ms into silence, so that tail is silence; the `final` arrives within
  milliseconds of the `flush` (measured below). `end` pads and decodes the tail,
  so the `end` final carries the last words.
- **Constant signal:** a segment in which every sample has the same value (a
  dead capture, e.g. bit-exact zeros) yields `text: ""` and no partials,
  whatever the model produced: the model invents words on such input. This is
  not an energy gate; quiet speech still varies. The last frame's padding does
  not count against it.
- After the `end` final the server closes with **1000**.

### Errors and close codes

Each error is one `error` frame, then a close:

| code | close | cause |
|---|---|---|
| `bad_frame` | 1003 | a binary frame that is not exactly 2560 bytes |
| `bad_message` | 1003 | a text frame that is not valid JSON or not `flush`/`end` |
| `bad_end` | 1003 | `end.samples` not an integer within the last frame (above) |
| `overloaded` | 1013 | this stream's undecoded audio passed `STT_MAX_BACKLOG_SECONDS` (default 30): the GPU is not keeping up |
| `internal` | 1011 | the model step failed |

None of these is expected in normal operation; the plugin treats each as a
mid-stream drop (reconnect, then fall back). The server sends **no pings**
(and requires no pongs); detect a dead peer by your own read loop. Messages
over 64 KiB are refused by the websocket layer (uvicorn's `ws_max_size`;
configured, not tested here).

### Health

| path | answers |
|---|---|
| `GET /healthz` | `200 {"ok": true}` while the process serves (liveness) |
| `GET /readyz` | `200 {"ok": true, "streams": n, "max_streams": N, "load_seconds": s}` once the model is loaded and a token is set; `503` otherwise. **Stays 200 at capacity**: a full server must keep answering `503` + `Retry-After` to new streams, which it cannot do once removed from its Service. |

The server listens only after the model has loaded.

## Configuration

| variable | default | |
|---|---|---|
| `STT_API_TOKEN` | none (**required**) | the bearer; without it every upgrade and `/readyz` answer 503 |
| `STT_MAX_STREAMS` | 8 | concurrent streams, and the batch size and slot count inside NeMo |
| `STT_RETRY_AFTER` | 30 | seconds, in the capacity `503` |
| `STT_MAX_BACKLOG_SECONDS` | 30 | undecoded audio one stream may queue |
| `STT_PORT` | 8000 | |
| `HF_HOME` | `/models` | where the weights are cached: mount a volume |

**Not configurable, on purpose:** the model (always the pinned download,
always hash-checked), the attention context (`70,6`: 560 ms chunks, the
benchmarked setting) and the precision (bf16, what the memory below was
measured in). They are constants in `nemo_engine.py`; changing one is a code
change and a re-measurement. `test_app.py` pins them.

**`STT_MAX_STREAMS` stays 8 until the fit beside `stt-cohere` is measured**
(an open follow-up; see [Measured](#measured)).

**Sample rate:** the server reads `preprocessor.sample_rate` from the loaded
`.nemo` and **refuses to start** unless it is 16000 (the wire format; the
server never resamples). The pinned model's is 16000.

## The model and the image

- **Weights: fetched on first start, not baked in.** `nemo_engine.py` pins
  `nvidia/nemotron-speech-streaming-en-0.6b` at commit
  `ebe59e5a817142986528bbbee5dba8db7b38ed50`, file
  `nemotron-speech-streaming-en-0.6b.nemo`, sha256
  `283638054c44f6794e74fe9af9048d78a6d9d6c058c12131856c7859a62ac9cd`. It is
  downloaded into `HF_HOME` (not gated, ~2.5 GB) and its hash checked before
  loading; a different file refuses to start. Not in the image because the
  image is public and the weights' licence (NVIDIA Open Model License) is the
  model's, not this repository's. The first start needs HTTPS to
  huggingface.co; later starts need nothing if `HF_HOME` is a volume.
- **Python packages:** every one pinned exactly in `requirements.txt`
  (NeMo 3.0.0) on a digest-pinned PyTorch 2.11 / CUDA 12.8 base, the same base
  as `stt-cohere`.
- **Batching:** NeMo's `CacheAwareRNNTPipeline` (its own multi-stream
  streaming path for this model) with its end-of-utterance detector off; the
  plugin's VAD and `flush` decide utterances. Each decode tick takes one
  560 ms chunk from every stream that has one and runs them as one batch.

## Measured

Measured with `measure.py` against this image on an **NVIDIA RTX 5080
(16 GB)**, bf16, `STT_MAX_STREAMS=8`, a fresh container per row. Audio:
LibriSpeech clean utterances (the 73 of the public
`hf-internal-testing/librispeech_asr_dummy` set), 8 per stream, streamed at
real time in 80 ms frames, 1 s of silence after each with a `flush` 700 ms
into it.

| streams | server process GPU memory (nvidia-smi) | WER | `flush` → `final` p50 / p95 | first partial p95 |
|---|---|---|---|---|
| idle (model loaded) | **1818–1830 MiB** | | | |
| 1 | **1892 MiB** | 3.5% | 0.001 / 0.017 s | 1.06 s |
| 4 | **1902 MiB** | 4.8% | 0.000 / 0.021 s | 1.07 s |
| 8 | **1960 MiB** | 4.4% | 0.000 / 0.031 s | 1.63 s |

The streaming rows are the idle process figure plus the rise in total GPU
memory while streams ran (the GPU had another tenant whose use did not
change). Without `torch.cuda.empty_cache()` after load the idle process held
5146 MiB (the fp32 copy the bf16 cast freed stays reserved); `nemo_engine.py`
releases it. The design's earlier figure (~6.8 GB) was fp32 and untuned.

**Not measured here:** the fit **beside `stt-cohere` on the same GPU** (this
GPU did not run `stt-cohere`) is an **open follow-up**: run the procedure
below on the GPU host before raising `STT_MAX_STREAMS` above 8. Also not
measured: WER on meeting audio (AMI, phase 3d), and streams past 8. Each run also checked: the 9th stream got `503` with
`Retry-After: 30`; one utterance followed directly by `end` came back with its
words; and after three rounds of dropping 8 streams without `end`, 8 fresh
streams all transcribed (no leaked slot).

### Measuring on a GPU host

```
docker run -d --name stt-nemotron --gpus all -e STT_API_TOKEN=<token> \
  -v stt-nemotron-models:/models -p 127.0.0.1:8000:8000 ghcr.io/<owner>/muster-stt-nemotron:<version>
python3 measure.py --url ws://127.0.0.1:8000/v1/stream --token <token> \
  --streams 8 --cap 8 --manifest meta.json --gpu
```

`meta.json` is a list of `{"wav": path, "text": reference}` (16 kHz mono
16-bit). `--gpu` samples `nvidia-smi` on the machine running `measure.py`, so
run it on the GPU host: its `gpu_mib_peak` is the **whole GPU**, i.e. the fit
beside whatever else runs there.
