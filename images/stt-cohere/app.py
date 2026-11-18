"""stt-cohere: an OpenAI-style speech-to-text server for muster's dictation.

Serves `POST /v1/audio/transcriptions` (multipart: a `file` part plus optional
`language` and `punctuation` fields, bearer authentication) and answers
`{"text": "..."}` plus diagnostic fields. muster's client (internal/stt) is the
caller; docs/dictation.md is the contract.

The model is CohereLabs/cohere-transcribe-03-2026 (about 2B parameters, BF16 on
a GPU, roughly 6 GB of VRAM). It is GATED on Hugging Face: set HF_TOKEN to a
token from an account that accepted its license.

Environment:
  STT_API_TOKEN          the bearer callers must present (REQUIRED; without it
                         every request and /healthz answer 503)
  HF_TOKEN               Hugging Face token for the gated model download
  STT_MODEL              model id (default CohereLabs/cohere-transcribe-03-2026)
  STT_MAX_AUDIO_SECONDS  longest accepted recording (default 3600)
  STT_MAX_NEW_TOKENS     generation cap per chunk (default 256)
  STT_SCORES             "0" turns off per-token scores (the diagnostics below)
  STT_NO_SPEECH_MAX      no-speech probability above which a transcript is
                         labelled unreliable (default 1e-9; "off" disables)
  STT_AVG_LOGPROB_MIN    mean logprob below which it is labelled unreliable
                         (default off)

The diagnostics LABEL a transcript; nothing here refuses a request on them.
"""

import hmac
import logging
import math
import os
import re
import subprocess
import tempfile
import time
import zlib
from contextlib import asynccontextmanager

import numpy as np
import torch
from fastapi import Depends, FastAPI, File, Form, HTTPException, UploadFile
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from transformers import AutoModelForSpeechSeq2Seq, AutoProcessor

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("stt")

MODEL_ID = os.environ.get("STT_MODEL", "CohereLabs/cohere-transcribe-03-2026")
API_TOKEN = os.environ.get("STT_API_TOKEN", "")
MAX_AUDIO_SECONDS = float(os.environ.get("STT_MAX_AUDIO_SECONDS", "3600"))
MAX_NEW_TOKENS = int(os.environ.get("STT_MAX_NEW_TOKENS", "256"))
FFMPEG_TIMEOUT = 120.0

# Per-token scores keep one [1, vocab] logits tensor per decoding step. This
# model's vocab is 16384, so the worst case is 256 x 16384 x 4 B = 16 MiB.
# The switch exists so the allocation can be dropped without a rebuild.
SCORES_ENABLED = os.environ.get("STT_SCORES", "1").strip().lower() not in {
    "0", "false", "no", "off", "",
}


def _optional_float(name: str, default: str):
    """A threshold, or None meaning "this rule is not configured".

    None is a real value: an unconfigured rule makes `reliability` answer null
    ("nothing was checked"), never false ("checked, looks fine"). An empty
    string, `off` or `none` turns a rule off.
    """
    raw = os.environ.get(name, default).strip().lower()
    if raw in {"", "off", "none", "null"}:
        return None
    try:
        return float(raw)
    except ValueError:
        log.warning("%s=%r is not a number — the rule is OFF", name, raw)
        return None


# A deliberately loose starting point, derived from a handful of synthetic
# probes (clean speech scored ~1e-10, fabrications and silence ~1e-8). It only
# labels, so erring towards false positives is the safe direction.
NO_SPEECH_MAX = _optional_float("STT_NO_SPEECH_MAX", "1e-9")

# Off by default: too little evidence to set a threshold.
AVG_LOGPROB_MIN = _optional_float("STT_AVG_LOGPROB_MIN", "")

SAMPLE_RATE = 16000

# The processor raises ValueError for any language outside this set, so it is
# checked up front and answered with a 400 that names the accepted codes. No
# silent coercion: `en-US` is refused, not turned into `en`. The processor's own
# ValueError is caught too, in case a model update moves the set.
SUPPORTED_LANGUAGES = (
    "ar", "de", "el", "en", "es", "fr", "it", "ja", "ko", "nl", "pl", "pt", "vi", "zh",
)

# Matched against the tokenizer's ADDED vocabulary (the special tokens) only,
# so the English word "silence" in the ordinary vocab cannot match.
NO_SPEECH_TOKEN_PATTERN = re.compile(r"no[\W_]*speech|nospeech|silen(?:ce|t)", re.I)

state: dict = {
    "model": None,
    "processor": None,
    "device": None,
    "loaded_at": None,
    "no_speech_token": None,
}


def language_error(language: str) -> str | None:
    """None if the processor will accept `language`, else the 400 body."""
    if language in SUPPORTED_LANGUAGES:
        return None
    return (
        f"unsupported language {language!r}: this model accepts the bare codes "
        f"{', '.join(SUPPORTED_LANGUAGES)}. A BCP-47 tag such as 'en-US' is not "
        f"one of them — send 'en'."
    )


def find_no_speech_token(added_vocab: dict) -> int | None:
    """The id of a dedicated no-speech token, if this model has one.

    cohere-transcribe-03-2026 has `<|nospeech|>` (id 1), the only added token
    this pattern matches. None stays a result for a model without one.
    """
    for token, tid in sorted(added_vocab.items()):
        if NO_SPEECH_TOKEN_PATTERN.search(str(token)):
            return int(tid)
    return None


def eos_id_set(raw) -> frozenset:
    """Normalise `eos_token_id` (int, list or None) to a set of ints."""
    if raw is None:
        return frozenset()
    if isinstance(raw, (list, tuple, set, frozenset)):
        return frozenset(int(t) for t in raw if t is not None)
    return frozenset({int(raw)})


def finish_reason_of(rows, eos: frozenset) -> str:
    """"stop" | "length" | "unknown", read from the tokens.

    The test is "is an EOS present in the row", not "is it last": chunked audio
    right-pads shorter rows past their EOS. With no EOS id the answer is
    "unknown", never "length" (which would mark every transcript truncated).
    """
    if not eos:
        return "unknown"
    for row in rows:
        if not any(int(t) in eos for t in row):
            return "length"
    return "stop"


def compression_ratio(text: str) -> float:
    """utf-8 bytes / deflated bytes — Whisper's repetition heuristic.

    Meaningless at dictation lengths (below ~100 bytes deflate's header
    dominates); reported because it is free, not thresholded.
    """
    data = text.encode("utf-8")
    if not data:
        return 0.0
    return len(data) / len(zlib.compress(data, 9))


def mean_logprob(token_logprobs) -> float | None:
    """Mean per-token logprob over the finite entries, or None.

    Padded positions of a chunked batch are -inf; a plain mean would be -inf
    for every multi-chunk transcript.
    """
    if token_logprobs is None:
        return None
    vals = [float(v) for v in token_logprobs if math.isfinite(float(v))]
    if not vals:
        return None
    return sum(vals) / len(vals)


def no_speech_prob_of(step_probs) -> float | None:
    """The largest P(no-speech token) over the generated steps, or None.

    An exact zero at every step is the signature of a masked token, not an
    observation about the audio, so it is reported as None ("no signal")
    rather than 0.0 ("definitely speech").
    """
    if not step_probs:
        return None
    vals = [float(p) for p in step_probs if math.isfinite(float(p))]
    if not vals:
        return None
    best = max(vals)
    return None if best == 0.0 else best


def reliability(no_speech_prob, avg_logprob, no_speech_max, avg_logprob_min):
    """(unreliable, reasons) — three-valued.

    True: a configured rule fired. False: every configured rule was evaluated
    and none fired. None: nothing could be checked. A client must render None
    as "no signal", never as "looks fine".
    """
    reasons = []
    checked = False
    if no_speech_max is not None and no_speech_prob is not None:
        checked = True
        if float(no_speech_prob) > float(no_speech_max):
            reasons.append("no_speech_prob")
    if avg_logprob_min is not None and avg_logprob is not None:
        checked = True
        if float(avg_logprob) < float(avg_logprob_min):
            reasons.append("avg_logprob")
    if not checked:
        return None, []
    return bool(reasons), reasons


def generation_diagnostics(rows, eos, token_logprobs, text, no_speech_prob) -> dict:
    """The additive response fields that let a client distrust a transcript.

    The verdict is computed from the ROUNDED values the response carries, so a
    client re-deriving it from the JSON gets the same answer. no_speech_prob is
    rounded to significant figures (%.6g): it is routinely below 1e-7.
    """
    reason = finish_reason_of(rows, eos)
    avg = mean_logprob(token_logprobs)
    shown_no_speech = (
        None if no_speech_prob is None else float(f"{float(no_speech_prob):.6g}")
    )
    shown_avg = None if avg is None else round(avg, 4)
    unreliable, reasons = reliability(
        shown_no_speech, shown_avg, NO_SPEECH_MAX, AVG_LOGPROB_MIN
    )
    return {
        "unreliable": unreliable,
        "unreliable_reasons": reasons,
        "finish_reason": reason,
        "truncated": None if reason == "unknown" else reason == "length",
        "avg_logprob": shown_avg,
        "compression_ratio": round(compression_ratio(text), 3),
        "no_speech_prob": shown_no_speech,
    }


CONSTANT_SIGNAL_REASON = "constant_signal"


def constant_signal_diagnostics() -> dict:
    """The diagnostics block for a recording refused by `is_constant_signal`.

    Exactly generation_diagnostics' key set (one response shape), with every
    value "no signal" since no generation ran, and `unreliable: True` because
    the server does know something definite about this audio.
    """
    return {
        "unreliable": True,
        "unreliable_reasons": [CONSTANT_SIGNAL_REASON],
        "finish_reason": "unknown",
        "truncated": None,
        "avg_logprob": None,
        "compression_ratio": round(compression_ratio(""), 3),
        "no_speech_prob": None,
    }


class UndecodableAudio(Exception):
    """ffmpeg could not read the upload as audio. Carries ffmpeg's own stderr."""


class DecoderCrashed(Exception):
    """The decoder was KILLED (a negative return code), e.g. by the OOM killer.

    A server fault, answered 503 (retryable) — never 400, which would tell the
    caller their recording can never work.
    """


def decode_audio(path: str) -> np.ndarray:
    """Decode any container ffmpeg understands to 16 kHz mono float32.

    ffmpeg rather than transformers' load_audio, which dispatches on the file
    EXTENSION and refuses the containers browsers record (webm, mp4).

    `-t` after `-i` is an OUTPUT option, so the decoded PCM is capped at
    MAX_AUDIO_SECONDS + 1 whatever arrives; the +1 lets the duration check in
    `transcribe` still see an over-long upload and answer 413.
    """
    cmd = [
        "ffmpeg", "-nostdin", "-loglevel", "error",
        "-i", path,
        "-t", str(MAX_AUDIO_SECONDS + 1),
        "-f", "f32le", "-acodec", "pcm_f32le",
        "-ac", "1", "-ar", str(SAMPLE_RATE),
        "-",
    ]
    try:
        proc = subprocess.run(
            cmd, capture_output=True, timeout=FFMPEG_TIMEOUT, stdin=subprocess.DEVNULL
        )
    except subprocess.TimeoutExpired:
        raise UndecodableAudio(f"ffmpeg did not finish within {FFMPEG_TIMEOUT:.0f}s")
    if proc.returncode < 0:
        said = _scrub(proc.stderr, path)
        raise DecoderCrashed(
            f"the decoder was killed by signal {-proc.returncode}"
            + (f"; ffmpeg said: {said}" if said else "")
        )
    if proc.returncode != 0:
        raise UndecodableAudio(_scrub(proc.stderr, path))
    # A valid, EMPTY container (e.g. a WAV with a zero-length data chunk) exits 0.
    if not proc.stdout:
        raise UndecodableAudio("no decodable audio stream in the upload")
    return np.frombuffer(proc.stdout, dtype=np.float32)


def is_constant_signal(audio) -> bool:
    """True when every decoded sample holds the SAME value — a dead capture.

    NOT an energy/RMS gate: quiet real speech can be quieter than room tone the
    model hallucinates on, so no amplitude cutoff separates them. A real
    microphone always carries dither; a dead capture (some phones hand a losing
    microphone client bit-exact zeros) has one distinct value, and the model
    answers it with fabricated prose. min == max, not np.unique: O(n), no sort.
    """
    if audio.size == 0:
        return False
    return bool(audio.min() == audio.max())


def _scrub(stderr: bytes, path: str) -> str:
    """ffmpeg's message with the temp path removed and newlines collapsed (so
    a crafted file cannot forge log records)."""
    flat = " ".join(stderr.decode("utf-8", "replace").replace(path, "<upload>").split())
    return flat[:500]


@asynccontextmanager
async def lifespan(app: FastAPI):
    if not API_TOKEN:
        log.error("STT_API_TOKEN is not set: every request and /healthz will answer 503")
    load_model()
    yield
    state.update(model=None, processor=None)


def load_model():
    t0 = time.time()
    log.info("loading %s", MODEL_ID)
    device = "cuda" if torch.cuda.is_available() else "cpu"
    if device == "cuda":
        log.info("gpu: %s", torch.cuda.get_device_name(0))
        # Prove a real tensor op works on this GPU before serving.
        x = torch.ones(8, device="cuda") @ torch.ones(8, device="cuda")
        log.info("cuda tensor op ok: sum=%s", float(x.sum()))
    dtype = torch.bfloat16 if device == "cuda" else torch.float32
    # CohereAsr is native in transformers 5.x; no trust_remote_code.
    processor = AutoProcessor.from_pretrained(MODEL_ID)
    model = AutoModelForSpeechSeq2Seq.from_pretrained(MODEL_ID, torch_dtype=dtype).to(device)
    model.eval()
    no_speech_token = None
    try:
        no_speech_token = find_no_speech_token(processor.tokenizer.get_added_vocab())
    except Exception as e:  # noqa: BLE001 - a diagnostic must never fail a load
        log.warning("could not inspect the added vocabulary for a no-speech token: %s", e)
    state.update(
        model=model,
        processor=processor,
        device=device,
        loaded_at=time.time() - t0,
        no_speech_token=no_speech_token,
    )
    log.info(
        "model loaded on %s in %.1fs (scores=%s, no_speech_token=%s, eos=%s, vocab=%s)",
        device,
        state["loaded_at"],
        SCORES_ENABLED,
        no_speech_token,
        sorted(eos_id_set(getattr(model.generation_config, "eos_token_id", None))),
        getattr(model.config, "vocab_size", "?"),
    )


bearer = HTTPBearer(auto_error=False)


def require_token(creds: HTTPAuthorizationCredentials = Depends(bearer)):
    if not API_TOKEN:
        raise HTTPException(503, "server has no STT_API_TOKEN configured")
    if creds is None or not hmac.compare_digest(creds.credentials, API_TOKEN):
        raise HTTPException(401, "invalid or missing bearer token")
    return creds


app = FastAPI(title="stt-cohere", lifespan=lifespan)


@app.get("/healthz")
async def healthz():
    """Healthy = the model is loaded AND a token is configured.

    `async def` on purpose: a sync handler would share the threadpool that
    `transcribe` holds for a whole decode, so a burst of transcriptions could
    starve the probe and get a working container killed. Keep it free of IO.
    """
    if not API_TOKEN:
        raise HTTPException(503, "server has no STT_API_TOKEN configured")
    if state["model"] is None:
        raise HTTPException(503, "model not loaded yet")
    return {"ok": True, "device": state["device"], "load_seconds": round(state["loaded_at"], 1)}


@app.get("/")
async def root():
    return {"service": "stt-cohere", "model": MODEL_ID, "usage": "POST /v1/audio/transcriptions (multipart: file)"}


@app.post("/v1/audio/transcriptions")
def transcribe(
    file: UploadFile = File(...),
    language: str = Form(default="en"),
    punctuation: bool = Form(default=True),
    _=Depends(require_token),
):
    if state["model"] is None:
        raise HTTPException(503, "model not loaded yet")
    # Before the handler's own copy of the upload: a request that can only be
    # refused should not pay for a second copy on disk.
    bad_language = language_error(language)
    if bad_language:
        raise HTTPException(400, bad_language)
    suffix = os.path.splitext(file.filename or "audio.wav")[1] or ".wav"
    tmp = tempfile.NamedTemporaryFile(suffix=suffix, delete=False)
    try:
        for chunk in iter(lambda: file.file.read(1 << 20), b""):
            tmp.write(chunk)
        tmp.close()
        t0 = time.time()
        try:
            audio = decode_audio(tmp.name)
        except DecoderCrashed as e:
            log.error("decoder crashed: %s", e)
            raise HTTPException(503, f"the audio decoder failed: {e}")
        except UndecodableAudio as e:
            raise HTTPException(400, f"could not decode the uploaded audio: {e}")
        duration = len(audio) / float(SAMPLE_RATE)
        if duration > MAX_AUDIO_SECONDS:
            # "more than", not a figure: decode_audio caps the duration, so the
            # decoded length is the cap, not the upload's real length.
            raise HTTPException(
                413, f"audio too long: more than {MAX_AUDIO_SECONDS:.0f}s"
            )
        # A dead capture is answered 200 with an empty transcript, never a 4xx:
        # the refusal is final, and a 4xx would invite retries of identical
        # silence.
        if is_constant_signal(audio):
            elapsed = time.time() - t0
            diagnostics = constant_signal_diagnostics()
            log.info(
                "rejected constant-signal audio: %.1fs, every sample == %r "
                "(lang=%s, unreliable=%s %s)",
                duration, float(audio[0]), language,
                diagnostics["unreliable"],
                ",".join(diagnostics["unreliable_reasons"]),
            )
            return {
                "text": "",
                "duration_s": round(duration, 2),
                "elapsed_s": round(elapsed, 2),
                **diagnostics,
            }
        proc = state["processor"]
        model = state["model"]
        try:
            inputs = proc(
                audio=audio, sampling_rate=SAMPLE_RATE, return_tensors="pt",
                language=language, punctuation=punctuation,
            )
        except ValueError as e:
            # The backstop for SUPPORTED_LANGUAGES, scoped to this one call.
            raise HTTPException(400, f"the processor refused the request: {e}")
        # Long audio is auto-chunked; decode needs the chunk index to reassemble.
        audio_chunk_index = inputs.get("audio_chunk_index")
        inputs = inputs.to(model.device, dtype=model.dtype)
        # return_dict_in_generate=True: every reader below uses `.sequences`.
        gen = model.generate(
            **inputs,
            max_new_tokens=MAX_NEW_TOKENS,
            return_dict_in_generate=True,
            output_scores=SCORES_ENABLED,
        )
        sequences = gen.sequences
        decode_kwargs = dict(skip_special_tokens=True, language=language)
        if audio_chunk_index is not None:
            decode_kwargs["audio_chunk_index"] = audio_chunk_index
        decoded = proc.decode(sequences, **decode_kwargs)
        text = decoded[0] if isinstance(decoded, list) else decoded
        diagnostics = generation_diagnostics(
            rows=sequences.tolist(),
            eos=eos_id_set(getattr(model.generation_config, "eos_token_id", None)),
            token_logprobs=_token_logprobs(model, gen),
            text=text,
            no_speech_prob=_no_speech_prob(gen, state["no_speech_token"]),
        )
        elapsed = time.time() - t0
        # The reasons only, never the TEXT: transcripts do not go to the log.
        log.info(
            "transcribed %.1fs audio in %.2fs (RTFx %.1f, lang=%s, finish=%s, "
            "avg_logprob=%s, cr=%s, no_speech=%s, unreliable=%s%s)",
            duration, elapsed, duration / elapsed, language,
            diagnostics["finish_reason"], diagnostics["avg_logprob"],
            diagnostics["compression_ratio"], diagnostics["no_speech_prob"],
            diagnostics["unreliable"],
            (" " + ",".join(diagnostics["unreliable_reasons"])
             if diagnostics["unreliable_reasons"] else ""),
        )
        return {
            "text": text,
            "duration_s": round(duration, 2),
            "elapsed_s": round(elapsed, 2),
            **diagnostics,
        }
    finally:
        os.unlink(tmp.name)


def _token_logprobs(model, gen):
    """Per-token logprobs for the generated ids, or None on any failure: a
    diagnostic must never cost the caller their transcript."""
    if not SCORES_ENABLED or not getattr(gen, "scores", None):
        return None
    try:
        scores = model.compute_transition_scores(
            gen.sequences, gen.scores, normalize_logits=True
        )
        return scores.flatten().tolist()
    except Exception as e:  # noqa: BLE001
        log.warning("could not compute transition scores: %s", e)
        return None


def _no_speech_prob(gen, token_id):
    """P(no-speech token) across every generated step (chunk 0 only), reduced
    by no_speech_prob_of. None means "no signal", never "there was speech"."""
    if token_id is None or not getattr(gen, "scores", None):
        return None
    try:
        tid = int(token_id)
        per_step = [
            torch.softmax(step[0].float(), dim=-1)[tid].item() for step in gen.scores
        ]
        return no_speech_prob_of(per_step)
    except Exception as e:  # noqa: BLE001
        log.warning("could not read the no-speech probability: %s", e)
        return None
