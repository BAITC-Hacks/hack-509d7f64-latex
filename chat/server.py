"""Voice chat gateway: browser <-> (STT service, dialogue router, TTS service).

    python chat/server.py                     # http://localhost:9102, router at http://127.0.0.1:8080/v1/turns (backend/, Go)
    ROUTER_URL=http://host:8080/v1/turns ROUTER_TOKEN=... python chat/server.py

Flow per turn:  mic PCM (16 kHz) --ws--> partial STT every ~0.7 s --> final STT --> router (Go) --> TTS --> audio + trace
Text turns skip STT. Everything the browser needs is same-origin here; STT/TTS run as separate services.

WebSocket /ws?session_id=...
  client -> server : binary frames = PCM16 mono 16 kHz
                     {"type":"start"} {"type":"end"} {"type":"cancel"} {"type":"text","text":"..."} {"type":"ping"}
  server -> client : {"type":"ready"} {"type":"partial","text"} {"type":"final","text","lang","stt_ms"}
                     {"type":"reply","text","lang","source","router_ms","trace"} {"type":"audio","url","duration_s","tts_ms","segments"}
                     {"type":"turn_done","turn","latency_ms"} {"type":"notice","message"} {"type":"error","message"}
"""
from __future__ import annotations

import asyncio
import base64
import io
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid
from collections import OrderedDict
from contextlib import asynccontextmanager
from pathlib import Path

import numpy as np
import soundfile as sf
import uvicorn
from fastapi import FastAPI, HTTPException, WebSocket, WebSocketDisconnect
from fastapi.responses import FileResponse, Response
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel
from starlette.concurrency import run_in_threadpool

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from router_client import Router, detect_lang  # noqa: E402

STT_URL = os.environ.get("STT_URL", "http://127.0.0.1:9100").rstrip("/")
TTS_URL = os.environ.get("TTS_URL", "http://127.0.0.1:9101").rstrip("/")
DATASET_DIR = Path(os.environ.get("DATASET_DIR", HERE.parent / "voice_router_dataset"))
TTS_LANG = os.environ.get("TTS_LANG", "auto")
TTS_VOICE_RU = os.environ.get("TTS_VOICE_RU") or None
TTS_VOICE_KK = os.environ.get("TTS_VOICE_KK") or None
SAMPLE_RATE = 16000
PARTIAL_EVERY_S = 0.7
PARTIAL_WINDOW_S = 20.0
MAX_UTTERANCE_S = 60.0

router = Router(os.environ.get("ROUTER_URL", "http://127.0.0.1:8080/v1/turns"), os.environ.get("ROUTER_SESSION_URL", ""),
                os.environ.get("ROUTER_MODE", "auto"), DATASET_DIR, token=os.environ.get("ROUTER_TOKEN", ""))
SESSIONS: dict[str, dict] = {}
AUDIO: "OrderedDict[str, bytes]" = OrderedDict()  # id -> wav bytes, bounded


# --------------------------------------------------------------------------- service calls (blocking; run in threadpool)
def _multipart(fields: dict[str, str], file_field: str, filename: str, data: bytes, ctype: str) -> tuple[bytes, str]:
    boundary = "----speech" + uuid.uuid4().hex
    body = io.BytesIO()
    for k, v in fields.items():
        body.write(f"--{boundary}\r\nContent-Disposition: form-data; name=\"{k}\"\r\n\r\n{v}\r\n".encode())
    body.write(f"--{boundary}\r\nContent-Disposition: form-data; name=\"{file_field}\"; filename=\"{filename}\"\r\n"
               f"Content-Type: {ctype}\r\n\r\n".encode())
    body.write(data)
    body.write(f"\r\n--{boundary}--\r\n".encode())
    return body.getvalue(), f"multipart/form-data; boundary={boundary}"


def _http(url: str, data: bytes | None = None, ctype: str = "application/json", timeout: float = 120) -> dict:
    req = urllib.request.Request(url, data=data, method="POST" if data is not None else "GET",
                                 headers={"Content-Type": ctype} if data is not None else {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"{url} -> HTTP {e.code}: {e.read().decode(errors='replace')[:200]}")
    except urllib.error.URLError as e:
        raise RuntimeError(f"{url} unreachable ({e.reason})")


def pcm_to_wav(pcm: bytes) -> bytes:
    audio = np.frombuffer(pcm, dtype=np.int16)
    buf = io.BytesIO()
    sf.write(buf, audio, SAMPLE_RATE, format="WAV", subtype="PCM_16")
    return buf.getvalue()


def stt(pcm: bytes, vad: bool) -> dict:
    body, ctype = _multipart({}, "file", "utterance.wav", pcm_to_wav(pcm), "audio/wav")
    return _http(f"{STT_URL}/transcribe?vad={'true' if vad else 'false'}", body, ctype, timeout=60)


def tts(text: str, lang: str) -> dict:
    payload = {"text": text, "lang": lang if lang in ("ru", "kk") else TTS_LANG, "voice_ru": TTS_VOICE_RU,
               "voice_kk": TTS_VOICE_KK, "format": "wav"}
    return _http(f"{TTS_URL}/synthesize", json.dumps(payload).encode(), timeout=120)


def service_ok(url: str) -> bool:
    try:
        return _http(f"{url}/health", timeout=3).get("status") == "ok"
    except Exception:
        return False


def store_audio(wav: bytes) -> str:
    aid = uuid.uuid4().hex
    AUDIO[aid] = wav
    while len(AUDIO) > 60:
        AUDIO.popitem(last=False)
    return aid


# --------------------------------------------------------------------------- turn pipeline
async def run_turn(ws: WebSocket | None, session: dict, text: str, lang: str, stt_ms: int | None) -> dict:
    """Router -> TTS; streams intermediate events to ws when given. Returns the full turn record."""
    async def emit(msg: dict):
        if ws is not None:
            await ws.send_json(msg)

    session["turn"] += 1
    turn_no = session["turn"]
    t0 = time.perf_counter()
    await emit({"type": "thinking"})
    try:
        r = await run_in_threadpool(router.turn, session["id"], session.get("remote_session"), text, lang, turn_no)
    except Exception as e:
        await emit({"type": "error", "message": f"router: {e}"})
        return {"error": str(e)}
    reply_lang = r.get("lang") or detect_lang(r["reply"])
    await emit({"type": "reply", "text": r["reply"], "lang": reply_lang, "source": r["source"],
                "router_ms": r["ms"], "trace": r.get("trace", {})})

    audio_info = None
    tts_ms = None
    try:
        t1 = time.perf_counter()
        s = await run_in_threadpool(tts, r["reply"], reply_lang if TTS_LANG != "auto" else "auto")
        tts_ms = round((time.perf_counter() - t1) * 1000)
        aid = store_audio(base64.b64decode(s["audio_base64"]))
        audio_info = {"url": f"/audio/{aid}.wav", "duration_s": s["duration_s"], "tts_ms": tts_ms,
                      "segments": [{"lang": g["lang"], "engine": g["engine"], "voice": g["voice"], "ms": g["ms"]} for g in s["segments"]]}
        await emit({"type": "audio", **audio_info})
    except Exception as e:
        await emit({"type": "notice", "message": f"TTS unavailable: {e}"})

    latency = {"stt": stt_ms, "router": r["ms"], "tts": tts_ms, "total": round((time.perf_counter() - t0) * 1000) + (stt_ms or 0)}
    record = {"turn": turn_no, "user": text, "lang": lang, "reply": r["reply"], "reply_lang": reply_lang,
              "source": r["source"], "trace": r.get("trace", {}), "audio": audio_info, "latency_ms": latency}
    session["history"].append(record)
    await emit({"type": "turn_done", "turn": turn_no, "latency_ms": latency})
    return record


# --------------------------------------------------------------------------- app
@asynccontextmanager
async def lifespan(app: FastAPI):
    print(f"chat gateway: STT={STT_URL} TTS={TTS_URL} router={router.status()}")
    yield


app = FastAPI(title="Voice chat gateway", lifespan=lifespan)
app.mount("/static", StaticFiles(directory=HERE / "static"), name="static")


@app.get("/")
async def index():
    return FileResponse(HERE / "static" / "index.html", headers={"Cache-Control": "no-cache"})


@app.get("/health")
async def health():
    stt_ok, tts_ok, rstat = await asyncio.gather(run_in_threadpool(service_ok, STT_URL), run_in_threadpool(service_ok, TTS_URL),
                                                 run_in_threadpool(router.status))
    return {"status": "ok" if stt_ok and tts_ok else "degraded", "stt": {"url": STT_URL, "ok": stt_ok},
            "tts": {"url": TTS_URL, "ok": tts_ok}, "router": rstat, "sessions": len(SESSIONS)}


@app.post("/api/session")
async def new_session():
    sid = "chat_" + uuid.uuid4().hex[:12]
    remote = await run_in_threadpool(router.new_session)
    SESSIONS[sid] = {"id": sid, "remote_session": remote, "turn": 0, "history": [], "created": time.time()}
    if len(SESSIONS) > 500:
        oldest = min(SESSIONS, key=lambda k: SESSIONS[k]["created"])
        SESSIONS.pop(oldest, None)
    return {"session_id": sid, "remote_session": remote, "router": router.status()}


class TurnRequest(BaseModel):
    session_id: str
    text: str


@app.post("/api/turn")
async def rest_turn(req: TurnRequest):
    """Text turn without a WebSocket (curl / other clients). Returns reply, trace, and an audio URL."""
    session = SESSIONS.get(req.session_id)
    if session is None:
        raise HTTPException(404, "unknown session; POST /api/session first")
    if not req.text.strip():
        raise HTTPException(400, "empty text")
    record = await run_turn(None, session, req.text.strip(), detect_lang(req.text), None)
    if "error" in record:
        raise HTTPException(502, record["error"])
    return record


@app.get("/api/session/{sid}/history")
async def history(sid: str):
    session = SESSIONS.get(sid)
    if session is None:
        raise HTTPException(404, "unknown session")
    return {"session_id": sid, "turns": session["history"]}


@app.get("/audio/{aid}.wav")
async def audio(aid: str):
    data = AUDIO.get(aid)
    if data is None:
        raise HTTPException(404, "audio expired")
    return Response(content=data, media_type="audio/wav", headers={"Cache-Control": "no-store"})


@app.websocket("/ws")
async def ws_endpoint(ws: WebSocket):
    await ws.accept()
    sid = ws.query_params.get("session_id", "")
    session = SESSIONS.get(sid)
    if session is None:
        await ws.send_json({"type": "error", "message": "unknown session; POST /api/session first"})
        await ws.close()
        return
    await ws.send_json({"type": "ready", "session_id": sid, "router": router.status()})

    pcm = bytearray()
    recording = False
    last_partial_len = 0
    partial_task: asyncio.Task | None = None
    busy = asyncio.Lock()

    async def partial_loop():
        nonlocal last_partial_len
        try:
            while recording:
                await asyncio.sleep(PARTIAL_EVERY_S)
                if len(pcm) - last_partial_len < SAMPLE_RATE:  # < 0.5 s of new audio (2 bytes/sample)
                    continue
                window = bytes(pcm[-int(PARTIAL_WINDOW_S * SAMPLE_RATE * 2):])
                last_partial_len = len(pcm)
                try:
                    r = await run_in_threadpool(stt, window, False)
                    if recording:
                        await ws.send_json({"type": "partial", "text": r.get("text", "")})
                except Exception as e:
                    await ws.send_json({"type": "notice", "message": f"live transcription unavailable: {e}"})
                    return  # final recognition still runs on "end"
        except asyncio.CancelledError:
            pass

    async def finish_utterance():
        nonlocal recording, partial_task
        recording = False
        if partial_task:
            partial_task.cancel()
            partial_task = None
        data = bytes(pcm)
        pcm.clear()
        if len(data) < SAMPLE_RATE * 2 * 0.3:
            await ws.send_json({"type": "final", "text": "", "lang": None, "stt_ms": 0})
            await ws.send_json({"type": "notice", "message": "too short, nothing recognized"})
            return
        t0 = time.perf_counter()
        try:
            r = await run_in_threadpool(stt, data, True)
        except Exception as e:
            await ws.send_json({"type": "error", "message": f"STT: {e}"})
            return
        stt_ms = round((time.perf_counter() - t0) * 1000)
        text = r.get("text", "").strip()
        lang = r.get("language_hint") or detect_lang(text)
        await ws.send_json({"type": "final", "text": text, "lang": lang, "stt_ms": stt_ms, "audio_s": r.get("duration_s")})
        if not text:
            await ws.send_json({"type": "notice", "message": "no speech recognized"})
            return
        async with busy:
            await run_turn(ws, session, text, lang, stt_ms)

    try:
        while True:
            msg = await ws.receive()
            if msg.get("type") == "websocket.disconnect":
                break
            if msg.get("bytes") is not None:
                if recording and len(pcm) < MAX_UTTERANCE_S * SAMPLE_RATE * 2:
                    pcm.extend(msg["bytes"])
                continue
            try:
                ev = json.loads(msg.get("text") or "{}")
            except json.JSONDecodeError:
                continue
            kind = ev.get("type")
            if kind == "start":
                pcm.clear()
                last_partial_len = 0
                recording = True
                if partial_task:
                    partial_task.cancel()
                partial_task = asyncio.create_task(partial_loop())
                await ws.send_json({"type": "listening"})
            elif kind == "end":
                if recording:
                    await finish_utterance()
            elif kind == "cancel":
                recording = False
                pcm.clear()
                if partial_task:
                    partial_task.cancel()
                    partial_task = None
                await ws.send_json({"type": "cancelled"})
            elif kind == "text":
                text = (ev.get("text") or "").strip()
                if text:
                    async with busy:
                        await run_turn(ws, session, text, detect_lang(text), None)
            elif kind == "ping":
                await ws.send_json({"type": "pong", "t": ev.get("t")})
    except WebSocketDisconnect:
        pass
    finally:
        recording = False
        if partial_task:
            partial_task.cancel()


if __name__ == "__main__":
    uvicorn.run(app, host=os.environ.get("CHAT_HOST", "0.0.0.0"), port=int(os.environ.get("CHAT_PORT", "9102")))
