"""The stt-cohere server speaks the contract muster's client (internal/stt) uses.

Run: python3 images/stt-cohere/test_app.py
Needs: fastapi, httpx, python-multipart, numpy, and an `ffmpeg` binary on PATH.

The model is NEVER loaded: torch and transformers are replaced with stubs in
sys.modules before app.py is imported, and the stub model "transcribes" every
recording as FAKE_TEXT. What this pins is everything that is ours: the route,
the multipart field names, the bearer check, the status mapping muster reads
(400 undecodable, 401 refused, 413 too long, 503 not ready), the response's
`text` key, and that an extra `model` field (muster sends one when
MUSTER_STT_MODEL is set) is accepted. Real ffmpeg decodes real WAV bytes, so
the decode path is exercised, not stubbed.
"""

import importlib
import io
import math
import os
import pathlib
import shutil
import struct
import sys
import types
import unittest
import wave

HERE = pathlib.Path(__file__).resolve().parent
TOKEN = "stt-fixture-token-4b1d"
# A value no default or literal in app.py could produce, so a test that passes
# proves the model's output reached the response.
FAKE_TEXT = "fixture transcript 7741"


class _Seq(list):
    def tolist(self):
        return [list(r) for r in self]


class _Inputs(dict):
    def to(self, *_a, **_k):
        return self


class _FakeProcessor:
    class tokenizer:  # noqa: N801 - mirrors the attribute the app reads
        @staticmethod
        def get_added_vocab():
            return {"<unk>": 0, "<|nospeech|>": 1, "<|endoftext|>": 3}

    calls = []

    def __call__(self, audio, sampling_rate, return_tensors, language, punctuation):
        _FakeProcessor.calls.append({"language": language, "punctuation": punctuation, "n": len(audio)})
        return _Inputs()

    def decode(self, sequences, **_k):
        return [FAKE_TEXT]


class _FakeModel:
    device = "cpu"
    dtype = "float32"
    generation_config = types.SimpleNamespace(eos_token_id=3)
    config = types.SimpleNamespace(vocab_size=16384)

    def to(self, _device):
        return self

    def eval(self):
        return self

    def generate(self, **_k):
        return types.SimpleNamespace(sequences=_Seq([[5, 6, 3]]), scores=None)


def _install_stubs():
    torch = types.ModuleType("torch")
    torch.cuda = types.SimpleNamespace(is_available=lambda: False)
    torch.bfloat16, torch.float32 = "bfloat16", "float32"
    transformers = types.ModuleType("transformers")
    transformers.AutoProcessor = types.SimpleNamespace(from_pretrained=lambda _id: _FakeProcessor())
    transformers.AutoModelForSpeechSeq2Seq = types.SimpleNamespace(
        from_pretrained=lambda _id, torch_dtype=None: _FakeModel()
    )
    sys.modules["torch"] = torch
    sys.modules["transformers"] = transformers


def _load_app(token):
    """Import app.py fresh with STT_API_TOKEN=token (it is read at import)."""
    _install_stubs()
    os.environ["STT_API_TOKEN"] = token
    sys.path.insert(0, str(HERE))
    try:
        sys.modules.pop("app", None)
        return importlib.import_module("app")
    finally:
        sys.path.remove(str(HERE))


def _wav(samples):
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(b"".join(struct.pack("<h", s) for s in samples))
    return buf.getvalue()


TONE = _wav([int(8000 * math.sin(2 * math.pi * 440 * i / 16000)) for i in range(8000)])
SILENCE = _wav([0] * 8000)


def _muster_request(client, audio, token=TOKEN, **fields):
    """The request internal/stt sends: file + language=en + punctuation=true."""
    data = {"language": "en", "punctuation": "true", **fields}
    headers = {"Authorization": f"Bearer {token}"} if token is not None else {}
    return client.post(
        "/v1/audio/transcriptions",
        files={"file": ("recording.wav", audio)},
        data=data,
        headers=headers,
    )


@unittest.skipUnless(
    shutil.which("ffmpeg") or os.environ.get("STT_TEST_REQUIRE_FFMPEG") == "1",
    "ffmpeg is not on PATH (STT_TEST_REQUIRE_FFMPEG=1 makes this a failure)",
)
class ContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not shutil.which("ffmpeg"):
            raise RuntimeError("ffmpeg is not on PATH and STT_TEST_REQUIRE_FFMPEG=1")
        from fastapi.testclient import TestClient

        cls.app = _load_app(TOKEN)
        cls.ctx = TestClient(cls.app.app)
        cls.client = cls.ctx.__enter__()  # runs the lifespan: the stub "model" loads

    @classmethod
    def tearDownClass(cls):
        cls.ctx.__exit__(None, None, None)

    def test_healthz_is_200_once_loaded(self):
        r = self.client.get("/healthz")
        self.assertEqual(r.status_code, 200, r.text)
        self.assertTrue(r.json()["ok"])

    def test_musters_request_gets_the_models_text(self):
        _FakeProcessor.calls.clear()
        r = _muster_request(self.client, TONE)
        self.assertEqual(r.status_code, 200, r.text)
        self.assertEqual(r.json()["text"], FAKE_TEXT)
        # The fields muster sends reached the processor, decoded from real audio.
        self.assertEqual(_FakeProcessor.calls[-1]["language"], "en")
        self.assertIs(_FakeProcessor.calls[-1]["punctuation"], True)
        self.assertEqual(_FakeProcessor.calls[-1]["n"], 8000)

    def test_an_extra_model_field_is_accepted(self):
        r = _muster_request(self.client, TONE, model="whisper-1")
        self.assertEqual(r.status_code, 200, r.text)
        self.assertEqual(r.json()["text"], FAKE_TEXT)

    def test_a_wrong_or_missing_bearer_is_401(self):
        self.assertEqual(_muster_request(self.client, TONE, token="not-the-token").status_code, 401)
        self.assertEqual(_muster_request(self.client, TONE, token=None).status_code, 401)

    def test_undecodable_bytes_are_400(self):
        r = _muster_request(self.client, b"this is not audio at all")
        self.assertEqual(r.status_code, 400, r.text)

    def test_a_bcp47_language_is_400_naming_the_accepted_codes(self):
        r = _muster_request(self.client, TONE, language="en-US")
        self.assertEqual(r.status_code, 400)
        self.assertIn("'en'", r.text)

    def test_a_dead_capture_is_200_and_empty_and_never_reaches_the_model(self):
        _FakeProcessor.calls.clear()
        r = _muster_request(self.client, SILENCE)
        self.assertEqual(r.status_code, 200, r.text)
        body = r.json()
        self.assertEqual(body["text"], "")
        self.assertEqual(body["unreliable_reasons"], ["constant_signal"])
        self.assertEqual(_FakeProcessor.calls, [])

    def test_over_long_audio_is_413(self):
        old = self.app.MAX_AUDIO_SECONDS
        self.app.MAX_AUDIO_SECONDS = 0.1  # TONE is 0.5 s
        try:
            r = _muster_request(self.client, TONE)
        finally:
            self.app.MAX_AUDIO_SECONDS = old
        self.assertEqual(r.status_code, 413, r.text)


class RequirementsArePinnedTest(unittest.TestCase):
    """Every requirement is `name==version`: an unpinned line lets a rebuild pull
    a different transformers into an image whose model load nothing in CI can
    exercise (no GPU, gated weights)."""

    def test_every_requirement_is_an_exact_pin(self):
        lines = [
            l.strip() for l in (HERE / "requirements.txt").read_text().splitlines()
            if l.strip() and not l.strip().startswith("#")
        ]
        self.assertGreater(len(lines), 0, "control: no requirement lines parsed")
        loose = [l for l in lines if not __import__("re").fullmatch(r"[A-Za-z0-9._-]+==[A-Za-z0-9.+!-]+", l)]
        self.assertEqual(loose, [], "requirements that are not exact pins")
        names = [l.split("==")[0].lower() for l in lines]
        for direct in ("fastapi", "uvicorn", "python-multipart", "librosa", "numpy", "transformers", "huggingface_hub"):
            self.assertIn(direct, names, f"{direct} is not pinned")


class NoTokenTest(unittest.TestCase):
    """Without STT_API_TOKEN the server is unhealthy and refuses every request,
    so a compose stack missing the token fails `--wait` instead of serving an
    open or broken endpoint."""

    def test_no_token_is_503_everywhere(self):
        from fastapi.testclient import TestClient

        app = _load_app("")
        with TestClient(app.app) as client:
            self.assertEqual(client.get("/healthz").status_code, 503)
            r = client.post(
                "/v1/audio/transcriptions",
                files={"file": ("a.wav", TONE)},
                headers={"Authorization": "Bearer anything"},
            )
            self.assertEqual(r.status_code, 503)


if __name__ == "__main__":
    unittest.main(verbosity=2)
