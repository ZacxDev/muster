"""stt-nemotron: a streaming speech-to-text websocket server for live captions.

One websocket per audio track. The client sends 16 kHz mono S16LE audio in
80 ms frames and asks for a `final` at each end of utterance (`flush`); the
server answers with `partial` results as the model decodes and a `final` for
each flush and for `end`. README.md in this directory is the protocol
contract; docs/design/meetings.md ("Serving Nemotron") is the design.

The model is nvidia/nemotron-speech-streaming-en-0.6b, a cache-aware streaming
FastConformer RNNT, run in bf16 by NeMo. All live streams share ONE decode
loop on one thread: each tick runs one batched model step over every stream
that has a whole chunk of audio waiting (nemo_engine.py). Everything in this
file is model-agnostic and is tested on CPU with a stub engine (test_app.py).

Environment:
  STT_API_TOKEN            the bearer clients must present (REQUIRED; without it
                           every upgrade and /readyz answer 503)
  STT_MAX_STREAMS          concurrent streams (default 8); the next upgrade is
                           refused with 503 + Retry-After BEFORE the upgrade
  STT_RETRY_AFTER          seconds in that Retry-After (default 30)
  STT_MAX_BACKLOG_SECONDS  undecoded audio one stream may queue before it is
                           closed 1013 (default 30)
  STT_IDLE_SECONDS         a stream that sends nothing for this long is closed
                           1001 and frees its slot (default 30)
  STT_FINAL_HOLD_SECONDS   longest a flush's final waits for the next model
                           step to bring its late closing mark (default 1.0)
  STT_PORT                 listen port (default 8000)

The model, its attention context (70,6: 560 ms chunks) and its precision
(bf16) are NOT configurable: they are the benchmarked values, fixed in
nemo_engine.py.
"""

from __future__ import annotations

import asyncio
import collections
import hmac
import json
import logging
import os
import re
import threading
import time
from contextlib import asynccontextmanager

import numpy as np
from fastapi import FastAPI, WebSocket
from fastapi.responses import JSONResponse, Response
from starlette.websockets import WebSocketDisconnect

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("stt-nemotron")

API_TOKEN = os.environ.get("STT_API_TOKEN", "")
MAX_STREAMS = int(os.environ.get("STT_MAX_STREAMS", "8"))
RETRY_AFTER = int(os.environ.get("STT_RETRY_AFTER", "30"))
MAX_BACKLOG_SECONDS = float(os.environ.get("STT_MAX_BACKLOG_SECONDS", "30"))
# 🔴 A PEER THAT GOES SILENT IS CLOSED. Pings are off (the design), so a dead
# peer that never sent FIN/RST would otherwise wait in receive() forever and
# hold its slot: eight of them and every new stream is refused while /readyz
# says ready. The plugin's track egress sends a frame every 80 ms, silence
# included, so a live stream is never idle this long.
IDLE_SECONDS = float(os.environ.get("STT_IDLE_SECONDS", "30"))

# 🔴 THE WIRE FORMAT. The model's own rate must equal it or the server refuses
# to start (check_sample_rate): a mismatch would not error anywhere, it would
# transcribe sped-up or slowed-down audio into confident nonsense.
SAMPLE_RATE = 16000
FRAME_SAMPLES = 1280  # 80 ms
FRAME_BYTES = FRAME_SAMPLES * 2  # S16LE

# Websocket close codes (RFC 6455).
CLOSE_NORMAL = 1000
CLOSE_GOING_AWAY = 1001  # idle: no frame for STT_IDLE_SECONDS
CLOSE_UNSUPPORTED = 1003  # bad_frame, bad_message, bad_end
CLOSE_INTERNAL = 1011  # the model step failed
CLOSE_TRY_AGAIN = 1013  # overloaded: this stream's backlog passed the cap


def ms(samples: int) -> int:
    """Audio time in whole milliseconds from a sample count (never wall time)."""
    return samples * 1000 // SAMPLE_RATE


def check_sample_rate(rate) -> None:
    """Refuse to serve a model whose preprocessor is not 16 kHz."""
    if rate is None or int(rate) != SAMPLE_RATE:
        raise RuntimeError(
            f"the model's preprocessor.sample_rate is {rate!r}, not {SAMPLE_RATE}: "
            f"this server's wire format is {SAMPLE_RATE} Hz and it will not resample"
        )


# 🔴 A SENTENCE'S CLOSING MARK IS DECODED ONE STEP LATE. The model emits it
# only after it has heard what follows, and at a `flush` up to one chunk of
# audio is still undecoded, so the "?" of "Can Carol book the room?" lands in
# the step AFTER the flush, which belongs to the NEXT segment (measured live:
# every final from the second on read ". Thanks everyone."). So a flush's
# final is HELD until that stream's next model step, and the marks that step
# opens the next segment with are moved back onto it (DecodeLoop._release).
# The hold is bounded: it is released by the next step, the next flush, `end`,
# a failed step, or after FINAL_HOLD_SECONDS of wall time, whichever is first.
# Any mark that still opens a segment (decoded two steps late, or after a
# timed-out hold) is dropped by strip_leading_punct, the backstop: a segment's
# text never starts with one. Punctuation inside the text is never touched.
FINAL_HOLD_SECONDS = float(os.environ.get("STT_FINAL_HOLD_SECONDS", "1.0"))
_LEADING_PUNCT = re.compile(r"^[\s.,;:!?…]+")


def strip_leading_punct(text: str) -> str:
    """`text` without the run of whitespace and . , ; : ! ? … it starts with."""
    return _LEADING_PUNCT.sub("", text)


def leading_marks(text: str) -> str:
    """The marks (. , ; : ! ? …) `text` opens with, whitespace removed."""
    m = _LEADING_PUNCT.match(text)
    return "" if m is None else re.sub(r"\s", "", m.group(0))


def bearer_ok(header: str | None) -> bool:
    """True when `Authorization` carries exactly `Bearer <STT_API_TOKEN>`."""
    if not API_TOKEN or not header:
        return False
    scheme, _, cred = header.partition(" ")
    if scheme.lower() != "bearer":
        return False
    return hmac.compare_digest(cred.strip().encode(), API_TOKEN.encode())


# ---------------------------------------------------------------------------
# Streams and the decode loop


class Stream:
    """One websocket's state, shared between its handler (event loop) and the
    decode thread. The handler only appends to `inbox` and reads `outbox`;
    everything else belongs to the decode thread."""

    def __init__(self, sid: int, engine_state, loop: asyncio.AbstractEventLoop):
        self.sid = sid
        self.es = engine_state
        self.loop = loop
        self.inbox: collections.deque = collections.deque()  # ("audio", int16) | ("flush",) | ("end", n)
        self.outbox: asyncio.Queue = asyncio.Queue()
        self.closed = False  # set by the handler when the socket is gone
        # decode-thread state
        self.received = 0  # samples moved from the inbox into the engine
        self.ending = False
        self.seg_start = 0  # sample where the current segment began
        self.seg_min: int | None = None
        self.seg_max: int | None = None
        self.pending: np.ndarray | None = None  # the newest frame, not yet in seg_min/max
        self.last_partial = ""
        self.done = False
        self.held: dict | None = None  # a flush's final, waiting for its late marks
        self.held_until = 0.0  # time.monotonic() at which it goes out regardless

    # -- constant-signal tracking -------------------------------------------
    def _fold(self, samples: np.ndarray) -> None:
        if samples.size == 0:
            return
        lo, hi = int(samples.min()), int(samples.max())
        self.seg_min = lo if self.seg_min is None else min(self.seg_min, lo)
        self.seg_max = hi if self.seg_max is None else max(self.seg_max, hi)

    def take_frame(self, frame: np.ndarray) -> None:
        """The newest frame is held back from the statistics until the next
        one arrives: if it turns out to be the last, its zero padding must not
        count (a constant non-zero signal plus padding would look varied)."""
        if self.pending is not None:
            self._fold(self.pending)
        self.pending = frame

    def settle(self, real_tail: int | None = None) -> None:
        """Fold the held-back frame, trimmed to `real_tail` samples if given."""
        if self.pending is not None:
            self._fold(self.pending if real_tail is None else self.pending[:real_tail])
            self.pending = None

    def constant_so_far(self) -> bool:
        """True when every sample of the segment so far had ONE value (or none
        arrived) — a dead capture, which the model answers with invented text.
        Not an energy gate: quiet real speech still varies.

        The held-back frame is NOT counted: until the next frame, a flush or
        `end` arrives it may be the zero-padded last one, and its padding
        would make a constant non-zero stream look varied and let partials of
        invented words out before `end`. It counts once settle() folds it in
        (trimmed to its real samples on `end`). Cost: when ONLY the newest
        frame varies (speech starting after a constant stretch), that step's
        partial is skipped and the words arrive with the next step's (560 ms
        later); finals are unaffected."""
        return self.seg_min is None or self.seg_min == self.seg_max

    def emit(self, msg: dict | None) -> None:
        """Hand a message (or None = close normally) to the handler."""
        if not self.closed:
            self.loop.call_soon_threadsafe(self.outbox.put_nowait, msg)


class DecodeLoop:
    """The single thread that owns the model.

    The engine (nemo_engine.NemotronEngine; a stub in the tests) is called
    only from this thread, through:
      new_stream() -> es          per-stream state (called by the handler,
                                  before the stream is visible here)
      push(es, float32 pcm)       append audio
      runnable(es, ending) -> b   one step can run now (ending: pad and finish)
      step([es], [ending])        ONE batched model step for all of them
      segment_text(es) -> str     text since the last cut
      cut(es)                     start a new segment
      decoded_samples(es) -> int  samples consumed (also read by the handler)
      close(es)                   free a stream that ended without `end`
      sample_rate                 the model's own rate (must be 16000)
 Each tick: move every stream's
    queued audio into the engine (stopping at a flush/end marker), run batched
    steps until no stream has a whole chunk, answer the markers, send partials.

    A flush therefore answers only after every chunk received before it was
    decoded, and its final then waits for that stream's next step (or the
    hold timeout) to collect a late closing mark. Streams are batched
    together whatever their phase."""

    def __init__(self, engine):
        self.engine = engine
        self.streams: dict[int, Stream] = {}
        self.cv = threading.Condition()
        self.stop = False
        self.thread = threading.Thread(target=self._run, name="decode", daemon=True)
        self.gone: list[Stream] = []  # removed streams the engine must still free

    def start(self):
        self.thread.start()

    def shutdown(self):
        with self.cv:
            self.stop = True
            self.cv.notify()
        self.thread.join(timeout=10)

    def add(self, st: Stream):
        with self.cv:
            self.streams[st.sid] = st

    def wake(self):
        with self.cv:
            self.cv.notify()

    def remove(self, st: Stream):
        """The socket is gone. The engine's slot is freed on the decode thread
        (the only thread that touches the engine), at the next tick."""
        with self.cv:
            st.closed = True
            self.streams.pop(st.sid, None)
            self.gone.append(st)
            self.cv.notify()

    # -- the loop -------------------------------------------------------------
    def _run(self):
        while True:
            with self.cv:
                while not self.stop and not self.gone and not any(
                    s.inbox or self._runnable(s) for s in self.streams.values()
                ):
                    due = self._hold_deadline()
                    if due is not None and due <= time.monotonic():
                        break
                    self.cv.wait(None if due is None else due - time.monotonic())
                if self.stop:
                    return
                gone, self.gone = self.gone, []
                live = [s for s in self.streams.values() if not s.closed and not s.done]
            now = time.monotonic()
            for st in live:
                if st.held is not None and st.held_until <= now:
                    self._release(st)
            for st in gone:
                try:
                    self.engine.close(st.es)
                except Exception:  # noqa: BLE001
                    log.exception("freeing a closed stream failed")
            try:
                self._tick(live)
            except Exception:  # noqa: BLE001 - a crash here would silently stall every stream
                log.exception("decode tick failed")

    def _hold_deadline(self) -> float | None:
        return min((s.held_until for s in self.streams.values() if s.held is not None), default=None)

    def _runnable(self, st: Stream) -> bool:
        return not st.done and self.engine.runnable(st.es, st.ending)

    def _tick(self, live: list[Stream]):
        progress = True
        while progress:
            progress = False
            for st in live:
                self._intake(st)
            batch = [s for s in live if not s.done and not s.closed and self._runnable(s)]
            if batch:
                self._step(batch)
                progress = True
                continue
            for st in live:
                if st.done or st.closed or not st.inbox:
                    continue
                if st.inbox[0][0] != "audio":
                    self._command(st, st.inbox.popleft())
                    progress = True

    def _intake(self, st: Stream):
        while st.inbox and st.inbox[0][0] == "audio":
            frame = st.inbox.popleft()[1]
            st.take_frame(frame)
            st.received += frame.size
            self.engine.push(st.es, frame.astype(np.float32) / 32768.0)

    def _step(self, batch: list[Stream]):
        try:
            self.engine.step([s.es for s in batch], [s.ending for s in batch])
        except Exception as e:  # noqa: BLE001
            log.exception("model step failed for %d stream(s)", len(batch))
            for st in batch:
                st.done = True
                self._release(st)
                st.emit({"type": "error", "code": "internal", "message": f"model step failed: {type(e).__name__}"})
                st.emit(("close", CLOSE_INTERNAL))
            return
        for st in batch:
            if st.held is not None:
                # The step after a flush: the marks it opens the next segment
                # with close the held final's sentence.
                late = "" if st.constant_so_far() else leading_marks(self.engine.segment_text(st.es))
                self._release(st, late)
            self._partial(st)

    def _segment_text(self, st: Stream) -> str:
        return strip_leading_punct(self.engine.segment_text(st.es))

    def _decoded_end(self, st: Stream) -> int:
        return min(self.engine.decoded_samples(st.es), st.received)

    def _partial(self, st: Stream):
        if st.constant_so_far():
            return
        text = self._segment_text(st)
        if not text or text == st.last_partial:
            return
        st.last_partial = text
        st.emit({"type": "partial", "text": text, "start_ms": ms(st.seg_start), "end_ms": ms(max(self._decoded_end(st), st.seg_start))})

    def _final(self, st: Stream, end_sample: int, hold: bool = False):
        text = "" if st.constant_so_far() else self._segment_text(st)
        msg = {"type": "final", "text": text, "start_ms": ms(st.seg_start), "end_ms": ms(end_sample)}
        if hold:
            st.held, st.held_until = msg, time.monotonic() + FINAL_HOLD_SECONDS
        else:
            st.emit(msg)
        st.seg_start = end_sample
        self.engine.cut(st.es)
        st.seg_min = st.seg_max = None
        st.last_partial = ""

    def _release(self, st: Stream, marks: str = ""):
        """Send the held final, if any, with `marks` appended (never to an
        empty final: a dead or mark-only segment stays empty)."""
        msg, st.held = st.held, None
        if msg is None:
            return
        if marks and msg["text"]:
            msg["text"] += marks
        st.emit(msg)

    def _command(self, st: Stream, cmd: tuple):
        if cmd[0] == "flush":
            # Every frame before a flush is real audio (padding only ever
            # precedes `end`), so the held-back frame counts in full.
            st.settle()
            self._release(st)  # finals stay in flush order
            self._final(st, st.received, hold=True)
        elif cmd[0] == "end":
            n = cmd[1]
            pad = st.received - n
            st.settle(real_tail=None if st.pending is None else st.pending.size - pad)
            st.ending = True
            while self._runnable(st):
                self._step([st])
                if st.done:
                    return
            self._release(st)  # no step ran since the flush: it goes as it is
            self._final(st, n)
            st.done = True
            st.emit(None)


# ---------------------------------------------------------------------------
# The application

state: dict = {"engine": None, "loop": None, "next_id": 0, "active": 0, "load_seconds": None}


def make_engine():
    """The real engine. Imported lazily so tests (and the image smoke test)
    never import NeMo; tests replace this function."""
    import nemo_engine  # noqa: PLC0415

    return nemo_engine.NemotronEngine.from_env()


@asynccontextmanager
async def lifespan(app: FastAPI):
    if not API_TOKEN:
        log.error("STT_API_TOKEN is not set: every upgrade and /readyz will answer 503")
    t0 = time.time()
    engine = make_engine()
    check_sample_rate(engine.sample_rate)
    loop = DecodeLoop(engine)
    loop.start()
    state.update(engine=engine, loop=loop, load_seconds=time.time() - t0)
    log.info("ready: max_streams=%d, model loaded in %.1fs", MAX_STREAMS, state["load_seconds"])
    try:
        yield
    finally:
        loop.shutdown()
        state.update(engine=None, loop=None)


app = FastAPI(title="stt-nemotron", lifespan=lifespan)


@app.get("/healthz")
async def healthz():
    """Liveness: the event loop answers. `async def` and no IO on purpose."""
    return {"ok": True}


@app.get("/readyz")
async def readyz():
    """Ready = the model is loaded AND a token is configured. Stays 200 at
    capacity: a full server must keep answering 503 + Retry-After, which it
    cannot do once taken out of its Service."""
    if not API_TOKEN:
        return JSONResponse({"ok": False, "reason": "server has no STT_API_TOKEN configured"}, 503)
    if state["engine"] is None:
        return JSONResponse({"ok": False, "reason": "model not loaded yet"}, 503)
    return {
        "ok": True,
        "streams": state["active"],
        "max_streams": MAX_STREAMS,
        "load_seconds": round(state["load_seconds"] or 0, 1),
    }


def _refuse(status: int, reason: str, headers: dict | None = None) -> Response:
    return JSONResponse({"error": reason}, status_code=status, headers=headers)


@app.websocket("/v1/stream")
async def stream(ws: WebSocket):
    # 🔴 EVERY REFUSAL HAPPENS BEFORE accept(): the client gets a real HTTP
    # status and never an upgraded socket, so "full" (503) is distinguishable
    # from "broken" (a close after the upgrade).
    if not API_TOKEN:
        await ws.send_denial_response(_refuse(503, "server has no STT_API_TOKEN configured"))
        return
    if not bearer_ok(ws.headers.get("authorization")):
        await ws.send_denial_response(_refuse(401, "invalid or missing bearer token"))
        return
    if state["loop"] is None:
        await ws.send_denial_response(_refuse(503, "model not loaded yet", {"Retry-After": str(RETRY_AFTER)}))
        return
    # Check and take the slot with no await in between: two upgrades racing
    # for the last slot cannot both see it free.
    if state["active"] >= MAX_STREAMS:
        await ws.send_denial_response(
            _refuse(503, f"at capacity ({MAX_STREAMS} streams)", {"Retry-After": str(RETRY_AFTER)})
        )
        return
    state["active"] += 1
    try:
        await _serve(ws)
    finally:
        state["active"] -= 1


async def _serve(ws: WebSocket):
    decode: DecodeLoop = state["loop"]
    state["next_id"] += 1
    st = Stream(state["next_id"], state["engine"].new_stream(), asyncio.get_running_loop())
    await ws.accept()
    decode.add(st)
    sender = asyncio.create_task(_send_loop(ws, st))
    total = 0  # samples received, padding included
    ended = False
    try:
        while not ended:
            try:
                msg = await asyncio.wait_for(ws.receive(), IDLE_SECONDS)
            except asyncio.TimeoutError:
                await _fail(ws, "idle", f"no frame for {IDLE_SECONDS:g}s", CLOSE_GOING_AWAY)
                return
            if msg["type"] == "websocket.disconnect":
                return
            if sender.done():
                return
            data, text = msg.get("bytes"), msg.get("text")
            if data is not None:
                if len(data) != FRAME_BYTES:
                    await _fail(ws, "bad_frame", f"a binary frame must be exactly {FRAME_BYTES} bytes, got {len(data)}")
                    return
                total += FRAME_SAMPLES
                if (total - state["engine"].decoded_samples(st.es)) / SAMPLE_RATE > MAX_BACKLOG_SECONDS:
                    await _fail(ws, "overloaded", "this stream's undecoded audio passed the backlog cap", CLOSE_TRY_AGAIN)
                    return
                st.inbox.append(("audio", np.frombuffer(data, dtype="<i2").copy()))
                decode.wake()
                continue
            try:
                body = json.loads(text)
                kind = body.get("type") if isinstance(body, dict) else None
            except (TypeError, ValueError):
                kind = None
            if kind == "flush":
                st.inbox.append(("flush",))
                decode.wake()
            elif kind == "end":
                n = body.get("samples")
                # The last frame is zero-padded to 1280 samples, so the true
                # total lies within it: (total - 1280, total], or 0 for none.
                if not isinstance(n, int) or isinstance(n, bool) or not (
                    (total == 0 and n == 0) or (total - FRAME_SAMPLES < n <= total)
                ):
                    await _fail(ws, "bad_end", f"samples must be the true total within the last frame's padding ({total} received)")
                    return
                st.inbox.append(("end", n))
                decode.wake()
                ended = True
            else:
                await _fail(ws, "bad_message", 'a text frame must be {"type": "flush"} or {"type": "end", "samples": N}')
                return
        # The final for `end`, then the normal close. Bounded too: a sender
        # stuck writing to a dead peer must not hold the slot.
        try:
            await asyncio.wait_for(asyncio.shield(sender), IDLE_SECONDS)
            sender = None
        except asyncio.TimeoutError:
            return
    except WebSocketDisconnect:
        return
    finally:
        decode.remove(st)
        if sender is not None and not sender.done():
            sender.cancel()


async def _send_loop(ws: WebSocket, st: Stream):
    while True:
        msg = await st.outbox.get()
        if msg is None:
            await ws.close(CLOSE_NORMAL)
            return
        if isinstance(msg, tuple) and msg[0] == "close":
            await ws.close(msg[1])
            return
        await ws.send_text(json.dumps(msg))


async def _fail(ws: WebSocket, code: str, message: str, close_code: int = CLOSE_UNSUPPORTED):
    """Error frame, then close; bounded, since the peer may be dead (the
    caller's `finally` frees the slot whatever happens here)."""
    log.warning("closing a stream: %s (%s)", code, message)

    async def go():
        await ws.send_text(json.dumps({"type": "error", "code": code, "message": message}))
        await ws.close(close_code)

    try:
        await asyncio.wait_for(go(), 5)
    except (asyncio.TimeoutError, RuntimeError, WebSocketDisconnect):
        pass


def server_config() -> dict:
    """uvicorn settings. Pings OFF: the design says the server sends no pings
    it requires answered (the plugin's read loop detects a dead peer), and a
    frame is 2.5 KB, so a 64 KiB message cap is generous."""
    return dict(
        host="0.0.0.0",
        port=int(os.environ.get("STT_PORT", "8000")),
        ws="websockets-sansio",
        ws_ping_interval=None,
        ws_ping_timeout=None,
        ws_max_size=65536,
        log_level="info",
    )


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, **server_config())
