"""Drive a running stt-nemotron server the way the meetings plugin will, and
report accuracy, latency, capacity behaviour and (optionally) GPU memory.

  python measure.py --url ws://127.0.0.1:8000/v1/stream --token T \\
      --streams 8 --manifest meta.json [--gpu] [--per-stream 6] [--json out.json]

The manifest is a JSON list of {"wav": path, "text": reference}: 16 kHz mono
16-bit WAVs. Each of --streams concurrent streams plays --per-stream
utterances back to back IN REAL TIME, in 80 ms S16LE frames, with 1 s of
silence after each; 700 ms into each silence it sends `flush` (what the
plugin's VAD does), and at the end it sends the zero-padded last frame and
`end` with the true sample count. Then it opens one stream more than the
server's cap and records the refusal (expects 503 + Retry-After).

Reported (aggregates only, never a transcript):
  wer               word error rate of all finals against the references
                    (lower-cased, punctuation stripped)
  empty_finals      finals for a spoken utterance that came back empty
  first_partial_s   wall time from a stream's first frame to its first partial
  flush_final_s     wall time from sending `flush` to receiving its `final`
  gpu_mib_peak      peak used GPU memory (nvidia-smi, every 0.5 s) with --gpu;
                    `gpu_mib_idle` is the reading before any stream opened
  over_cap          the status and Retry-After of the stream past the cap
  end_tail_wer      one utterance then `end` with no flush: its final's WER
                    (1.0 = the final came back empty)
  after_drops_nonempty  after 3 rounds of dropping a full set of streams
                    without `end`, how many of a fresh full set transcribed
                    (fewer than all = a leaked slot)
Needs: numpy, websockets.
"""

import argparse
import asyncio
import json
import re
import subprocess
import threading
import time
import wave

import numpy as np
import websockets

FRAME = 1280
RATE = 16000


def load(path):
    with wave.open(path) as w:
        if w.getframerate() != RATE or w.getnchannels() != 1 or w.getsampwidth() != 2:
            raise SystemExit(f"{path}: need 16 kHz mono 16-bit")
        return np.frombuffer(w.readframes(w.getnframes()), "<i2")


_P = re.compile(r"[^\w\s']+")


def words(t):
    return [w.strip("'") for w in _P.sub(" ", t.lower()).split() if w.strip("'")]


def edits(r, h):
    prev = list(range(len(h) + 1))
    for i, a in enumerate(r, 1):
        cur = [i] + [0] * len(h)
        for j, b in enumerate(h, 1):
            cur[j] = min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a != b))
        prev = cur
    return prev[-1]


def pct(xs, p):
    if not xs:
        return None
    s = sorted(xs)
    k = (len(s) - 1) * p
    lo, hi = int(k), min(int(k) + 1, len(s) - 1)
    return round(s[lo] + (s[hi] - s[lo]) * (k - lo), 3)


class Gpu(threading.Thread):
    def __init__(self):
        super().__init__(daemon=True)
        self.peak = 0
        self.stop = False

    @staticmethod
    def read():
        out = subprocess.check_output(
            ["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"], text=True
        )
        return int(out.split()[0])

    def run(self):
        while not self.stop:
            try:
                self.peak = max(self.peak, self.read())
            except Exception:  # noqa: BLE001
                pass
            time.sleep(0.5)


async def one_stream(url, token, utts, out, idx):
    silence = np.zeros(RATE, "<i2")
    flush_at = int(0.7 * RATE)
    sent_flush = []
    finals = []
    first_partial = [None]
    async with websockets.connect(url, additional_headers={"Authorization": f"Bearer {token}"}, max_size=None) as ws:
        t0 = time.monotonic()

        async def recv():
            async for m in ws:
                d = json.loads(m)
                if d["type"] == "partial" and first_partial[0] is None:
                    first_partial[0] = time.monotonic() - t0
                elif d["type"] == "final":
                    finals.append((time.monotonic(), d))
                elif d["type"] == "error":
                    raise RuntimeError(f"server error {d}")

        rt = asyncio.create_task(recv())
        # Build the whole stream with markers: (samples, flush_after?)
        pcm_parts, marks, pos = [], [], 0
        for u in utts:
            pcm_parts.append(u)
            pos += u.size
            marks.append(pos + flush_at)
            pcm_parts.append(silence)
            pos += silence.size
        pcm = np.concatenate(pcm_parts)
        total = pcm.size
        pad = (-total) % FRAME
        padded = np.concatenate([pcm, np.zeros(pad, "<i2")])
        start = time.monotonic()
        mi = 0
        for i in range(0, padded.size, FRAME):
            await ws.send(padded[i:i + FRAME].tobytes())
            end = i + FRAME
            while mi < len(marks) and marks[mi] <= end:
                await ws.send('{"type": "flush"}')
                sent_flush.append(time.monotonic())
                mi += 1
            due = start + end / RATE
            d = due - time.monotonic()
            if d > 0:
                await asyncio.sleep(d)
        await ws.send(json.dumps({"type": "end", "samples": int(total)}))
        await asyncio.wait_for(rt, 60)
    flush_finals = [d for _, d in finals[: len(sent_flush)]]
    lat = [t - s for (t, _), s in zip(finals, sent_flush)]
    out.append({"idx": idx, "finals": [d["text"] for d in flush_finals], "flush_lat": lat, "first_partial": first_partial[0],
                "tail": finals[-1][1]["text"] if len(finals) > len(sent_flush) else None})


async def over_cap(url, token, n):
    """Hold n streams open, then try one more."""
    hdr = {"Authorization": f"Bearer {token}"}
    held = [await websockets.connect(url, additional_headers=hdr) for _ in range(n)]
    try:
        try:
            async with websockets.connect(url, additional_headers=hdr):
                return {"status": 101, "retry_after": None}
        except websockets.exceptions.InvalidStatus as e:
            return {"status": e.response.status_code, "retry_after": e.response.headers.get("Retry-After")}
    finally:
        for w in held:
            await w.close()


async def end_tail(url, token, utt):
    """One utterance, then `end` at once (no silence, no flush): the final
    must carry the words. (Red once: NeMo cleared the tokens at is_last.)"""
    async with websockets.connect(url, additional_headers={"Authorization": f"Bearer {token}"}) as ws:
        pad = (-utt.size) % FRAME
        full = np.concatenate([utt, np.zeros(pad, "<i2")])
        for i in range(0, full.size, FRAME):
            await ws.send(full[i:i + FRAME].tobytes())
        await ws.send(json.dumps({"type": "end", "samples": int(utt.size)}))
        msgs = [json.loads(m) async for m in ws]
    return msgs[-1]


async def drop_then_fill(url, token, cap, utt, rounds=3):
    """Drop `cap` streams mid-audio without `end` (no close handshake), `rounds`
    times, then run `cap` complete streams. A leaked slot makes those fail."""
    hdr = {"Authorization": f"Bearer {token}"}
    for _ in range(rounds):
        socks = [await websockets.connect(url, additional_headers=hdr) for _ in range(cap)]
        for s in socks:
            for i in range(0, FRAME * 20, FRAME):
                await s.send(utt[i:i + FRAME].tobytes())
        await asyncio.sleep(2)
        for s in socks:
            s.transport.abort()
        await asyncio.sleep(1)
    finals = await asyncio.gather(*(end_tail(url, token, utt[:FRAME * 40]) for _ in range(cap)), return_exceptions=True)
    return sum(1 for f in finals if isinstance(f, dict) and f["type"] == "final" and words(f["text"]))


async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--token", required=True)
    ap.add_argument("--streams", type=int, default=8)
    ap.add_argument("--manifest", required=True)
    ap.add_argument("--per-stream", type=int, default=6)
    ap.add_argument("--cap", type=int, help="the server's STT_MAX_STREAMS (default: --streams)")
    ap.add_argument("--gpu", action="store_true")
    ap.add_argument("--json")
    a = ap.parse_args()
    meta = json.load(open(a.manifest))
    gpu = None
    idle = None
    if a.gpu:
        idle = Gpu.read()
        gpu = Gpu()
        gpu.start()
    jobs, refs = [], []
    for s in range(a.streams):
        pick = [meta[(s * a.per_stream + k) % len(meta)] for k in range(a.per_stream)]
        jobs.append([load(m["wav"]) for m in pick])
        refs.append([m["text"] for m in pick])
    out = []
    t0 = time.monotonic()
    await asyncio.gather(*(one_stream(a.url, a.token, j, out, i) for i, j in enumerate(jobs)))
    wall = time.monotonic() - t0
    if gpu:
        gpu.stop = True
    cap = await over_cap(a.url, a.token, a.cap or a.streams)
    utt = load(meta[0]["wav"])
    tail = await end_tail(a.url, a.token, utt)
    refilled = await drop_then_fill(a.url, a.token, a.cap or a.streams, utt)
    e = w = empty = 0
    for res in out:
        r = refs[res["idx"]]
        if len(r) != len(res["finals"]):
            raise SystemExit(f"stream {res['idx']}: {len(res['finals'])} finals for {len(r)} flushes")
        e += sum(edits(words(x), words(y)) for x, y in zip(r, res["finals"]))
        w += sum(len(words(x)) for x in r)
        empty += sum(1 for y in res["finals"] if not words(y))
    lat = [x for r in out for x in r["flush_lat"]]
    fp = [r["first_partial"] for r in out if r["first_partial"] is not None]
    summary = {
        "streams": a.streams, "utterances": a.streams * a.per_stream, "wall_s": round(wall, 1),
        "wer": round(e / w, 4) if w else None, "empty_finals": empty,
        "first_partial_s": {"p50": pct(fp, .5), "p95": pct(fp, .95)},
        "flush_final_s": {"p50": pct(lat, .5), "p95": pct(lat, .95), "max": pct(lat, 1.0)},
        "over_cap": cap,
        "end_tail_wer": round(edits(words(meta[0]["text"]), words(tail["text"])) / max(1, len(words(meta[0]["text"]))), 4),
        "after_drops_nonempty": f"{refilled}/{a.cap or a.streams}",
    }
    if gpu:
        summary["gpu_mib_idle"] = idle
        summary["gpu_mib_peak"] = gpu.peak
    print(json.dumps(summary))
    if a.json:
        json.dump(summary, open(a.json, "w"))


if __name__ == "__main__":
    asyncio.run(main())
