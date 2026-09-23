"""HTTP server for the Kazakh/Russian STT model, with a browser test page.

    python stt/server.py                       # http://localhost:9100
    STT_PORT=9101 STT_DEVICE=cpu python stt/server.py
    STT_UI=0 python stt/server.py              # API only, no test page (backend mode for the Go server)

Endpoints:
    GET  /            browser test page (mic recording, file upload, bundled samples)
    GET  /health      model / device status
    GET  /samples     bundled sample clips with reference transcripts
    GET  /samples/<name>  the sample audio itself
    GET  /utterances  dev_utterances.json from the case dataset (+ scenario names) for read-aloud testing
    POST /v1/audio/transcriptions  OpenAI-compatible alias (fields 'file', 'model') -> {"text": ...};
                      lets clients that speak the OpenAI STT API (e.g. STT_BASE_URL=http://127.0.0.1:9100/v1) use this server
    ANY  /tts/<path>  proxied to the TTS service (TTS_URL, default http://127.0.0.1:9101) so the test page stays same-origin
    POST /transcribe  multipart 'file' (any audio format) -> {"text", "language_hint", "segments", "timings_ms", ...}
                      form field 'reference' (optional text) adds "wer", "cer" and a word "alignment"
                      ?sample=<name>  transcribe a bundled sample instead (reference from its manifest)
                      ?vad=false      skip VAD chunking
"""
import json
import os
import sys
import urllib.error
import urllib.request
from contextlib import asynccontextmanager
from pathlib import Path

import uvicorn
from fastapi import FastAPI, File, Form, HTTPException, Query, Request, Response, UploadFile
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse
from fastapi.staticfiles import StaticFiles
from starlette.concurrency import run_in_threadpool

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from engine import STTEngine  # noqa: E402
from metrics import align, cer, wer  # noqa: E402

SAMPLES_DIR = HERE / "samples"
DATASET_DIR = Path(os.environ.get(
    "STT_DATASET_DIR", HERE.parent / "voice_router_dataset"))
TTS_URL = os.environ.get("TTS_URL", "http://127.0.0.1:9101").rstrip("/")  # tts/server.py, proxied under /tts/
STATE: dict = {}


def _load_manifest() -> list[dict]:
    path = SAMPLES_DIR / "manifest.json"
    if not path.exists():
        return []
    return [m for m in json.loads(path.read_text(encoding="utf-8")) if (SAMPLES_DIR / m["file"]).exists()]


@asynccontextmanager
async def lifespan(app: FastAPI):
    STATE["engine"] = STTEngine(lang=os.environ.get("STT_LANG", "rukk"),
                                device=os.environ.get("STT_DEVICE", "auto"))
    print(f"STT ready: model={STATE['engine'].lang} device={STATE['engine'].device}")
    yield


app = FastAPI(title="Kazakh/Russian STT", lifespan=lifespan)
app.add_middleware(CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"])
SERVE_UI = os.environ.get("STT_UI", "1") != "0"
if SERVE_UI:
    app.mount("/static", StaticFiles(directory=HERE / "static"), name="static")


@app.get("/")
async def index():
    if not SERVE_UI:
        return {"service": "stt", "ui": False, "endpoints": ["/health", "/transcribe", "/v1/audio/transcriptions"]}
    return FileResponse(HERE / "static" / "index.html", headers={"Cache-Control": "no-cache"})


@app.get("/health")
async def health():
    engine = STATE.get("engine")
    if engine is None:
        return {"status": "loading"}
    return {"status": "ok", "model": engine.lang, "device": engine.device, "vad": engine.vad is not None,
            "repo": "alibiserikbay/kazakh-russian-mixed-stt", "decoding": "greedy-ctc", "ui": SERVE_UI}


@app.get("/samples")
async def samples():
    return _load_manifest()


@app.get("/utterances")
async def utterances():
    """Labeled phrases from the case dataset, for reading aloud and scoring the transcript."""
    path = DATASET_DIR / "dev_utterances.json"
    if not path.exists():
        return {"utterances": [], "scenarios": {}, "source": str(path)}
    data = json.loads(path.read_text(encoding="utf-8"))
    names: dict[str, str] = {}
    sc_path = DATASET_DIR / "scenarios.json"
    if sc_path.exists():
        sc = json.loads(sc_path.read_text(encoding="utf-8"))
        for item in sc.get("scenarios", []) + sc.get("system_intents", []):
            key = next((v for k, v in item.items() if k.endswith("id") and isinstance(v, str)), None)
            if key:
                names[key] = item.get("name") or key
    responses: list[dict] = []
    if sc_path.exists():
        for item in sc.get("scenarios", []):
            for lang, lines in (item.get("responses") or {}).items():
                for kind, text in (lines or {}).items():
                    responses.append({"scenario_id": item["scenario_id"], "name": item.get("name", ""),
                                      "lang": lang, "kind": kind, "text": text})
        for item in sc.get("system_intents", []):
            for lang, text in (item.get("response") or {}).items():
                responses.append({"scenario_id": item.get("id", ""), "name": item.get("id", ""),
                                  "lang": lang, "kind": "response", "text": text})
    return {"utterances": data.get("utterances", []), "scenarios": names, "responses": responses, "source": str(path)}


@app.api_route("/tts/{path:path}", methods=["GET", "POST"])
async def tts_proxy(path: str, request: Request):
    """Forward to the TTS service so the browser page can use it without CORS or a second origin."""
    body = await request.body()
    url = f"{TTS_URL}/{path}" + (f"?{request.url.query}" if request.url.query else "")

    def call():
        req = urllib.request.Request(url, data=body if request.method == "POST" else None, method=request.method,
                                     headers={"Content-Type": request.headers.get("content-type", "application/json")})
        try:
            with urllib.request.urlopen(req, timeout=180) as r:
                return r.status, r.headers.get("content-type", "application/octet-stream"), r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.headers.get("content-type", "application/json"), e.read()
        except urllib.error.URLError as e:
            return 503, "application/json", json.dumps(
                {"detail": f"TTS service unreachable at {TTS_URL} ({e.reason}); start it with: .venv-tts/bin/python tts/server.py"}).encode()

    status, ctype, data = await run_in_threadpool(call)
    return Response(content=data, status_code=status, media_type=ctype)


@app.get("/samples/{name}")
async def sample_audio(name: str):
    path = SAMPLES_DIR / Path(name).name
    if not path.exists():
        raise HTTPException(404, f"unknown sample {name!r}")
    return FileResponse(path, media_type="audio/wav")


@app.post("/transcribe")
async def transcribe(file: UploadFile | None = File(None), reference: str | None = Form(None),
                     sample: str | None = Query(None), vad: bool = Query(True)):
    engine = STATE.get("engine")
    if engine is None:
        raise HTTPException(503, "model still loading")

    if sample:
        path = SAMPLES_DIR / Path(sample).name
        if not path.exists():
            raise HTTPException(404, f"unknown sample {sample!r}")
        source = path
        entry = next((m for m in _load_manifest() if m["file"] == path.name), None)
        reference = entry["transcription"] if entry else reference
    elif file is not None:
        source = await file.read()
        if not source:
            raise HTTPException(400, "empty upload")
    else:
        raise HTTPException(400, "send a multipart 'file' or ?sample=<name>")

    try:
        result = await run_in_threadpool(engine.transcribe, source, vad)
    except ValueError as e:
        raise HTTPException(400, str(e))

    out = result.to_dict()
    if reference and reference.strip():
        out["reference"] = reference
        out["wer"] = round(wer(reference, result.text), 4)
        out["cer"] = round(cer(reference, result.text), 4)
        out["alignment"] = align(reference, result.text)
    return out


@app.post("/v1/audio/transcriptions")
async def openai_transcriptions(file: UploadFile = File(...), model: str | None = Form(None),
                                language: str | None = Form(None)):
    """OpenAI /audio/transcriptions-compatible alias. `model` and `language` are accepted and ignored."""
    engine = STATE.get("engine")
    if engine is None:
        raise HTTPException(503, "model still loading")
    data = await file.read()
    if not data:
        raise HTTPException(400, "empty upload")
    try:
        result = await run_in_threadpool(engine.transcribe, data, True)
    except ValueError as e:
        raise HTTPException(400, str(e))
    return {"text": result.text, "language": result.language_hint, "duration": result.duration_s,
            "timings_ms": result.timings_ms, "model": engine.lang}


if __name__ == "__main__":
    uvicorn.run(app, host=os.environ.get("STT_HOST", "0.0.0.0"), port=int(os.environ.get("STT_PORT", "9100")))
