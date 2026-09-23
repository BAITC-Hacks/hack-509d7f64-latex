"""HTTP service for text-to-speech (Russian: Silero v5, Kazakh: ISSAI KazakhTTS).

    .venv-tts/bin/python tts/server.py                 # http://127.0.0.1:9101
    TTS_DEVICE=cpu TTS_KK_VOICES=female1 .venv-tts/bin/python tts/server.py

Endpoints:
    GET  /health            engines loaded, voices, device, errors
    GET  /voices            {"ru": [...], "kk": [...], "default": {...}}
    POST /synthesize        JSON {"text", "lang": "auto|ru|kk", "voice_ru", "voice_kk", "format": "wav|mp3|opus"}
                            -> JSON {"audio_base64", "content_type", "sample_rate", "duration_s", "segments", "timings_ms"}
    POST /v1/audio/speech   OpenAI-compatible: JSON {"model": "auto|ru|kk", "input", "voice", "response_format"} -> audio bytes
"""
import base64
import os
import sys
import traceback
from contextlib import asynccontextmanager
from pathlib import Path

import uvicorn
from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import Response
from pydantic import BaseModel
from starlette.concurrency import run_in_threadpool

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from engine import OUT_SR, KazakhTTS, RussianTTS, TTSRouter, encode_audio  # noqa: E402

STATE: dict = {"ru": None, "kk": None, "router": None, "errors": {}}


def _load_engines() -> None:
    device = os.environ.get("TTS_DEVICE", "auto")
    if os.environ.get("TTS_RU", "1") != "0":
        try:
            STATE["ru"] = RussianTTS(device=os.environ.get("TTS_RU_DEVICE", "cpu"), sample_rate=OUT_SR)
            print(f"TTS ru ready: {STATE['ru'].name} voices={len(STATE['ru'].voices)} device={STATE['ru'].device}")
        except Exception as e:
            STATE["errors"]["ru"] = f"{type(e).__name__}: {e}"
            traceback.print_exc()
    if os.environ.get("TTS_KK", "1") != "0":
        try:
            voices = [v for v in os.environ.get("TTS_KK_VOICES", "").split(",") if v] or None
            STATE["kk"] = KazakhTTS(voices=voices, device=device)
            print(f"TTS kk ready: {STATE['kk'].name} voices={STATE['kk'].voices} device={STATE['kk'].device}")
        except Exception as e:
            STATE["errors"]["kk"] = f"{type(e).__name__}: {e}"
            traceback.print_exc()
    if STATE["ru"] or STATE["kk"]:
        STATE["router"] = TTSRouter(STATE["ru"], STATE["kk"])


@asynccontextmanager
async def lifespan(app: FastAPI):
    await run_in_threadpool(_load_engines)
    yield


app = FastAPI(title="Kazakh/Russian TTS", lifespan=lifespan)
app.add_middleware(CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"])


def _engine_info(key: str) -> dict:
    eng = STATE.get(key)
    if eng is None:
        return {"loaded": False, "error": STATE["errors"].get(key)}
    return {"loaded": True, "name": eng.name, "voices": eng.voices, "default_voice": eng.default_voice,
            "device": eng.device, "native_sample_rate": eng.sample_rate}


@app.get("/health")
async def health():
    return {"status": "ok" if STATE["router"] else ("loading" if not STATE["errors"] else "error"),
            "engines": {"ru": _engine_info("ru"), "kk": _engine_info("kk")}, "sample_rate": OUT_SR}


@app.get("/voices")
async def voices():
    ru, kk = STATE.get("ru"), STATE.get("kk")
    return {"ru": ru.voices if ru else [], "kk": kk.voices if kk else [],
            "default": {"ru": ru.default_voice if ru else None, "kk": kk.default_voice if kk else None}}


class SynthRequest(BaseModel):
    text: str
    lang: str = "auto"
    voice_ru: str | None = None
    voice_kk: str | None = None
    format: str = "wav"


def _synthesize(req: SynthRequest):
    router = STATE.get("router")
    if router is None:
        raise HTTPException(503, "no TTS engine loaded: " + str(STATE["errors"] or "still loading"))
    if not req.text.strip():
        raise HTTPException(400, "text is empty")
    if len(req.text) > 2000:
        raise HTTPException(400, "text too long (max 2000 characters)")
    try:
        result = router.synthesize(req.text, lang=req.lang, voice_ru=req.voice_ru, voice_kk=req.voice_kk)
        data, ctype = encode_audio(result.audio, result.sample_rate, req.format)
    except ValueError as e:
        raise HTTPException(400, str(e))
    return result, data, ctype


@app.post("/synthesize")
async def synthesize(req: SynthRequest):
    result, data, ctype = await run_in_threadpool(_synthesize, req)
    return {"audio_base64": base64.b64encode(data).decode(), "content_type": ctype, **result.meta()}


class SpeechRequest(BaseModel):
    input: str
    model: str = "auto"
    voice: str | None = None
    response_format: str = "mp3"


@app.post("/v1/audio/speech")
async def openai_speech(req: SpeechRequest):
    """OpenAI /audio/speech-compatible alias. `model` selects the language (auto|ru|kk); `voice` may be a
    Silero voice (ru_*) or a Kazakh voice (female1, male1)."""
    lang = req.model if req.model in ("auto", "ru", "kk") else "auto"
    voice_ru = req.voice if req.voice and req.voice.startswith("ru_") else None
    voice_kk = req.voice if req.voice and not req.voice.startswith("ru_") else None
    sreq = SynthRequest(text=req.input, lang=lang, voice_ru=voice_ru, voice_kk=voice_kk, format=req.response_format)
    result, data, ctype = await run_in_threadpool(_synthesize, sreq)
    return Response(content=data, media_type=ctype,
                    headers={"X-TTS-Duration": str(result.duration_s), "X-TTS-Ms": str(result.timings_ms["total"])})


if __name__ == "__main__":
    uvicorn.run(app, host=os.environ.get("TTS_HOST", "127.0.0.1"), port=int(os.environ.get("TTS_PORT", "9101")))
