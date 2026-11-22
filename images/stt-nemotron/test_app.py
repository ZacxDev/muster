"""The stt-nemotron server speaks the protocol in README.md.

Run: python3 images/stt-nemotron/test_app.py
Needs: httpx and the server packages AT THE PINS in requirements.txt (fastapi,
starlette, uvicorn, websockets, numpy). No GPU, no NeMo. (An older uvicorn
sends the 503 denial with Content-Length twice and RealServerTest fails.)

The model is NEVER loaded: app.make_engine is replaced with StubEngine, which
"decodes" every whole 560 ms chunk into one fixture word, constant chunks
included (as the real model invents text for a dead capture). What this pins
is everything that is ours: the bearer check, the capacity refusal BEFORE the
upgrade with Retry-After, the frame and message rules, flush and end
semantics, padding trimmed from timestamps, the constant-signal rule, batching
across streams, the backlog cap, the sample-rate refusal, and that the real
uvicorn server sends the refusals as HTTP statuses.
"""

import asyncio
import faulthandler
import importlib
import json
import math
import os
import pathlib
import re
import socket
import sys
import threading
import time
import types
import unittest

import numpy as np

HERE = pathlib.Path(__file__).resolve().parent
TOKEN = "nemo-fixture-token-9c2e"
# Fixture values distinct from every default in app.py (8 streams, 30 s,
# 30 s backlog), so a mutant that hard-codes a default cannot pass.
MAX = 3
RETRY = 47
CHUNK = 8960  # the stub's chunk: 560 ms, the real model's chunk size
FRAME = 1280
WORD = "fx7741w"
# A flush's final is held for the next step or this long (app default 1.0 s).
# Short, so the tests that wait for a final with no audio after the flush stay
# fast; the late-mark tests use LONG, so only a model step can release a hold.
HOLD = 0.3
LONG = 5.0


class StubEngine:
    """One fixture word per whole chunk; on `end`, the zero-padded remainder
    is one more chunk. Records every batch it was asked to step."""

    sample_rate = 16000

    def __init__(self):
        self.batches = []
        self.gate = None  # a threading.Event that step() waits on, when set
        self.fail = False
        self.closed = []

    def new_stream(self):
        return types.SimpleNamespace(buf=np.zeros(0, np.float32), toks=[], decoded=0, mark=0, closed=0)

    def push(self, s, pcm):
        assert pcm.dtype == np.float32 and float(np.abs(pcm).max(initial=0)) <= 1.0
        s.buf = np.concatenate([s.buf, pcm])

    def runnable(self, s, ending):
        return s.buf.size >= CHUNK or (ending and s.buf.size > 0)

    def step(self, states, ending):
        self.batches.append(len(states))
        if self.gate is not None:
            self.gate.wait(10)
        if self.fail:
            raise RuntimeError("stub step failure")
        for s in states:
            s.buf = s.buf[CHUNK:]
            s.toks.append(len(s.toks) + 1)
            s.decoded += CHUNK

    words = None  # a script: chunk i decodes to words[i-1] (else WORD + i)

    def segment_text(self, s):
        if self.words:
            return " ".join(self.words[i - 1] for i in s.toks[s.mark:])
        return " ".join(f"{WORD}{i}" for i in s.toks[s.mark:])

    def cut(self, s):
        s.mark = len(s.toks)

    def close(self, s):
        s.closed += 1
        self.closed.append(s)

    def decoded_samples(self, s):
        return s.decoded


def load_app(token=TOKEN, engine=None, **env):
    """Import app.py fresh (its settings are read at import) with the stub."""
    os.environ["STT_API_TOKEN"] = token
    os.environ["STT_MAX_STREAMS"] = str(env.get("max_streams", MAX))
    os.environ["STT_RETRY_AFTER"] = str(env.get("retry", RETRY))
    os.environ["STT_MAX_BACKLOG_SECONDS"] = str(env.get("backlog", 30))
    os.environ["STT_IDLE_SECONDS"] = str(env.get("idle", 300))
    os.environ["STT_FINAL_HOLD_SECONDS"] = str(env.get("hold", HOLD))
    sys.path.insert(0, str(HERE))
    try:
        sys.modules.pop("app", None)
        mod = importlib.import_module("app")
    finally:
        sys.path.remove(str(HERE))
    eng = engine or StubEngine()
    mod.make_engine = lambda: eng
    return mod, eng


def tone(n, start=0):
    i = np.arange(start, start + n)
    return (8000 * np.sin(2 * math.pi * 440 * i / 16000)).astype("<i2")


def frames(pcm):
    """Split S16LE samples into 80 ms frames, zero-padding the last."""
    pad = (-pcm.size) % FRAME
    full = np.concatenate([pcm.astype("<i2"), np.zeros(pad, "<i2")])
    return [full[i:i + FRAME].tobytes() for i in range(0, full.size, FRAME)]


def auth(token=TOKEN):
    return {"Authorization": f"Bearer {token}"}


def until_close(ws):
    """Every message the server sends, then the close code."""
    msgs = []
    while True:
        m = ws.receive()
        if m["type"] == "websocket.close":
            return msgs, m.get("code")
        msgs.append(json.loads(m["text"]))


def next_of(ws, kind):
    """Skip messages until one of `kind` arrives."""
    while True:
        m = ws.receive()
        if m["type"] == "websocket.close":
            raise AssertionError(f"closed {m.get('code')} while waiting for {kind}")
        d = json.loads(m["text"])
        if d["type"] == kind:
            return d


class Timeout:
    """Fail, never hang: a broken server must not wedge the suite.

    A blocked TestClient receive is not interrupted by SIGALRM (measured: a
    mutant that crashed the handler hung the suite for 10 minutes), so the
    watchdog dumps every thread's stack and EXITS the process non-zero."""

    def __init__(self, s=20):
        self.s = s

    def __enter__(self):
        def boom():
            sys.stderr.write(f"\nFAIL: a test exceeded {self.s}s; the suite is aborted\n")
            faulthandler.dump_traceback(all_threads=True)
            os._exit(3)
        self.timer = threading.Timer(self.s, boom)
        self.timer.daemon = True
        self.timer.start()

    def __exit__(self, *a):
        self.timer.cancel()


class Base(unittest.TestCase):
    env: dict = {}

    def setUp(self):
        from fastapi.testclient import TestClient

        self.t = Timeout()
        self.t.__enter__()
        self.app, self.eng = load_app(**self.env)
        self.ctx = TestClient(self.app.app)
        self.client = self.ctx.__enter__()

    def tearDown(self):
        self.eng.gate = None
        self.ctx.__exit__(None, None, None)
        self.t.__exit__()

    def connect(self, token=TOKEN):
        return self.client.websocket_connect("/v1/stream", headers=auth(token) if token else {})

    def denied(self, token=TOKEN):
        """The HTTP refusal of an upgrade. Fails if the socket was UPGRADED
        (accepted, even if closed straight after)."""
        from starlette.testclient import WebSocketDenialResponse

        try:
            with self.connect(token) as ws:
                ws.close()
        except WebSocketDenialResponse as r:
            return r
        self.fail("the upgrade was accepted; it must be refused with an HTTP status before the upgrade")


class HealthTest(Base):
    def test_readyz_once_loaded_reports_capacity(self):
        r = self.client.get("/readyz")
        self.assertEqual(r.status_code, 200, r.text)
        self.assertEqual(r.json()["max_streams"], MAX)
        self.assertEqual(r.json()["streams"], 0)
        self.assertEqual(self.client.get("/healthz").status_code, 200)


class AuthTest(Base):
    def test_missing_bearer_is_401_before_the_upgrade(self):
        self.assertEqual(self.denied(token=None).status_code, 401)

    def test_wrong_bearer_is_401_before_the_upgrade(self):
        self.assertEqual(self.denied(token="not-the-fixture-token").status_code, 401)

    def test_a_token_that_only_prefixes_the_real_one_is_401(self):
        self.assertEqual(self.denied(token=TOKEN[:-1]).status_code, 401)

    def test_another_scheme_is_401(self):
        from starlette.testclient import WebSocketDenialResponse

        with self.assertRaises(WebSocketDenialResponse) as cm:
            with self.client.websocket_connect("/v1/stream", headers={"Authorization": f"Basic {TOKEN}"}):
                pass
        self.assertEqual(cm.exception.status_code, 401)

    def test_the_right_bearer_is_upgraded(self):
        with self.connect() as ws:
            ws.send_text(json.dumps({"type": "end", "samples": 0}))
            msgs, code = until_close(ws)
        self.assertEqual(code, 1000)
        self.assertEqual(msgs[-1]["type"], "final")


class NoTokenTest(unittest.TestCase):
    """Without STT_API_TOKEN nothing is served: a deployment missing its secret
    fails readiness instead of serving an open endpoint."""

    def test_no_token_is_503_everywhere(self):
        from fastapi.testclient import TestClient
        from starlette.testclient import WebSocketDenialResponse

        with Timeout():
            app, _ = load_app(token="")
            with TestClient(app.app) as client:
                self.assertEqual(client.get("/readyz").status_code, 503)
                with self.assertRaises(WebSocketDenialResponse) as cm:
                    with client.websocket_connect("/v1/stream", headers=auth("anything")):
                        pass
                self.assertEqual(cm.exception.status_code, 503)


class CapacityTest(Base):
    def test_the_stream_past_the_cap_is_503_with_retry_after_and_never_upgraded(self):
        opened = [self.connect() for _ in range(MAX)]
        socks = [c.__enter__() for c in opened]
        try:
            self.assertEqual(self.client.get("/readyz").json()["streams"], MAX)
            r = self.denied()
            self.assertEqual(r.status_code, 503)
            self.assertEqual(r.headers.get("retry-after"), str(RETRY))
        finally:
            for c in opened:
                c.__exit__(None, None, None)
        del socks

    def test_a_closed_stream_frees_its_slot(self):
        for _ in range(MAX + 2):  # more streams in sequence than the cap
            with self.connect() as ws:
                ws.send_text(json.dumps({"type": "end", "samples": 0}))
                _, code = until_close(ws)
            self.assertEqual(code, 1000)
        deadline = time.time() + 5
        while self.client.get("/readyz").json()["streams"] and time.time() < deadline:
            time.sleep(0.01)
        self.assertEqual(self.client.get("/readyz").json()["streams"], 0)

    def test_a_refused_upgrade_does_not_take_a_slot(self):
        opened = [self.connect() for _ in range(MAX)]
        for c in opened:
            c.__enter__()
        for _ in range(3):
            self.assertEqual(self.denied().status_code, 503)
        opened[0].__exit__(None, None, None)
        deadline = time.time() + 5
        while self.client.get("/readyz").json()["streams"] >= MAX and time.time() < deadline:
            time.sleep(0.01)
        try:
            with self.connect() as ws:  # exactly one slot is free again
                ws.send_text(json.dumps({"type": "end", "samples": 0}))
                _, code = until_close(ws)
            self.assertEqual(code, 1000)
        finally:
            for c in opened[1:]:
                c.__exit__(None, None, None)

    def test_an_unauthenticated_upgrade_at_capacity_is_401_not_503(self):
        opened = [self.connect() for _ in range(MAX)]
        for c in opened:
            c.__enter__()
        try:
            self.assertEqual(self.denied(token="not-the-fixture-token").status_code, 401)
        finally:
            for c in opened:
                c.__exit__(None, None, None)


class FrameTest(Base):
    def test_a_frame_of_any_other_size_is_bad_frame_and_1003(self):
        for size in (2559, 2561, 0, 5120):
            with self.subTest(size=size), self.connect() as ws:
                ws.send_bytes(b"\x01" * size)
                msgs, code = until_close(ws)
                self.assertEqual(code, 1003)
                self.assertEqual([m["code"] for m in msgs if m["type"] == "error"], ["bad_frame"])

    def test_an_unknown_text_frame_is_bad_message_and_1003(self):
        for body in ('{"type": "flsh"}', "not json", "[]", '{"type": null}'):
            with self.subTest(body=body), self.connect() as ws:
                ws.send_text(body)
                msgs, code = until_close(ws)
                self.assertEqual(code, 1003)
                self.assertEqual(msgs[-1]["code"], "bad_message")


class FlushTest(Base):
    def test_flush_finals_only_the_text_since_the_previous_flush(self):
        with self.connect() as ws:
            for f in frames(tone(2 * CHUNK)):
                ws.send_bytes(f)
            ws.send_text('{"type": "flush"}')
            first = next_of(ws, "final")
            for f in frames(tone(CHUNK, start=2 * CHUNK)):
                ws.send_bytes(f)
            ws.send_text('{"type": "flush"}')
            second = next_of(ws, "final")
            ws.send_text(json.dumps({"type": "end", "samples": 3 * CHUNK}))
            rest, code = until_close(ws)
        self.assertEqual(first, {"type": "final", "text": f"{WORD}1 {WORD}2", "start_ms": 0, "end_ms": 1120})
        self.assertEqual(second, {"type": "final", "text": f"{WORD}3", "start_ms": 1120, "end_ms": 1680})
        # `end` with no audio since the last flush: an empty final, then 1000.
        self.assertEqual(rest[-1], {"type": "final", "text": "", "start_ms": 1680, "end_ms": 1680})
        self.assertEqual(code, 1000)

    def test_partials_carry_the_segment_so_far_in_audio_time(self):
        with self.connect() as ws:
            for f in frames(tone(2 * CHUNK)):
                ws.send_bytes(f)
            p1 = next_of(ws, "partial")
            p2 = next_of(ws, "partial")
            ws.send_text(json.dumps({"type": "end", "samples": 2 * CHUNK}))
            until_close(ws)
        self.assertEqual(p1, {"type": "partial", "text": f"{WORD}1", "start_ms": 0, "end_ms": 560})
        self.assertEqual(p2, {"type": "partial", "text": f"{WORD}1 {WORD}2", "start_ms": 0, "end_ms": 1120})

    def test_end_trims_the_last_frames_padding_from_the_final(self):
        n = CHUNK + 500  # the last frame carries 500 real samples, 780 of padding
        with self.connect() as ws:
            for f in frames(tone(n)):
                ws.send_bytes(f)
            ws.send_text(json.dumps({"type": "end", "samples": n}))
            msgs, code = until_close(ws)
        final = [m for m in msgs if m["type"] == "final"]
        self.assertEqual(final, [{"type": "final", "text": f"{WORD}1 {WORD}2", "start_ms": 0, "end_ms": n * 1000 // 16000}])
        self.assertEqual(code, 1000)

    def test_end_samples_true_is_bad_end_even_where_1_is_in_range(self):
        # After ONE frame the valid range is 1..1280, so `true` (== 1 in
        # Python) passes the range check: only the bool check refuses it.
        with self.connect() as ws:
            ws.send_bytes(frames(tone(FRAME))[0])
            ws.send_text(json.dumps({"type": "end", "samples": True}))
            msgs, code = until_close(ws)
        self.assertEqual(code, 1003)
        self.assertEqual(msgs[-1]["code"], "bad_end")

    def test_end_samples_outside_the_last_frame_is_bad_end(self):
        n = FRAME * 3
        for samples in (n + 1, n - FRAME, -1, "3840", True, None):
            with self.subTest(samples=samples), self.connect() as ws:
                for f in frames(tone(n)):
                    ws.send_bytes(f)
                ws.send_text(json.dumps({"type": "end", "samples": samples}))
                msgs, code = until_close(ws)
                self.assertEqual(code, 1003)
                self.assertEqual(msgs[-1]["code"], "bad_end")


class LeadingPunctuationTest(Base):
    """The model emits a sentence's closing mark one step late, so after a
    flush it opens the NEXT segment. The flush's final is held for that step
    and the mark moved back onto it; no segment's text starts with a mark."""

    env = {"hold": LONG}  # only a model step (or flush/end) releases a hold here

    def _run(self, words, cuts, tail=0):
        """One chunk per word, a flush after each chunk count in `cuts`, then
        `tail` more chunks and `end`. Everything is sent before anything is
        read, so the message order is the decode thread's alone. Returns
        (finals, partials per segment)."""
        self.eng.words = words
        sent = 0
        with self.connect() as ws:
            for n in cuts:
                for f in frames(tone(n * CHUNK, start=sent * CHUNK)):
                    ws.send_bytes(f)
                sent += n
                ws.send_text('{"type": "flush"}')
            for f in frames(tone(tail * CHUNK, start=sent * CHUNK)):
                ws.send_bytes(f)
            sent += tail
            ws.send_text(json.dumps({"type": "end", "samples": sent * CHUNK}))
            msgs, code = until_close(ws)
        self.assertEqual(code, 1000)
        finals, partials = [], [[]]
        for m in msgs:
            if m["type"] == "final":
                finals.append(m["text"])
                partials.append([])
            else:
                partials[-1].append(m["text"])
        return finals, partials[:-1]

    def test_a_question_mark_decoded_after_the_flush_closes_its_own_final(self):
        finals, partials = self._run(
            ["Can", "Carol", "book", "the room", "?", "Thanks", "everyone."], [4], tail=3)
        self.assertEqual(finals, ["Can Carol book the room?", "Thanks everyone."])
        # The late "?" alone was never a partial; the next one starts at the word.
        self.assertEqual(partials[1], ["Thanks", "Thanks everyone."])

    def test_a_mark_decoded_with_the_next_word_moves_back_and_the_word_stays(self):
        finals, partials = self._run(["Carol books the room", ". Thanks", "everyone."], [1], tail=2)
        self.assertEqual(finals, ["Carol books the room.", "Thanks everyone."])
        self.assertEqual(partials[1], ["Thanks", "Thanks everyone."])

    def test_every_leading_mark_moves_back_and_inner_ones_stay(self):
        finals, _ = self._run(["Hi", "?! \u2026;:,", "ok, so.", "no:", "fine!"], [1], tail=4)
        self.assertEqual(finals, ["Hi?!\u2026;:,", "ok, so. no: fine!"])

    def test_end_with_a_hold_pending_finals_both_and_closes(self):
        # The mark is decoded by the steps `end` runs.
        finals, _ = self._run(["Done", "?"], [1], tail=1)
        self.assertEqual(finals, ["Done?", ""])
        # No audio after the flush: no step runs, the hold goes out as it is.
        finals, _ = self._run(["Done", "?"], [1], tail=0)
        self.assertEqual(finals, ["Done", ""])

    def test_a_mark_only_segment_adds_no_text_segment(self):
        # One final per flush (the contract), but the mark-only segment is
        # empty, sends no partial, and its mark closed the sentence before it.
        finals, partials = self._run(["Done", ".", "Next"], [1, 1, 1])
        self.assertEqual(finals, ["Done.", "", "Next", ""])
        self.assertEqual(partials[1], [])
        self.assertEqual(partials[2], ["Next"])
        # A late mark never becomes an empty final's whole text.
        finals, _ = self._run(["", "?"], [1], tail=1)
        self.assertEqual(finals, ["", ""])

    def test_a_second_flush_before_any_step_sends_the_first_hold_in_order(self):
        finals, _ = self._run(["Hi"], [1, 0])
        self.assertEqual(finals, ["Hi", "", ""])

    def test_a_mark_two_steps_late_is_dropped_not_leaked(self):
        # The backstop: the step after the flush decoded nothing, so the hold
        # went out bare; the mark that comes a step later must not open the
        # segment (nor be appended to a final already sent).
        finals, partials = self._run(["Hi", "", ". Bye"], [1], tail=2)
        self.assertEqual(finals, ["Hi", "Bye"])
        self.assertEqual(partials[1], ["Bye"])

    def test_the_next_step_releases_the_held_final_without_waiting_for_the_timeout(self):
        self.eng.words = ["Hi", "there"]
        with self.connect() as ws:
            for f in frames(tone(CHUNK)):
                ws.send_bytes(f)
            ws.send_text('{"type": "flush"}')
            t0 = time.monotonic()
            for f in frames(tone(CHUNK, start=CHUNK)):
                ws.send_bytes(f)
            first = next_of(ws, "final")
            waited = time.monotonic() - t0
            ws.send_text(json.dumps({"type": "end", "samples": 2 * CHUNK}))
            until_close(ws)
        self.assertEqual(first["text"], "Hi")
        self.assertLess(waited, LONG / 2, "the held final waited for the timeout, not the next step")

    def test_strip_leading_punct_never_touches_a_word_start(self):
        """INVARIANT GUARD, not regression coverage: these inputs pin what the
        backstop must leave alone (and what it must drop); the bug never
        involved a word start being stripped."""
        for text, want in [
            (". Thanks everyone.", "Thanks everyone."),
            (" .. Good morning, everyone.", "Good morning, everyone."),
            ("Good morning. Alice will send it.", "Good morning. Alice will send it."),
            ("'tis fine", "'tis fine"),
            ("3.5 percent", "3.5 percent"),
            ("", ""),
        ]:
            with self.subTest(text=text):
                self.assertEqual(self.app.strip_leading_punct(text), want)


class HoldTimeoutTest(Base):
    def test_a_flush_with_no_audio_after_it_finals_within_the_hold(self):
        with self.connect() as ws:
            for f in frames(tone(CHUNK)):
                ws.send_bytes(f)
            ws.send_text('{"type": "flush"}')
            t0 = time.monotonic()
            final = next_of(ws, "final")  # nothing more is sent: only the timeout can release it
            waited = time.monotonic() - t0
            ws.send_text(json.dumps({"type": "end", "samples": CHUNK}))
            until_close(ws)
        self.assertEqual(final["text"], f"{WORD}1")
        self.assertLess(waited, HOLD + 2.0)


class ConstantSignalTest(Base):
    def test_a_constant_stream_yields_no_text(self):
        dead = np.full(2 * CHUNK, 1234, "<i2")
        with self.connect() as ws:
            for f in frames(dead):
                ws.send_bytes(f)
            ws.send_text('{"type": "flush"}')
            # EVERY message up to and including the flush's final: the
            # partials this rule suppresses would arrive BEFORE the final.
            before = []
            while not before or before[-1]["type"] != "final":
                before.append(json.loads(ws.receive_text()))
            ws.send_text(json.dumps({"type": "end", "samples": 2 * CHUNK}))
            after, _ = until_close(ws)
        # The stub "decoded" two words; the server must drop them.
        self.assertGreaterEqual(sum(self.eng.batches), 2, "control: the stub never stepped")
        self.assertEqual([m for m in before + after if m["type"] == "partial"], [],
                         "a dead (constant) stream produced partials")
        self.assertEqual(before[-1]["text"], "")

    def test_a_constant_stream_with_a_zero_padded_last_frame_still_yields_no_text(self):
        n = CHUNK + 300
        dead = np.full(n, -77, "<i2")
        with self.connect() as ws:
            for f in frames(dead):  # the padding zeros differ from -77
                ws.send_bytes(f)
            ws.send_text(json.dumps({"type": "end", "samples": n}))
            msgs, code = until_close(ws)
        self.assertEqual([m["text"] for m in msgs if m["type"] == "final"], [""])
        self.assertEqual(code, 1000)

    def test_a_constant_nonzero_stream_sends_no_partials_while_its_padded_last_frame_awaits_end(self):
        # 3 chunks less half a frame: 21 frames, the 21st carrying 640 real
        # samples and 640 of zero padding. That frame COMPLETES the third
        # chunk, so the third model step always runs with it already received
        # and before `end` is answered (a step outranks a queued command):
        # its padding is in the stream before the server can know it is
        # padding. The value is non-zero so the padding differs from it.
        n = 3 * CHUNK - FRAME // 2
        dead = np.full(n, 2311, "<i2")
        with self.connect() as ws:
            for f in frames(dead):
                ws.send_bytes(f)
            ws.send_text(json.dumps({"type": "end", "samples": n}))
            msgs, code = until_close(ws)
        self.assertGreaterEqual(sum(self.eng.batches), 3, "control: the stub never decoded the three chunks")
        self.assertEqual([m for m in msgs if m["type"] == "partial"], [],
                         "the padding of the last frame made a constant stream send partials")
        self.assertEqual([m["text"] for m in msgs if m["type"] == "final"], [""])
        self.assertEqual(code, 1000)

    def test_positive_control_speech_after_a_dead_segment_is_transcribed(self):
        with self.connect() as ws:
            for f in frames(np.zeros(CHUNK, "<i2")):
                ws.send_bytes(f)
            ws.send_text('{"type": "flush"}')
            dead = next_of(ws, "final")
            for f in frames(tone(CHUNK)):
                ws.send_bytes(f)
            ws.send_text('{"type": "flush"}')
            live = next_of(ws, "final")
            ws.send_text(json.dumps({"type": "end", "samples": 2 * CHUNK}))
            until_close(ws)
        self.assertEqual(dead["text"], "")
        self.assertEqual(live["text"], f"{WORD}2")


class BatchingTest(Base):
    def test_waiting_streams_are_decoded_in_one_batch(self):
        gate = threading.Event()
        self.eng.gate = gate
        conns = [self.connect() for _ in range(MAX)]
        socks = [c.__enter__() for c in conns]
        try:
            for f in frames(tone(CHUNK)):
                socks[0].send_bytes(f)
            time.sleep(0.3)  # stream 0's step is now held at the gate
            for ws in socks:
                for f in frames(tone(CHUNK)):
                    ws.send_bytes(f)
            time.sleep(0.3)
            gate.set()
            for ws in socks[1:]:
                next_of(ws, "partial")
        finally:
            for c in conns:
                c.__exit__(None, None, None)
        self.assertEqual(self.eng.batches[0], 1)
        self.assertEqual(max(self.eng.batches), MAX, self.eng.batches)


class BacklogTest(Base):
    env = {"backlog": 1.0}

    def test_a_stream_that_outruns_the_decoder_is_closed_1013(self):
        gate = threading.Event()
        self.eng.gate = gate
        try:
            with self.connect() as ws:
                for f in frames(tone(2 * CHUNK)):  # 1.12 s > the 1.0 s cap
                    ws.send_bytes(f)
                msgs = []
                while True:
                    m = ws.receive()
                    if m["type"] == "websocket.close":
                        code = m.get("code")
                        break
                    msgs.append(json.loads(m["text"]))
        finally:
            gate.set()
        self.assertEqual(code, 1013)
        self.assertEqual(msgs[-1]["code"], "overloaded")


class IdleTest(Base):
    env = {"idle": 0.5}

    def test_silent_peers_are_closed_and_their_slots_freed(self):
        # Fill every slot with a peer that sends a frame and then goes silent
        # WITHOUT closing (as a dead peer behind a lost connection does).
        conns = [self.connect() for _ in range(MAX)]
        socks = [c.__enter__() for c in conns]
        try:
            for ws in socks:
                ws.send_bytes(frames(tone(FRAME))[0])
            self.assertEqual(self.client.get("/readyz").json()["streams"], MAX)
            deadline = time.time() + 5
            while self.client.get("/readyz").json()["streams"] and time.time() < deadline:
                time.sleep(0.05)
            held = self.client.get("/readyz").json()["streams"]
            self.assertEqual(held, 0, f"silent peers still hold {held} of {MAX} slots after 5 s")
            with self.connect() as ws:  # a new stream is accepted
                ws.send_text(json.dumps({"type": "end", "samples": 0}))
                _, code = until_close(ws)
            self.assertEqual(code, 1000)
            msgs, code = until_close(socks[0])
            self.assertEqual((msgs[-1]["code"], code), ("idle", 1001))
        finally:
            for c in conns:
                c.__exit__(None, None, None)


class StepFailureTest(Base):
    def test_a_failed_model_step_is_an_error_and_1011(self):
        self.eng.fail = True
        with self.connect() as ws:
            for f in frames(tone(CHUNK)):
                ws.send_bytes(f)
            msgs, code = until_close(ws)
        self.assertEqual(code, 1011)
        self.assertEqual(msgs[-1]["code"], "internal")


class DropTest(Base):
    def test_a_stream_dropped_without_end_is_freed_in_the_engine(self):
        with self.connect() as ws:
            for f in frames(tone(CHUNK)):
                ws.send_bytes(f)
            next_of(ws, "partial")
            es = None
        deadline = time.time() + 5
        while not self.eng.closed and time.time() < deadline:
            time.sleep(0.01)
        self.assertEqual(len(self.eng.closed), 1)
        self.assertEqual(self.eng.closed[0].toks, [1])  # the stream that was dropped
        del es


class SampleRateTest(unittest.TestCase):
    def test_a_model_that_is_not_16k_refuses_to_start(self):
        from fastapi.testclient import TestClient

        eng = StubEngine()
        eng.sample_rate = 22050
        app, _ = load_app(engine=eng)
        with self.assertRaisesRegex(RuntimeError, "22050"):
            with TestClient(app.app):
                pass

    def test_positive_control_a_16k_model_starts(self):
        from fastapi.testclient import TestClient

        app, _ = load_app()
        with TestClient(app.app) as c:
            self.assertEqual(c.get("/readyz").status_code, 200)


class RealServerTest(unittest.TestCase):
    """The refusals as a real client sees them from real uvicorn: an HTTP
    status on the upgrade request, never a 101. (TestClient emulates the ASGI
    denial extension; this proves the server we ship implements it.)"""

    def test_uvicorn_answers_401_and_503_with_retry_after_before_the_upgrade(self):
        import uvicorn
        import websockets
        from websockets.exceptions import InvalidStatus

        with Timeout(30):
            app, _ = load_app(max_streams=1)
            with socket.socket() as s:
                s.bind(("127.0.0.1", 0))
                port = s.getsockname()[1]
            cfg = {**app.server_config(), "host": "127.0.0.1", "port": port, "log_level": "warning"}
            server = uvicorn.Server(uvicorn.Config(app.app, **cfg))
            th = threading.Thread(target=server.run, daemon=True)
            th.start()
            try:
                deadline = time.time() + 10
                while not server.started and time.time() < deadline:
                    time.sleep(0.05)
                url = f"ws://127.0.0.1:{port}/v1/stream"

                async def scenario():
                    with self.assertRaises(InvalidStatus) as cm:
                        async with websockets.connect(url, additional_headers=auth("not-the-fixture-token")):
                            pass
                    unauth = cm.exception.response.status_code
                    async with websockets.connect(url, additional_headers=auth()) as first:
                        with self.assertRaises(InvalidStatus) as cm:
                            async with websockets.connect(url, additional_headers=auth()):
                                pass
                        full = cm.exception.response
                        await first.send(json.dumps({"type": "end", "samples": 0}))
                        final = json.loads(await first.recv())
                    return unauth, full.status_code, full.headers.get("Retry-After"), final

                unauth, status, retry, final = asyncio.run(scenario())
                self.assertEqual(unauth, 401)
                self.assertEqual(status, 503)
                self.assertEqual(retry, str(RETRY))
                self.assertEqual(final["type"], "final")
            finally:
                server.should_exit = True
                th.join(10)

    def test_server_config_turns_pings_off(self):
        app, _ = load_app()
        cfg = app.server_config()
        self.assertIsNone(cfg["ws_ping_interval"])
        self.assertIsNone(cfg["ws_ping_timeout"])


class BenchmarkedConfigTest(unittest.TestCase):
    """The model, attention context and precision are the benchmarked ones and
    NO environment variable changes them: the weights always come from the
    pinned, hash-checked download, at [70, 6] (560 ms chunks) in bf16.

    nemo_engine.py is imported with torch, omegaconf and NeMo stubbed (only
    their names are needed to import it); the engine is never built."""

    STUBBED = (
        "torch", "omegaconf", "nemo", "nemo.collections", "nemo.collections.asr",
        "nemo.collections.asr.inference", "nemo.collections.asr.inference.factory",
        "nemo.collections.asr.inference.factory.cache_aware_pipeline_builder",
        "nemo.collections.asr.inference.streaming", "nemo.collections.asr.inference.streaming.framing",
        "nemo.collections.asr.inference.streaming.framing.request",
        "nemo.collections.asr.inference.streaming.framing.request_options",
        "nemo.collections.asr.inference.utils", "nemo.collections.asr.inference.utils.pipeline_utils",
    )

    def load_engine_module(self):
        saved = {n: sys.modules.get(n) for n in self.STUBBED + ("nemo_engine",)}
        self.addCleanup(lambda: [sys.modules.pop(n, None) if m is None else sys.modules.__setitem__(n, m)
                                 for n, m in saved.items()])
        for n in self.STUBBED:
            sys.modules[n] = types.ModuleType(n)
        sys.modules["torch"].cuda = types.SimpleNamespace(is_available=lambda: False)
        sys.modules["omegaconf"].OmegaConf = types.SimpleNamespace(create=lambda d: d)
        base = "nemo.collections.asr.inference."
        sys.modules[base + "factory.cache_aware_pipeline_builder"].CacheAwarePipelineBuilder = object
        sys.modules[base + "streaming.framing.request"].Frame = object
        sys.modules[base + "streaming.framing.request_options"].ASRRequestOptions = object
        sys.modules[base + "utils.pipeline_utils"].ids_to_text_without_stripping = None
        sys.modules.pop("nemo_engine", None)
        sys.path.insert(0, str(HERE))
        try:
            return importlib.import_module("nemo_engine")
        finally:
            sys.path.remove(str(HERE))

    def test_the_constants_are_the_benchmarked_values(self):
        ne = self.load_engine_module()
        self.assertEqual(ne.ATT_CONTEXT, [70, 6])
        self.assertEqual(ne.COMPUTE_DTYPE, "bfloat16")

    def test_environment_cannot_change_model_context_or_precision(self):
        ne = self.load_engine_module()
        fetched = []
        ne.fetch_model = lambda: fetched.append(1) or "/fixture/pinned-9d1.nemo"
        built = []
        ne.NemotronEngine.__init__ = lambda self, *a: built.append(a)
        # Values a removed knob would have honoured, each distinct from the
        # benchmarked one; the stream cap (still a setting) is distinct from 8.
        env = {"STT_MODEL": "/fixture/other-model.nemo", "STT_ATT_CONTEXT": "70,1",
               "STT_DTYPE": "fp32", "STT_MAX_STREAMS": "5"}
        old = {k: os.environ.get(k) for k in env}
        os.environ.update(env)
        try:
            ne.NemotronEngine.from_env()
        finally:
            for k, v in old.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v
        self.assertEqual(fetched, [1], "the pinned, hash-checked download must always be used")
        self.assertEqual(built, [("/fixture/pinned-9d1.nemo", 5, [70, 6], "bfloat16")])


class EngineSlotTest(BenchmarkedConfigTest):
    """nemo_engine's slot bookkeeping against a fake NeMo pipeline."""

    test_the_constants_are_the_benchmarked_values = None
    test_environment_cannot_change_model_context_or_precision = None

    def test_a_failed_last_step_still_frees_the_slots_on_close(self):
        ne = self.load_engine_module()
        ne.torch = types.SimpleNamespace(from_numpy=lambda a: a)
        ne.Frame = lambda **k: types.SimpleNamespace(**k)
        ne.ASRRequestOptions = lambda: None
        freed, deleted = [], []

        class Pipe:
            bufferer = types.SimpleNamespace(streamidx2slotidx={}, free_slots=lambda ids: freed.append(("feat", ids)))
            context_manager = types.SimpleNamespace(streamidx2slotidx={}, _reset_slots=lambda ids: freed.append(("cache", ids)))

            def init_state(self, sid, opts):
                return types.SimpleNamespace(tokens=[])

            def transcribe_step(self, frames):
                raise RuntimeError("fixture: the last step failed")

            def delete_state(self, sid):
                deleted.append(sid)

        eng = object.__new__(ne.NemotronEngine)
        eng.pipeline, eng.chunk, eng._next = Pipe(), CHUNK, 0
        es = eng.new_stream()
        es.frames, es.state = 1, types.SimpleNamespace(tokens=[])  # one step already ran
        eng.pipeline.context_manager.streamidx2slotidx[es.sid] = 6
        eng.push(es, np.full(300, 0.1, np.float32))
        self.assertTrue(eng.runnable(es, True), "control: the last step is due")
        with self.assertRaisesRegex(RuntimeError, "fixture"):
            eng.step([es], [True])
        eng.close(es)
        self.assertEqual((freed, deleted), ([("cache", [6])], [es.sid]),
                         "a stream whose last step failed kept its NeMo slot")


class RequirementsArePinnedTest(unittest.TestCase):
    def test_every_requirement_is_an_exact_pin(self):
        lines = [
            l.strip() for l in (HERE / "requirements.txt").read_text().splitlines()
            if l.strip() and not l.strip().startswith("#")
        ]
        self.assertGreater(len(lines), 0, "control: no requirement lines parsed")
        loose = [l for l in lines if not re.fullmatch(r"[A-Za-z0-9._\[\]-]+==[A-Za-z0-9.+!-]+", l)]
        self.assertEqual(loose, [], "requirements that are not exact pins")
        names = [l.split("==")[0].lower() for l in lines]
        for direct in ("fastapi", "uvicorn", "websockets", "numpy", "nemo_toolkit[asr]"):
            self.assertIn(direct, names, f"{direct} is not pinned")

    def test_the_model_is_pinned_to_a_commit_and_a_file_hash(self):
        src = (HERE / "nemo_engine.py").read_text()
        rev = re.search(r'^MODEL_REVISION = "([^"]*)"$', src, re.M)
        sha = re.search(r'^MODEL_SHA256 = "([^"]*)"$', src, re.M)
        self.assertIsNotNone(rev, "nemo_engine.py pins no MODEL_REVISION")
        self.assertIsNotNone(sha, "nemo_engine.py pins no MODEL_SHA256")
        self.assertRegex(rev.group(1), r"^[0-9a-f]{40}$", "the revision must be a full commit sha, not a branch")
        self.assertRegex(sha.group(1), r"^[0-9a-f]{64}$")


if __name__ == "__main__":
    unittest.main(verbosity=2)
