"""The Nemotron engine: NeMo's batched cache-aware RNNT streaming pipeline.

app.py's DecodeLoop drives this through six methods (new_stream, push,
runnable, step, segment_text, cut, plus close and decoded_samples). Every call
comes from the decode thread except decoded_samples, which reads one int.

What NeMo does, and what is ours:
  * NeMo's CacheAwareRNNTPipeline (nemo.collections.asr.inference) owns the
    per-stream feature cache, the per-stream encoder cache slots and the
    greedy RNNT decode. One `transcribe_step` call runs ONE batched model step
    for every stream in the list. It is NVIDIA's own multi-stream streaming
    path for this model; its example config is the base of `pipeline_cfg`.
  * Its end-of-utterance detector is switched OFF (stop_history_eou = -1):
    the plugin's VAD decides where an utterance ends and says so with
    `flush`. With it off the per-stream `tokens` list only grows, and a
    segment is the tokens after the index `cut` last recorded.
  * Model weights: a .nemo file at a pinned Hugging Face commit, fetched on
    first start into HF_HOME (a volume) and checked against a pinned sha256
    before it is loaded.
"""

from __future__ import annotations

import hashlib
import logging
import os
import re
import time
import types

import numpy as np
import torch
from omegaconf import OmegaConf

from nemo.collections.asr.inference.factory.cache_aware_pipeline_builder import CacheAwarePipelineBuilder
from nemo.collections.asr.inference.streaming.framing.request import Frame
from nemo.collections.asr.inference.streaming.framing.request_options import ASRRequestOptions
from nemo.collections.asr.inference.utils.pipeline_utils import ids_to_text_without_stripping

log = logging.getLogger("stt-nemotron")

# 🔴 THE MODEL IS PINNED TO ONE COMMIT AND ONE FILE HASH. A new upload under
# the same name cannot change what a release serves; to upgrade, change both,
# re-run the measurement in README.md, and record it.
MODEL_REPO = "nvidia/nemotron-speech-streaming-en-0.6b"
MODEL_FILE = "nemotron-speech-streaming-en-0.6b.nemo"
MODEL_REVISION = "ebe59e5a817142986528bbbee5dba8db7b38ed50"
MODEL_SHA256 = "283638054c44f6794e74fe9af9048d78a6d9d6c058c12131856c7859a62ac9cd"


def sha256_of(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 24), b""):
            h.update(block)
    return h.hexdigest()


def fetch_model() -> str:
    """The pinned .nemo, downloaded on first start and verified every start
    (a marker keyed on size and mtime skips re-hashing an unchanged file)."""
    from huggingface_hub import hf_hub_download  # noqa: PLC0415

    path = hf_hub_download(MODEL_REPO, MODEL_FILE, revision=MODEL_REVISION)
    st = os.stat(path)
    marker = f"{path}.verified"
    stamp = f"{MODEL_SHA256} {st.st_size} {st.st_mtime_ns}"
    try:
        with open(marker) as f:
            if f.read().strip() == stamp:
                return path
    except OSError:
        pass
    t0 = time.time()
    got = sha256_of(path)
    if got != MODEL_SHA256:
        raise RuntimeError(f"{MODEL_FILE} at {MODEL_REVISION} hashes to {got}, not the pinned {MODEL_SHA256}")
    log.info("verified %s (sha256 %s) in %.1fs", MODEL_FILE, got[:12], time.time() - t0)
    try:
        with open(marker, "w") as f:
            f.write(stamp)
    except OSError:  # a read-only cache still serves; it just re-hashes
        pass
    return path


def pipeline_cfg(model: str, max_streams: int, att_context: list[int], dtype: str):
    """NVIDIA's example cache_aware_rnnt.yaml, trimmed to what this server
    uses, with three changes: EOU off, ITN/NMT off, and batch = slots = the
    stream cap (each live stream holds one slot until it ends)."""
    return OmegaConf.create(
        {
            "asr": {
                "model_name": model,
                "device": "cuda" if torch.cuda.is_available() else "cpu",
                "device_id": 0,
                "compute_dtype": dtype,
                "use_amp": False,
                "decoding": {
                    "strategy": "greedy_batch",
                    "preserve_alignments": False,
                    "fused_batch_size": -1,
                    "greedy": {
                        "use_cuda_graph_decoder": False,
                        "enable_per_stream_biasing": False,
                        "preserve_frame_confidence": False,
                        "max_symbols": 10,
                    },
                },
            },
            "itn": {"input_case": "lower_cased", "whitelist": None, "overwrite_cache": False,
                    "max_number_of_permutations_per_split": 729, "left_padding_size": 4,
                    "batch_size": 32, "n_jobs": 1},
            "nmt": None,
            "confidence": {"exclude_blank": True, "aggregation": "mean",
                           "method_cfg": {"name": "entropy", "entropy_type": "tsallis",
                                          "alpha": 0.5, "entropy_norm": "exp"}},
            "endpointing": {"stop_history_eou": -1, "residue_tokens_at_end": 2},
            "streaming": {
                "sample_rate": 16000,
                "batch_size": max_streams,
                "word_boundary_tolerance": 4,
                "att_context_size": att_context,
                "use_cache": True,
                "use_feat_cache": True,
                "chunk_size_in_secs": None,
                "request_type": "frame",
                "num_slots": max_streams,
            },
            "matmul_precision": "high",
            "log_level": 20,
            "pipeline_type": "cache_aware",
            "asr_decoding_type": "rnnt",
            "enable_itn": False,
            "enable_nmt": False,
            "asr_output_granularity": "segment",
            "cache_dir": None,
            "lang": None,
            "return_tail_result": False,
        }
    )


def _keep_tokens() -> None:
    """Stands in for StreamingState.cleanup_after_eou on a stream's last step."""


class NemotronEngine:
    def __init__(self, model: str, max_streams: int, att_context: list[int], dtype: str):
        torch.set_float32_matmul_precision("high")
        cfg = pipeline_cfg(model, max_streams, att_context, dtype)
        self.pipeline = CacheAwarePipelineBuilder.build(cfg)
        p = self.pipeline
        self.sample_rate = int(p.asr_model.asr_model.cfg.preprocessor.sample_rate)
        self.chunk = int(round(p.chunk_size_in_secs * p.sample_rate))
        self.max_streams = max_streams
        self._sep = p.get_sep()
        self._lead = p.leading_regex_pattern
        self._next = 0
        if torch.cuda.is_available():
            # The .nemo loads in fp32 straight onto the GPU and is then cast to
            # bf16; without this the freed fp32 copy stays RESERVED by the
            # caching allocator: the idle process held 5146 MiB without this
            # and 1830 MiB with it (nvidia-smi, RTX 5080).
            torch.cuda.empty_cache()
            log.info(
                "gpu memory after load: %.0f MiB allocated, %.0f MiB reserved",
                torch.cuda.memory_allocated() / 2**20, torch.cuda.memory_reserved() / 2**20,
            )
        log.info(
            "nemotron: %s, dtype=%s, att_context=%s, chunk=%d samples (%.0f ms), slots=%d",
            model, dtype, att_context, self.chunk, 1000 * self.chunk / p.sample_rate, max_streams,
        )

    @classmethod
    def from_env(cls) -> "NemotronEngine":
        model = os.environ.get("STT_MODEL", "")
        if not model:
            model = fetch_model()
        att = [int(x) for x in os.environ.get("STT_ATT_CONTEXT", "70,6").split(",")]
        dtype = {"bf16": "bfloat16", "fp32": "float32"}[os.environ.get("STT_DTYPE", "bf16")]
        return cls(model, int(os.environ.get("STT_MAX_STREAMS", "8")), att, dtype)

    # -- per stream -----------------------------------------------------------
    def new_stream(self):
        self._next += 1
        return types.SimpleNamespace(
            sid=self._next, buf=np.zeros(0, np.float32), frames=0, decoded=0,
            state=None, mark=0, finished=False,
        )

    def push(self, es, pcm: np.ndarray) -> None:
        es.buf = np.concatenate([es.buf, pcm])

    def runnable(self, es, ending: bool) -> bool:
        if es.finished:
            return False
        if es.buf.size >= self.chunk and not (ending and es.buf.size <= self.chunk):
            return True
        # At the end: one last frame (is_last) for a stream that has audio or
        # was ever stepped; it frees the stream's slots inside NeMo.
        return ending and (es.buf.size > 0 or es.frames > 0)

    def step(self, states, ending) -> None:
        p = self.pipeline
        frames = []
        for es, end in zip(states, ending):
            last = end and es.buf.size <= self.chunk
            take = es.buf[: self.chunk]
            n = take.size
            if n < self.chunk:
                take = np.concatenate([take, np.zeros(self.chunk - n, np.float32)])
            if es.state is None:
                # Created here, not inside transcribe_step, so this object
                # holds the state even after NeMo drops it at is_last.
                es.state = p.init_state(es.sid, ASRRequestOptions())
            frames.append(Frame(
                samples=torch.from_numpy(take), stream_id=es.sid,
                is_first=es.frames == 0, is_last=last,
                # NeMo cannot take an all-padding frame; one real sample of
                # the zeros is harmless and keeps the length positive.
                length=max(n, 1) if last else -1,
                options=ASRRequestOptions(),
            ))
            es.buf = es.buf[n:]
            es.frames += 1
            if last:
                es.finished = True
        for es, fr in zip(states, frames):
            if fr.is_last:
                # 🔴 NeMo treats is_last as an end of utterance and CLEARS the
                # state's tokens inside transcribe_step, before this engine can
                # read the final segment (measured: every `end` final came back
                # empty). Keep them; the state object is discarded afterwards.
                es.state.cleanup_after_eou = _keep_tokens
        p.transcribe_step(frames)
        for es in states:
            es.decoded = es.frames * self.chunk

    def segment_text(self, es) -> str:
        if es.state is None:
            return ""
        toks = es.state.tokens[es.mark:]
        if not toks:
            return ""
        text = ids_to_text_without_stripping(toks, self.pipeline.tokenizer, self._sep)
        if self._lead:
            text = re.sub(self._lead, r"\1", text)
        return text.strip()

    def cut(self, es) -> None:
        es.mark = 0 if es.state is None else len(es.state.tokens)

    def decoded_samples(self, es) -> int:
        return es.decoded

    def close(self, es) -> None:
        """Free a stream that went away without `end`: its feature-buffer
        slot, its encoder-cache slot and its state. Without this every
        dropped connection would leak one of the `max_streams` slots."""
        if es.finished or es.frames == 0:
            es.finished = True
            return
        p = self.pipeline
        fslot = p.bufferer.streamidx2slotidx.get(es.sid)
        if fslot is not None:
            p.bufferer.free_slots([fslot])
        cslot = p.context_manager.streamidx2slotidx.get(es.sid)
        if cslot is not None:
            p.context_manager._reset_slots([cslot])
        p.delete_state(es.sid)
        es.finished = True
