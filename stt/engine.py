"""Kazakh / Russian / code-switched speech-to-text engine.

Wraps the TorchScript wav2vec2-CTC acoustic model and the Silero-style ONNX VAD from
https://huggingface.co/alibiserikbay/kazakh-russian-mixed-stt (Apache-2.0).

    from engine import STTEngine
    engine = STTEngine()                      # loads stt/models/asr/rukk + VAD, picks CUDA if available
    result = engine.transcribe("call.mp3")    # any format ffmpeg can read, or raw bytes, or a 16 kHz float32 array
    print(result.text)

Pipeline: ffmpeg decode -> 16 kHz mono float32 -> VAD splits speech into <= 20 s chunks ->
acoustic model per chunk -> greedy CTC decode -> join.
"""
from __future__ import annotations

import os
import re
import subprocess
import tempfile
import threading
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path

import numpy as np

SAMPLE_RATE = 16000
VAD_WINDOW = 1536  # samples per VAD step (96 ms)
MIN_CHUNK_SAMPLES = 4000  # wav2vec2 conv front-end needs a few hundred samples; pad anything shorter
MODELS_DIR = Path(os.environ.get("STT_MODELS_DIR", Path(__file__).resolve().parent / "models"))
KAZAKH_LETTERS = frozenset("әғқңөұүһі")


# --------------------------------------------------------------------------- audio I/O
def load_audio(source, sample_rate: int = SAMPLE_RATE) -> np.ndarray:
    """Decode a file path or raw encoded bytes (wav/mp3/ogg/webm/m4a/...) to float32 mono at 16 kHz."""
    if isinstance(source, (str, Path)):
        return _ffmpeg_decode(str(source), sample_rate)
    # Bytes: go through a temp file so container formats that need seeking (mp4/m4a) also work.
    with tempfile.NamedTemporaryFile(suffix=".bin", delete=True) as tmp:
        tmp.write(bytes(source))
        tmp.flush()
        return _ffmpeg_decode(tmp.name, sample_rate)


def _ffmpeg_decode(path: str, sample_rate: int) -> np.ndarray:
    cmd = ["ffmpeg", "-v", "error", "-nostdin", "-i", path,
           "-f", "f32le", "-ac", "1", "-ar", str(sample_rate), "pipe:1"]
    proc = subprocess.run(cmd, capture_output=True)
    if proc.returncode != 0:
        raise ValueError("ffmpeg could not decode audio: " + proc.stderr.decode(errors="replace").strip()[:300])
    wav = np.frombuffer(proc.stdout, dtype=np.float32)
    if wav.size == 0:
        raise ValueError("decoded audio is empty")
    return wav


# --------------------------------------------------------------------------- VAD
class VAD:
    """Silero-architecture LSTM VAD (vad/vad.onnx). Returns speech segments as sample ranges."""

    def __init__(self, onnx_path: Path, threshold: float = 0.5, min_speech_ms: int = 250,
                 min_silence_ms: int = 100, pad_ms: int = 100):
        import onnxruntime as ort

        opts = ort.SessionOptions()
        opts.intra_op_num_threads = 1
        opts.inter_op_num_threads = 1
        self.sess = ort.InferenceSession(str(onnx_path), opts, providers=["CPUExecutionProvider"])
        self.on = threshold
        self.off = max(threshold - 0.15, 0.01)  # hysteresis, as in Silero's reference iterator
        self.min_speech = min_speech_ms * SAMPLE_RATE // 1000
        self.min_silence = min_silence_ms * SAMPLE_RATE // 1000
        self.pad = pad_ms * SAMPLE_RATE // 1000

    def speech_probs(self, wav: np.ndarray) -> np.ndarray:
        h = c = np.zeros((2, 1, 64), np.float32)
        sr = np.array(SAMPLE_RATE, dtype=np.int64)
        tail = (-len(wav)) % VAD_WINDOW
        padded = np.concatenate([wav, np.zeros(tail, np.float32)]) if tail else wav
        probs = np.empty(len(padded) // VAD_WINDOW, np.float32)
        for k in range(len(probs)):
            chunk = padded[k * VAD_WINDOW:(k + 1) * VAD_WINDOW][None, :]
            p, h, c = self.sess.run(None, {"input": chunk, "sr": sr, "h": h, "c": c})
            probs[k] = p[0, 0]
        return probs

    def segments(self, wav: np.ndarray) -> list[tuple[int, int]]:
        probs = self.speech_probs(wav)
        segs: list[tuple[int, int]] = []
        start = silence_since = None
        for k, p in enumerate(probs):
            t = k * VAD_WINDOW
            if start is None:
                if p >= self.on:
                    start = t
            elif p < self.off:
                if silence_since is None:
                    silence_since = t
                elif t + VAD_WINDOW - silence_since >= self.min_silence:
                    if silence_since - start >= self.min_speech:
                        segs.append((start, silence_since))
                    start = silence_since = None
            else:
                silence_since = None
        if start is not None:
            end = len(wav) if silence_since is None else silence_since
            if end - start >= self.min_speech:
                segs.append((start, end))
        # pad and merge overlaps
        merged: list[tuple[int, int]] = []
        for s, e in segs:
            s, e = max(0, s - self.pad), min(len(wav), e + self.pad)
            if merged and s <= merged[-1][1]:
                merged[-1] = (merged[-1][0], max(merged[-1][1], e))
            else:
                merged.append((s, e))
        return merged


def plan_chunks(segments: list[tuple[int, int]], max_chunk_s: float = 20.0,
                max_gap_s: float = 1.0) -> list[tuple[int, int]]:
    """Pack speech segments into chunks of at most max_chunk_s, splitting over-long ones."""
    max_len = int(max_chunk_s * SAMPLE_RATE)
    max_gap = int(max_gap_s * SAMPLE_RATE)
    chunks: list[tuple[int, int]] = []
    for s, e in segments:
        for a in range(s, e, max_len):
            b = min(a + max_len, e)
            if chunks and a - chunks[-1][1] <= max_gap and b - chunks[-1][0] <= max_len:
                chunks[-1] = (chunks[-1][0], b)
            else:
                chunks.append((a, b))
    return chunks


def fixed_chunks(n_samples: int, max_chunk_s: float = 20.0) -> list[tuple[int, int]]:
    step = int(max_chunk_s * SAMPLE_RATE)
    return [(a, min(a + step, n_samples)) for a in range(0, n_samples, step)]


def language_hint(text: str) -> str:
    """Crude script-based hint: 'kk' / 'ru' / 'mixed'. Real language handling belongs to the router."""
    words = text.split()
    if not words:
        return "unknown"
    kk_words = sum(1 for w in words if KAZAKH_LETTERS & set(w))
    if kk_words == 0:
        return "ru"
    return "kk" if kk_words / len(words) >= 0.25 else "mixed"


# --------------------------------------------------------------------------- engine
@dataclass
class Segment:
    start: float
    end: float
    text: str


@dataclass
class Transcription:
    text: str
    language_hint: str
    duration_s: float
    segments: list[Segment]
    timings_ms: dict = field(default_factory=dict)
    device: str = "cpu"
    model: str = "rukk"

    def to_dict(self) -> dict:
        return asdict(self)


class STTEngine:
    def __init__(self, models_dir: Path | str = MODELS_DIR, lang: str = "rukk", device: str = "auto",
                 num_threads: int | None = None, use_vad: bool = True, max_chunk_s: float = 20.0):
        import torch

        self.torch = torch
        models_dir = Path(models_dir)
        model_path = models_dir / "asr" / lang / "model.pt"
        tokens_path = models_dir / "asr" / lang / "tokens.lst"
        if not model_path.exists() or not tokens_path.exists():
            raise FileNotFoundError(
                f"{model_path} not found. Run: python stt/download_model.py --lang {lang}")

        self.lang = lang
        self.max_chunk_s = max_chunk_s
        self.device = self._resolve_device(device)
        if self.device == "cpu":
            torch.set_num_threads(num_threads or max(1, min(8, os.cpu_count() or 1)))

        self.tokens: dict[int, str] = {}
        for line in tokens_path.read_text(encoding="utf-8").splitlines():
            if line.strip():
                sym, idx = line.rstrip("\n").split("\t")
                self.tokens[int(idx)] = sym
        self.blank = max(self.tokens) + 1  # CTC blank is the last index

        self.model = torch.jit.load(str(model_path), map_location="cpu").eval().to(self.device)

        vad_path = models_dir / "vad" / "vad.onnx"
        self.vad = VAD(vad_path) if use_vad and vad_path.exists() else None
        self._lock = threading.Lock()
        self.warmup()

    def _resolve_device(self, device: str) -> str:
        if device == "auto":
            return "cuda" if self.torch.cuda.is_available() else "cpu"
        if device.startswith("cuda") and not self.torch.cuda.is_available():
            raise RuntimeError("CUDA requested but torch.cuda.is_available() is False")
        return device

    def warmup(self) -> None:
        self._logits(np.zeros(SAMPLE_RATE, np.float32))

    # --- core
    def _logits(self, wav: np.ndarray) -> np.ndarray:
        """Per-frame CTC logits [frames, vocab] for one chunk (20 ms per frame)."""
        if len(wav) < MIN_CHUNK_SAMPLES:
            wav = np.concatenate([wav, np.zeros(MIN_CHUNK_SAMPLES - len(wav), np.float32)])
        x = self.torch.from_numpy(np.ascontiguousarray(wav, dtype=np.float32)).unsqueeze(0).to(self.device)
        with self.torch.inference_mode():
            out = self.model(x)
        logits = out[0] if isinstance(out, (tuple, list)) else out
        if logits.dim() == 3:
            logits = logits[0]
        return logits.float().cpu().numpy()

    def _greedy_decode(self, logits: np.ndarray) -> str:
        ids = logits.argmax(-1)
        out, prev = [], None
        for i in ids.tolist():
            if i != prev and i != self.blank:
                out.append(self.tokens.get(i, ""))
            prev = i
        text = "".join(out).replace("|", " ").replace("_", " ")
        return re.sub(r"\s+", " ", text).strip()

    def transcribe(self, source, use_vad: bool | None = None) -> Transcription:
        """source: path, encoded bytes, or float32 numpy array already at 16 kHz mono."""
        t0 = time.perf_counter()
        wav = source if isinstance(source, np.ndarray) else load_audio(source)
        wav = np.ascontiguousarray(wav, dtype=np.float32)
        t1 = time.perf_counter()

        with self._lock:
            vad = self.vad if (self.vad is not None and use_vad is not False) else None
            chunks = plan_chunks(vad.segments(wav), self.max_chunk_s) if vad else []
            if not chunks:  # no VAD, or VAD found nothing: process everything in fixed windows
                chunks = fixed_chunks(len(wav), self.max_chunk_s)
            t2 = time.perf_counter()

            segments = []
            for s, e in chunks:
                text = self._greedy_decode(self._logits(wav[s:e]))
                if text:
                    segments.append(Segment(round(s / SAMPLE_RATE, 2), round(e / SAMPLE_RATE, 2), text))
            t3 = time.perf_counter()

        text = " ".join(seg.text for seg in segments)
        return Transcription(
            text=text,
            language_hint=language_hint(text),
            duration_s=round(len(wav) / SAMPLE_RATE, 2),
            segments=segments,
            timings_ms={"decode_audio": round((t1 - t0) * 1000), "vad": round((t2 - t1) * 1000),
                        "asr": round((t3 - t2) * 1000), "total": round((t3 - t0) * 1000)},
            device=self.device,
            model=self.lang,
        )
