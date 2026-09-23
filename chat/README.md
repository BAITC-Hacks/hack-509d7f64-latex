# Voice Samurai — chat frontend + gateway

Browser chat with the agent by voice or text, in Kazakh, Russian, or both, in the style of
sumi ink on washi paper with vermilion seals. Runs on <http://localhost:9102>.

```
mic (16 kHz PCM) ──ws──▶ gateway ──▶ STT service ──▶ live partial text every 0.7 s
                                  ──▶ final transcript ──▶ Go router (ROUTER_URL) ──▶ reply text
                                  ──▶ TTS service ──▶ reply audio ──ws──▶ browser plays it
```

- **Hold the seal** (or `Space`) to talk, release to send. Partial transcription appears while
  you speak. **Hands-free** mode: tap once, it stops after a 1.2 s pause and listens again after
  each reply. The text box sends typed turns through the same router and voice path.
- **巻 trace** opens the folding-screen panel: language, scenarios with confidence, reason,
  per-stage latency (stt / router / tts / total), and which TTS engine spoke each sentence.
- Status crests: STT, TTS, ROUTER (`go` when the Go service answers, `mock` otherwise), LINK.

## The Go router contract

The gateway posts each user turn to the layer-2 router in `backend/` (`ROUTER_URL`, default
`http://127.0.0.1:8080/v1/turns`, `http://router:8080/v1/turns` in Docker) and reads the reply back:

```
POST $ROUTER_URL   {"session_id": "chat_ab12…", "request_id": "chat_ab12…-t3", "text": "<transcript>", "language": "ru|kk|mixed"}
→ 200 {"answer": "…", "language": "ru|kk", "status": "completed|awaiting_slot|awaiting_confirmation|clarification|cancelled|handoff",
       "active_scenario", "pending_scenarios", "trace": {"decision": {"scenarios", "alternatives", "slots"}, "actions", "latency_ms", "error"}}
```

The chat session id doubles as the router session id and `request_id` is `<session>-t<turn>`, so a retried
turn is idempotent on the router side. `router_client.py` flattens the response into what the trace panel
shows (`language`, `scenarios` + names, `alternatives`, `reason`, `status`, `actions`, `latency_ms`).
`ROUTER_TOKEN` is sent as a bearer token when the router runs with `API_TOKEN`.

`ROUTER_MODE=auto` (default) uses the Go router when reachable and otherwise a keyword mock
over `scenarios.json` (replies are badged **mock router**). `remote` never falls back;
`mock` never calls out.

## API (besides the page)

| Method | Path | What |
|---|---|---|
| `GET` | `/health` | STT / TTS reachability, router mode and reachability |
| `POST` | `/api/session` | `{"session_id"}` (the Go router creates its session implicitly on the first turn) |
| `WS` | `/ws?session_id=` | binary PCM16 16 kHz frames + JSON `start` / `end` / `cancel` / `text`; events `partial`, `final`, `reply`, `audio`, `turn_done`, `notice`, `error` |
| `POST` | `/api/turn` | `{"session_id","text"}` → reply, trace, latency, audio URL (no WebSocket needed) |
| `GET` | `/api/session/{id}/history` | all turns of a session |
| `GET` | `/audio/{id}.wav` | synthesized reply audio (kept in memory, last 60) |

Environment: `CHAT_PORT` (9102), `STT_URL`, `TTS_URL`, `ROUTER_URL`, `ROUTER_TOKEN`,
`ROUTER_MODE`, `DATASET_DIR` (`../voice_router_dataset`, for the mock and scenario names), `TTS_LANG` (`auto`),
`TTS_VOICE_RU`, `TTS_VOICE_KK`.

Mic access needs `localhost` or HTTPS. Files: `server.py` (gateway), `router_client.py`
(Go adapter + mock), `static/` (`index.html`, `style.css`, `app.js`, `worklet.js`).
