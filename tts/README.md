# TTS — Russian and Kazakh speech synthesis

Speech output for the voice router, routed by language:

| Language | Engine | Source | Voices | Native rate |
|---|---|---|---|---|
| `ru` | Silero TTS v5 (`v5_cis_base`), neural stress marks via `silero_stress` | [Dimitrius174/silero-v5-tts-ru](https://huggingface.co/Dimitrius174/silero-v5-tts-ru) (wrapper over the `silero` package), MIT | 29 (`ru_eduard`, `ru_aigul`, `ru_ekaterina`, …) | 24 kHz here (model supports 8/24/48) |
| `kk` | ISSAI KazakhTTS: ESPnet Tacotron2 (char-level) + ParallelWaveGAN vocoder | [IS2AI/Kazakh_TTS](https://github.com/IS2AI/Kazakh_TTS), pretrained models from issai.nu.edu.kz | `female1`, `male1` (three more downloadable) | 22.05 kHz |

`lang=auto` splits the text into sentences and routes each one: any Kazakh-only letter
(ә ғ қ ң ө ұ ү һ і) → Kazakh engine, otherwise → Silero. Output is one mono 24 kHz stream.

ISSAI asks for attribution: *Mussakhojayeva et al. (2021), KazakhTTS: An Open-Source Kazakh
Text-to-Speech Synthesis Dataset, Interspeech 2021. ISSAI, Nazarbayev University.*

## Setup (once)

ESPnet needs Python 3.12 or 3.13, so this lives in its own venv (`.venv-tts`), separate from the STT one.

```bash
python3.13 -m venv .venv-tts
.venv-tts/bin/pip install -r tts/requirements.txt
.venv-tts/bin/pip install --no-build-isolation parallel_wavegan   # its setup.py imports pip
.venv-tts/bin/python tts/download_models.py                      # Kazakh female1 + male1 (~245 MB) -> tts/models/
```

The Silero model (87 MB) is downloaded by the `silero` package on first start.
`ffmpeg` is used for mp3/opus output.

## Run

```bash
.venv-tts/bin/python tts/server.py          # http://127.0.0.1:9101
TTS_DEVICE=cpu .venv-tts/bin/python tts/server.py
```

Then the STT console at <http://localhost:9100> gets a **Synthesis** tab (it proxies `/tts/*`
to this service) and every transcript card gets a "read back with TTS" button.

### API

| Method | Path | What |
|---|---|---|
| `GET` | `/health` | which engines loaded, voices, device, load errors |
| `GET` | `/voices` | `{"ru": [...], "kk": [...], "default": {...}}` |
| `POST` | `/synthesize` | JSON `{"text", "lang": "auto\|ru\|kk", "voice_ru", "voice_kk", "format": "wav\|mp3\|opus"}` → JSON with `audio_base64`, `duration_s`, `segments` (per-sentence engine/voice/ms), `timings_ms` |
| `POST` | `/v1/audio/speech` | OpenAI-compatible: `{"model": "auto\|ru\|kk", "input", "voice", "response_format"}` → audio bytes |

```bash
curl -s localhost:9101/synthesize -H 'content-type: application/json' \
  -d '{"text":"Сәлеметсіз бе! Полис на год выйдет тридцать восемь тысяч тенге.","lang":"auto"}' | jq '.segments, .timings_ms'

curl -s localhost:9101/v1/audio/speech -H 'content-type: application/json' \
  -d '{"model":"auto","input":"Қазір есептеп берейін.","voice":"female1","response_format":"mp3"}' -o reply.mp3
```

To point an OpenAI-style client here:
`TTS_BASE_URL=http://127.0.0.1:9101/v1`, `TTS_MODEL=auto`, `TTS_VOICE=ru_eduard` (or `female1`).

Environment: `TTS_PORT` (9101), `TTS_HOST`, `TTS_DEVICE` (`auto`: CUDA for Kazakh if available),
`TTS_RU_DEVICE` (`cpu`; Silero is faster on CPU), `TTS_KK_VOICES` (`female1,male1`),
`TTS_SAMPLE_RATE` (24000), `TTS_RU=0` / `TTS_KK=0` to skip an engine, `TTS_MODELS_DIR`.

## Notes

- **Numbers must be words.** Neither model expands digits; the router's response text should say
  "тридцать восемь тысяч тенге", which is also what the case rules require.
- **Kazakh model input** is lowercased and stripped to the characters in its training alphabet
  (Kazakh + Russian Cyrillic, basic Latin, `. , - : ; ? !`). Mixed sentences with Russian words
  are spoken by the Kazakh model letter by letter, which is intelligible but accented.
- **Placeholders** like `{price}` in `scenarios.json` replies are stripped by the Kazakh cleaner
  and read literally by Silero. Fill them before synthesis.
- Silero on CPU: about RTF 0.04. Kazakh Tacotron2 + PWG on the RTX 4060: about RTF 0.1.
- The first request after start is slower (CUDA warm-up).

## In the voice stack

The Go router in `backend/` does not call this service itself; the chat gateway (`chat/`) does, between the
browser and the router. `scripts/speech_services.sh start` launches it together with the other services
(API only; add `--ui` for the test console), `docker compose up` runs it as the `tts` container.

## Files

| File | Purpose |
|---|---|
| `engine.py` | `RussianTTS`, `KazakhTTS`, `TTSRouter`, `encode_audio` |
| `server.py` | FastAPI service on port 9101 |
| `download_models.py` | fetch and unpack the ISSAI models into `models/` (git-ignored) |
| `requirements.txt` | Python 3.12/3.13 dependencies |
