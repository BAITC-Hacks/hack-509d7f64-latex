# STT — Kazakh / Russian / code-switched speech-to-text

Local speech recognition for the voice router, using
[alibiserikbay/kazakh-russian-mixed-stt](https://huggingface.co/alibiserikbay/kazakh-russian-mixed-stt)
(Apache-2.0): a wav2vec2 + CTC acoustic model that transcribes Kazakh, Russian, and
Kazakh–Russian code-switching in one pass, plus a Silero-style VAD. Runs on CPU or CUDA.

Output is **lowercase, no punctuation, numbers as spoken words** ("сегіз жүз", not "800").

## Setup (once)

```bash
python3 -m venv .venv                       # from the repo root; Python 3.12/3.13 preferred (3.14 works, torch.jit prints a deprecation warning)
.venv/bin/pip install -r stt/requirements.txt
.venv/bin/python stt/download_model.py       # mixed kk+ru model (756 MB) + VAD -> stt/models/
.venv/bin/python stt/fetch_samples.py        # optional: 10 FLEURS test clips with references -> stt/samples/
```

`ffmpeg` must be on PATH (used to decode any input format and resample to 16 kHz).

## Run the server

```bash
.venv/bin/python stt/server.py               # http://localhost:9100
STT_DEVICE=cpu STT_PORT=9101 .venv/bin/python stt/server.py
```

## Test console

Open <http://localhost:9100> (`stt/static/`: plain HTML/JS, no build step).

| Tab | What it does |
|---|---|
| **Microphone** | Click the mic or hold `Space` (push-to-talk). Level meter, live transcript every 1.5 s while you speak, auto-stop after 1.5 s of silence (toggle in the header). Shows "end of speech → text" latency, the number the jury's latency bonus is about. |
| **Read-aloud test** | Shows a phrase from the case's `dev_utterances.json` (filter by `ru`/`kk`/`mixed` and type, Prev/Next/Random) with its expected scenario. Record yourself reading it and get WER/CER plus a word-level diff: substituted, missing, extra. |
| **Upload** | Drag-and-drop or browse, several files at once, optional reference text to score against. |
| **Samples** | 10 FLEURS clips with reference text; "Transcribe all" prints average WER/CER. |
| **Synthesis** | Text-to-speech test: pick a reply line from `scenarios.json` or type text, choose `auto`/`ru`/`kk` and voices, play the result and see which engine spoke each sentence. Needs the TTS service (`tts/README.md`). |
| **History** | Every transcription and synthesis this session with playback, download, and JSON export. |

Every transcript card also has a **read back with TTS** button: transcript → TTS → audio, the full input/output loop.

Header toggles: VAD chunking on/off, live transcript, auto-stop. Choices persist in the browser.
Mic recording works on `localhost`; from another machine the page must be served over HTTPS
(for example `ssh -L 9100:localhost:9100 <this machine>` and open `localhost:9100` there).

### API

| Method | Path | What |
|---|---|---|
| `GET` | `/health` | `{"status":"ok","model":"rukk","device":"cuda",...}` |
| `POST` | `/transcribe` | multipart field `file` (wav/mp3/ogg/webm/m4a/flac, any rate); optional field `reference` adds `wer`, `cer`, `alignment` |
| `POST` | `/transcribe?sample=<name>` | transcribe a bundled sample, scored against its manifest reference |
| `GET` | `/samples` | bundled samples with reference transcripts (`/samples/<file>` is the audio) |
| `GET` | `/utterances` | `dev_utterances.json` + scenario names (`STT_DATASET_DIR` to relocate) |
| `POST` | `/v1/audio/transcriptions` | OpenAI-compatible alias (fields `file`, `model`) returning `{"text": ...}` |
| any | `/tts/<path>` | proxied to the TTS service (`TTS_URL`, default `http://127.0.0.1:9101`), see `tts/README.md` |

To point an OpenAI-style client at this service:
`STT_BASE_URL=http://127.0.0.1:9100/v1` and `STT_MODEL=rukk`.

```bash
curl -s -F "file=@call.wav" http://localhost:9100/transcribe | jq .
curl -s -F "file=@call.wav" -F "reference=Сәлеметсіз бе, ОГПО оформить етейін деп едім" http://localhost:9100/transcribe | jq '.wer, .alignment'
```

Response:

```json
{
  "text": "сәлеметсіз бе огпо оформить етейін деп едім",
  "language_hint": "mixed",
  "duration_s": 3.1,
  "segments": [{"start": 0.1, "end": 3.0, "text": "..."}],
  "timings_ms": {"decode_audio": 25, "vad": 12, "asr": 60, "total": 97},
  "device": "cuda",
  "model": "rukk"
}
```

`language_hint` is a crude script-based guess (`kk` / `ru` / `mixed`) from Kazakh-only
letters; language handling belongs to the router's triage layer. `alignment` is a list of
`{"op": "eq"|"sub"|"del"|"ins", "ref": ..., "hyp": ...}` over normalized words.

Environment: `STT_LANG` (`rukk` default, `kk`, `ru` — download them first), `STT_DEVICE`
(`auto` default, `cpu`, `cuda`), `STT_HOST`, `STT_PORT`, `STT_MODELS_DIR`, `STT_DATASET_DIR`, `TTS_URL`, `STT_UI=0` (API only, no test page).

## CLI

```bash
.venv/bin/python stt/transcribe.py audio.mp3 other.wav        # text per file
.venv/bin/python stt/transcribe.py audio.mp3 --json           # segments + timings
.venv/bin/python stt/eval_samples.py --device cuda            # WER/CER on stt/samples
```

## Python

```python
import sys; sys.path.insert(0, "stt")
from engine import STTEngine
engine = STTEngine()                              # CUDA if available
result = engine.transcribe("call.mp3")            # path, encoded bytes, or 16 kHz float32 array
print(result.text, result.language_hint, result.timings_ms)
```

## Measured here (RTX 4060 laptop, 10 FLEURS test clips, greedy decoding)

| lang | WER | CER | GPU (RTX 4060) | CPU (8 threads, Ryzen 5 7640HS) |
|---|---|---|---|---|
| kk | 18.0% | 8.5% | ~90× real time | ~13× real time |
| ru | 5.6% | 0.9% | ~100× real time | ~14× real time |

WER on Kazakh is inflated by digits in the references (the model says "он мың", reference
says "10 000"); CER is the better indicator. Model card numbers: kk 16.9% / ru 12.3% greedy.

## Notes

- **Pipeline:** ffmpeg decode → 16 kHz mono → VAD splits speech into ≤ 20 s chunks → model
  per chunk → greedy CTC. `?vad=false` / `--no-vad` uses fixed 20 s windows instead.
- **Better accuracy:** the model card's KenLM beam search cuts ~4 WER points but needs the
  3 GB `lm/rukk` files, `flashlight-text` + `kenlm`, ~3 min lexicon build per process, and
  lots of RAM. Not wired up; `download_model.py --with-lm` fetches the files if you want to try.
- **Domain:** trained on telephony speech; expect studio/wideband audio to be slightly worse.
- **No punctuation / casing / digits.** The bundled punctuation model is Russian-only and
  not used here.
- Streaming is not implemented; for the latency bonus, send audio as soon as the user stops
  speaking (VAD on the client) — a 5 s utterance transcribes in ~50 ms on GPU, ~0.5 s on CPU.

## In the voice stack

The Go router in `backend/` does not call this service itself; the chat gateway (`chat/`) does, between the
browser and the router. `scripts/speech_services.sh start` launches it together with the other services
(API only; add `--ui` for the test console), `docker compose up` runs it as the `stt` container.

## Files

| File | Purpose |
|---|---|
| `engine.py` | `STTEngine`: audio decode, VAD, chunking, greedy CTC decode |
| `server.py` | FastAPI server |
| `static/` | test console: `index.html`, `app.js`, `style.css` |
| `transcribe.py` | CLI |
| `eval_samples.py` | WER/CER on the bundled samples |
| `metrics.py` | text normalization, WER, CER, word alignment (model-card protocol) |
| `download_model.py` | fetch weights from Hugging Face into `models/` (git-ignored) |
| `fetch_samples.py` | fetch FLEURS test clips into `samples/` (wavs git-ignored) |
