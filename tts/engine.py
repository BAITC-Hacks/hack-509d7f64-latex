"""Text-to-speech for the voice router.

Two engines, routed by language:
  ru  -> Silero TTS v5 (`v5_cis_base`, 29 Russian voices)      https://huggingface.co/Dimitrius174/silero-v5-tts-ru
  kk  -> ISSAI KazakhTTS: ESPnet Tacotron2 + ParallelWaveGAN   https://github.com/IS2AI/Kazakh_TTS

`TTSRouter.synthesize(text, lang="auto")` splits the text into sentences, picks the engine per sentence
(Kazakh-specific letters -> kk, otherwise ru), resamples everything to one rate and concatenates.
"""
from __future__ import annotations

import io
import logging
import os
import re
import subprocess
import threading
import time
from dataclasses import asdict, dataclass
from math import gcd
from pathlib import Path

import numpy as np

logging.getLogger().addFilter(lambda rec: "make_pad_mask" not in rec.getMessage())  # espnet tracing chatter

OUT_SR = int(os.environ.get("TTS_SAMPLE_RATE", "24000"))
MODELS_DIR = Path(os.environ.get("TTS_MODELS_DIR", Path(__file__).resolve().parent / "models"))
KAZAKH_LETTERS = frozenset("әғқңөұүһі")
GAP_S = 0.15  # silence between sentences
PEAK = 0.85


# --------------------------------------------------------------------------- helpers
def detect_lang(text: str) -> str:
    return "kk" if KAZAKH_LETTERS & set(text.lower()) else "ru"


def split_sentences(text: str) -> list[str]:
    parts = re.split(r"(?<=[.!?…])\s+|\n+", text.strip())
    return [p.strip() for p in parts if p and p.strip()]


def resample(wav: np.ndarray, sr_from: int, sr_to: int) -> np.ndarray:
    if sr_from == sr_to:
        return wav
    try:
        from scipy.signal import resample_poly
        g = gcd(sr_from, sr_to)
        return resample_poly(wav, sr_to // g, sr_from // g).astype(np.float32)
    except ImportError:
        n = int(round(len(wav) * sr_to / sr_from))
        return np.interp(np.linspace(0, len(wav) - 1, n), np.arange(len(wav)), wav).astype(np.float32)


def _peak_normalize(wav: np.ndarray, peak: float = PEAK) -> np.ndarray:
    m = float(np.max(np.abs(wav))) if wav.size else 0.0
    return wav * (peak / m) if m > 1e-4 else wav


def _pick_device(device: str, torch) -> str:
    if device == "auto":
        return "cuda" if torch.cuda.is_available() else "cpu"
    return device


# --------------------------------------------------------------------------- Russian: Silero v5
class RussianTTS:
    name = "silero_v5_cis_base"
    lang = "ru"

    def __init__(self, device: str = "cpu", sample_rate: int = 24000, use_stress: bool = True):
        import silero
        import torch

        self.torch = torch
        self.device = _pick_device(device, torch)
        self.sample_rate = sample_rate if sample_rate in (8000, 24000, 48000) else 48000
        try:
            model, _ = silero.silero_tts(language="ru", speaker="v5_cis_base", sample_rate=self.sample_rate)
        except TypeError:  # older package signature without sample_rate
            model, _ = silero.silero_tts(language="ru", speaker="v5_cis_base")
        model.to(self.device)
        self.model = model
        self.voices = sorted(s for s in getattr(model, "speakers", []) if str(s).startswith("ru_")) or ["ru_eduard"]
        self.default_voice = "ru_eduard" if "ru_eduard" in self.voices else self.voices[0]
        self.accentor = None
        if use_stress:
            try:
                from silero_stress import accentor
                self.accentor = accentor.load_accentor("ru")
            except Exception as e:  # optional component
                print(f"[tts] silero_stress accentor unavailable: {e}")

    def synthesize(self, text: str, voice: str | None = None) -> tuple[np.ndarray, int]:
        voice = voice or self.default_voice
        if voice not in self.voices:
            raise ValueError(f"unknown Russian voice {voice!r}; choose one of {self.voices}")
        if self.accentor is not None:
            try:
                text = self.accentor(text)
            except Exception:
                pass
        with self.torch.no_grad():
            audio = self.model.apply_tts(text=text, speaker=voice, sample_rate=self.sample_rate)
        if hasattr(audio, "cpu"):
            audio = audio.cpu().numpy()
        return np.asarray(audio, dtype=np.float32).ravel(), self.sample_rate


# --------------------------------------------------------------------------- Kazakh: ESPnet Tacotron2 + PWG
class KazakhTTS:
    name = "issai_kazakhtts_tacotron2_pwg"
    lang = "kk"
    sample_rate = 22050

    def __init__(self, models_dir: Path | str = MODELS_DIR / "kazakh", voices: list[str] | None = None,
                 device: str = "auto"):
        import torch

        self.torch = torch
        self.device = _pick_device(device, torch)
        models_dir = Path(models_dir)
        available = sorted(p.name for p in models_dir.iterdir()
                           if (p / "tts" / "exp" / "tts_train_raw_char" / "config.yaml").exists()) if models_dir.exists() else []
        wanted = [v for v in (voices or available) if v in available]
        if not wanted:
            raise FileNotFoundError(f"no Kazakh voices under {models_dir}; run: python tts/download_models.py")
        self.engines: dict[str, tuple] = {}
        self.token_set: set[str] = set()
        for v in wanted:
            self.engines[v] = self._load_voice(models_dir / v)
        self.voices = list(self.engines)
        self.default_voice = self.voices[0]

    def _load_voice(self, voice_dir: Path) -> tuple:
        import yaml
        from espnet2.bin.tts_inference import Text2Speech

        import scipy.signal
        if not hasattr(scipy.signal, "kaiser"):  # parallel_wavegan 0.6 imports it from the old location
            from scipy.signal.windows import kaiser
            scipy.signal.kaiser = kaiser
        from parallel_wavegan.utils import load_model

        tts_dir = voice_dir / "tts"
        cfg_path = tts_dir / "exp" / "tts_train_raw_char" / "config.yaml"
        model_file = tts_dir / "exp" / "tts_train_raw_char" / "train.loss.ave_5best.pth"
        conf = yaml.safe_load(cfg_path.read_text(encoding="utf-8"))
        nc = dict(conf.get("normalize_conf") or {})
        if nc.get("stats_file") and not os.path.isabs(nc["stats_file"]):
            nc["stats_file"] = str(tts_dir / nc["stats_file"])  # packed configs use paths relative to the recipe dir
            conf["normalize_conf"] = nc
        local_cfg = tts_dir / "config.local.yaml"
        local_cfg.write_text(yaml.safe_dump(conf, allow_unicode=True), encoding="utf-8")
        self.token_set |= {t for t in conf.get("token_list", []) if len(t) == 1}

        text2speech = Text2Speech(
            str(local_cfg), str(model_file), device=self.device,
            # Tacotron 2 decoding settings from the ISSAI recipe (tts1/synthesize.py)
            threshold=0.5, minlenratio=0.0, maxlenratio=10.0,
            use_att_constraint=True, backward_window=1, forward_window=3,
        )
        text2speech.spc2wav = None  # no Griffin-Lim; PWG vocoder below
        vocoder = load_model(str(voice_dir / "vocoder" / "checkpoint-400000steps.pkl")).to(self.device).eval()
        vocoder.remove_weight_norm()
        return text2speech, vocoder

    def clean(self, text: str) -> str:
        """Lowercase and keep only characters the char-level model was trained on."""
        text = text.lower().replace("ё", "е")
        text = "".join(ch if ch in self.token_set or ch == " " else " " for ch in text)
        text = re.sub(r"\s+", " ", text).strip()
        if text and text[-1] not in ".!?":
            text += "."  # helps Tacotron2 predict the stop token
        return text

    def synthesize(self, text: str, voice: str | None = None) -> tuple[np.ndarray, int]:
        voice = voice or self.default_voice
        if voice not in self.engines:
            raise ValueError(f"unknown Kazakh voice {voice!r}; choose one of {self.voices}")
        text2speech, vocoder = self.engines[voice]
        cleaned = self.clean(text)
        if not cleaned.strip(".!? "):
            return np.zeros(0, np.float32), self.sample_rate
        with self.torch.no_grad():
            out = text2speech(cleaned)
            wav = vocoder.inference(out["feat_gen"])
        return wav.view(-1).cpu().numpy().astype(np.float32), self.sample_rate


# --------------------------------------------------------------------------- router
@dataclass
class Synthesis:
    audio: np.ndarray
    sample_rate: int
    duration_s: float
    segments: list[dict]
    timings_ms: dict

    def meta(self) -> dict:
        d = asdict(self)
        d.pop("audio")
        return d


class TTSRouter:
    def __init__(self, ru: RussianTTS | None, kk: KazakhTTS | None, out_sr: int = OUT_SR):
        if ru is None and kk is None:
            raise RuntimeError("no TTS engine loaded")
        self.ru, self.kk, self.out_sr = ru, kk, out_sr
        self._lock = threading.Lock()

    def engine_for(self, lang: str):
        eng = {"ru": self.ru, "kk": self.kk}.get(lang)
        return eng if eng is not None else (self.ru or self.kk)  # fall back to whatever is loaded

    def synthesize(self, text: str, lang: str = "auto", voice_ru: str | None = None,
                   voice_kk: str | None = None) -> Synthesis:
        sentences = split_sentences(text)
        if not sentences:
            raise ValueError("empty text")
        if lang not in ("auto", "ru", "kk"):
            raise ValueError("lang must be auto, ru or kk")
        t0 = time.perf_counter()
        pieces, segments = [], []
        with self._lock:
            for sentence in sentences:
                want = detect_lang(sentence) if lang == "auto" else lang
                eng = self.engine_for(want)
                voice = voice_ru if eng.lang == "ru" else voice_kk
                ts = time.perf_counter()
                wav, sr = eng.synthesize(sentence, voice)
                wav = _peak_normalize(resample(wav, sr, self.out_sr))
                ms = round((time.perf_counter() - ts) * 1000)
                pieces.append(wav)
                pieces.append(np.zeros(int(GAP_S * self.out_sr), np.float32))
                segments.append({"text": sentence, "lang": eng.lang, "requested": want, "engine": eng.name,
                                 "voice": voice or eng.default_voice, "ms": ms,
                                 "duration_s": round(len(wav) / self.out_sr, 2)})
        audio = np.concatenate(pieces[:-1]) if pieces else np.zeros(0, np.float32)
        total = round((time.perf_counter() - t0) * 1000)
        duration = round(len(audio) / self.out_sr, 2)
        return Synthesis(audio=audio, sample_rate=self.out_sr, duration_s=duration, segments=segments,
                         timings_ms={"total": total, "rtf": round(total / 1000 / duration, 3) if duration else None})


def encode_audio(audio: np.ndarray, sr: int, fmt: str = "wav") -> tuple[bytes, str]:
    """Encode float32 mono audio as wav (soundfile) or mp3/opus/ogg (ffmpeg)."""
    fmt = (fmt or "wav").lower()
    if fmt == "wav":
        import soundfile as sf
        buf = io.BytesIO()
        sf.write(buf, audio, sr, format="WAV", subtype="PCM_16")
        return buf.getvalue(), "audio/wav"
    codec = {"mp3": ["-f", "mp3", "-codec:a", "libmp3lame", "-q:a", "4"],
             "opus": ["-f", "ogg", "-codec:a", "libopus", "-b:a", "48k"],
             "ogg": ["-f", "ogg", "-codec:a", "libopus", "-b:a", "48k"]}.get(fmt)
    if codec is None:
        raise ValueError(f"unsupported format {fmt!r}; use wav, mp3, opus or ogg")
    cmd = ["ffmpeg", "-v", "error", "-nostdin", "-f", "f32le", "-ar", str(sr), "-ac", "1", "-i", "pipe:0", *codec, "pipe:1"]
    proc = subprocess.run(cmd, input=audio.astype(np.float32).tobytes(), capture_output=True)
    if proc.returncode != 0:
        raise RuntimeError("ffmpeg encode failed: " + proc.stderr.decode(errors="replace")[:200])
    return proc.stdout, {"mp3": "audio/mpeg", "opus": "audio/ogg", "ogg": "audio/ogg"}[fmt]
