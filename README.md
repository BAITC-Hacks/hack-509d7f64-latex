# hack-509d7f64-latex

Hackathon team repository for Latex

Voice Router: Market and MVP Architecture Analysis
Executive summary

The proposed Voice Router is not only a voice bot. Its main product is a decision layer between speech recognition and business scenarios. That layer must understand the full conversation, choose one or more scenarios, keep state when the user changes topic, support Russian–Kazakh mixed speech, and make a decision fast enough that the call still feels natural. The hackathon brief asks for 40 scenarios, conversations up to 10 turns, a roughly 500 ms routing target, no more than roughly 1.5 seconds from the end of speech to the start of the reply, and a supervisor trace showing the selected scenario, alternatives, reason, and latency.

3 main layers:
S2T -> normalize (Ayat)
Classfier -> in memory key value -> scenario tools -> tools (Sanzhar)

## Layer 2 implementation (Go)

This repository now implements **only the classifier/router layer**. It accepts
normalized text from layer 1 and returns an answer plus a supervisor trace for
layer 3. Speech recognition, normalization, TTS, and the frontend are separate
team components.

The “classifier” is an **OpenAI LLM router**, as required by the brief. It does
not train an encoder intent classifier or map evaluation utterances to labels.
The fixed 40 scenarios, three system intents, slot definitions, knowledge base,
and synthetic backend are embedded into the Go binary. The dataset remains the
source of the hardcoded catalog; there is no scenario-editing endpoint.

### Run

Requires Go 1.25 or later and an OpenAI API key. There are no third-party Go
dependencies and no database to install. From this directory in PowerShell:

```powershell
$env:OPENAI_API_KEY = "your-key"
$env:OPENAI_MODEL = "gpt-4.1-mini" # optional; choose a Responses/Structured Outputs model
go run ./cmd/router
```

The server listens on `http://127.0.0.1:8080`. `LISTEN_ADDR` overrides the address;
`API_TOKEN` enables bearer authentication on all endpoints except `/healthz`.
`.env.example` documents the environment variables; `.env` files are **not**
loaded automatically. Keys stay on the server and are never written to traces.

To build a standalone binary:

```powershell
go build -o voice-router.exe ./cmd/router
```

### Layer 1 → layer 2 contract

`POST /v1/turns` accepts one JSON object:

```json
{
  "session_id": "call-001",
  "request_id": "turn-001",
  "text": "Подскажите адрес офиса в Алматы",
  "language": "ru",
  "reply_language": "ru",
  "slots": {"city": "Almaty"}
}
```

`session_id`, `request_id`, `text`, and `language` are required. Session and
request IDs allow letters, digits, `_`, and `-` (up to 100 characters).
`language` is `ru`, `kk`, or `mixed`. Optional `reply_language` is `ru` or `kk`;
layer 1's explicit language takes precedence. For mixed input without a reply
language, the LLM chooses the dominant language using conversation context.
`slots` is optional: values already extracted by the normalizer override the
router's extraction and must match `slots.json`. Integer slots use JSON numbers,
list slots use JSON arrays, boolean slots use JSON booleans.

Example request:

```powershell
$body = @{
  session_id = "call-001"
  request_id = "turn-001"
  text = "Подскажите адрес офиса в Алматы"
  language = "ru"
  slots = @{city = "Almaty"}
} | ConvertTo-Json -Depth 8
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/v1/turns `
  -ContentType "application/json; charset=utf-8" `
  -Body ([Text.Encoding]::UTF8.GetBytes($body))
```

When `API_TOKEN` is configured, add
`-Headers @{Authorization = "Bearer $env:API_TOKEN"}` to requests.

Use the same session ID for the whole call and a **new request ID for each user
turn**. Retrying identical input with the same request ID returns the stored
answer without calling the LLM or repeating actions. Reusing that ID with
different input returns HTTP 409. Unknown fields and invalid inputs return 400;
full session capacity returns 503.

### Layer 2 → layer 3 contract

The response includes `answer`, `language`, `status`, `active_scenario`,
`pending_scenarios`, and `trace`. `answer` can be passed directly to TTS.
`active_scenario` is present when a workflow still needs input. The selected
scenario(s), confidence, explanation, extracted slots, and alternatives are
always in `trace.decision` after successful routing, including completed turns.

| Status | Meaning |
| --- | --- |
| `awaiting_slot` | Ask the supplied question and submit the next user turn. |
| `awaiting_confirmation` | Read back the proposed operation; wait for explicit consent. |
| `clarification` | Routing was ambiguous; ask the supplied clarification. |
| `completed` | This scenario finished; pending topics remain available. |
| `cancelled` | The client declined the proposed operation. |
| `handoff` | A synthetic operator handoff record was created with conversation context. |

`trace.actions` contains action name, `preview`/`execute` mode, arguments, and
results. `trace.latency_ms` measures routing, tools, response generation, and
total **layer-2** time; it does not claim to measure STT/TTS or end-to-end voice
latency. `trace.error` records provider/response failures without API secrets.

Other endpoints:

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Process health; does not call OpenAI. |
| `GET /v1/scenarios` | Fixed scenario catalog for the operator frontend. |
| `GET /v1/sessions/{session_id}` | Stored inputs, answers, decisions, tool results, active workflow, suspended topics, and queue. |

The frontend can display the returned scenarios and trace. Manual operator
overrides and actual telephony transfers are outside this implementation.

### Execution and state

```text
Normalized turn → save input → OpenAI structured routing → decision policy
                                                        ↓
                           identify → collect slots → execute allowed actions
                                                        ↓
                      confirmation / question / answer / operator handoff
                                                        ↓
                                    save answer → return to the caller
```

The router receives scenario descriptions, boundary rules, examples, slot
definitions, the active state, and the last ten turns. Strict JSON-schema output
is validated again in Go. `dev_utterances.json` and its labels are never sent to
the model. OpenAI is accessed through the standard-library HTTP client using
the [Responses API and Structured Outputs](https://developers.openai.com/api/docs/guides/structured-outputs).
Requests use `store: false`; this is not a claim of zero provider retention.

The executor controls tools in Go; model output cannot name arbitrary tools or
write arbitrary storage keys. Confidence of at least 0.75 permits execution;
lower confidence asks for clarification. Two consecutive scores below 0.45
trigger a handoff. Urgent scenarios go first, other intents retain spoken order.
The primary scenario is handled first and remaining scenarios are queued;
the response offers to return to them on subsequent turns. Suspended scenarios
retain their own slots and action position. An OGPO quote carries its parameters
into a subsequent purchase.

The execution loop stops only when an answer/question/handoff has been stored.
It has a 24-step and 60-second turn budget, so a failed tool/model cannot spin
forever. A model failure creates a stored fallback answer and handoff. Each
session is serialized; unrelated calls can run concurrently.

Irreversible actions require a preview and explicit Russian/Kazakh confirmation
on the next turn, for example `Да`, `Да, верно`, `Подтверждаю`, `Иә`, or
`Иә, тіркеңіз`. The first request cannot confirm itself. Corrections invalidate
the preview and recalculate read-only work; topic switches and intervening
questions require a fresh preview. Each irreversible action is confirmed
separately. A refusal cancels it. Clear but unrecognized confirmation wording
conservatively results in another preview.

**Storage lifetime:** the KV store is an in-process Go map, persistent across
turns while the process runs. It is **not durable across restarts** or shared
between instances. Up to 1,000 sessions are retained, with a one-hour idle TTL
(expired entries are pruned when acquiring a session) and 100 turns per session.
Idempotency applies while the session remains in memory. Backend mutations and
mock receipts also last only for the process lifetime. Durable recovery would
require a transactional database or a persistence-enabled external store.

### Synthetic backend and limits

All 31 catalog actions have local implementations over `mock_backend.json` and
`knowledge_base.json`. The reference date is **2026-10-01**, as specified in the
dataset. Prices use its formulas; unknown IINs receive bonus-malus class 3.
IDs are allocated above the supplied fixture ranges. Backend results are
copied before returning, and mutations are protected by a lock.

No SMS, callback, payment, policy, medical appointment, or operator transfer
reaches a real service. Mock creation/booking/transfer results include
`mock: true`. New policies wait for payment. Phone/IIN lookup is synthetic client
identification, not production authentication.

Deliberate mock simplifications:

- CASCO quotation supports the Standard package; older vehicles are rejected.
- Travel quotation recognizes an explicit set of country names and common
  Russian/Kazakh equivalents. Unmapped countries require operator verification.
- Appointment calendars are not supplied: mocks use one 10:00 slot per location
  and day, prevent duplicate bookings, and suggest the next day when occupied.
- DMS coverage returns the package rules; ambiguous services remain unverified.
  Basic-package specialist booking is referred for verification of a referral.
- Renewal reuses the fixture premium; policy updates currently use a synthetic
  zero extra premium. These are demo rules, not production insurance pricing.
- Response generation is constrained to tool results and the knowledge base,
  but an LLM's wording still needs evaluation. There is no claim of measured
  routing accuracy or achievement of the brief's 500 ms target without a live run.

### Validation and development-set evaluation

```powershell
go test ./...
go vet ./...
go test -race ./... # requires a supported C compiler for cgo
```

Tests use a fake model or local mock OpenAI HTTP server, so they need no API key.
They cover persistence-before-return, idempotency, concurrent sessions,
confirmation/corrections/cancellation, topic return, quote-to-purchase context,
multi-intent priority, confidence thresholds, language, tool errors, pricing,
all catalog action implementations, HTTP contracts, and OpenAI error responses.

For actual routing evaluation, configure `OPENAI_API_KEY` and run:

```powershell
go run ./cmd/evaluate -limit 5  # smoke run; makes paid OpenAI requests
go run ./cmd/evaluate           # all 104 utterances
python voice_router_dataset/evaluate.py predictions.json voice_router_dataset/dev_utterances.json
```

The Go evaluator prints primary accuracy and mean routing latency, then writes
the format accepted by the supplied Python scorer. Python is only needed for
the reference scorer's detailed breakdown. The evaluator invokes routing only;
it does not execute business actions or measure complete multi-turn quality.

Implementation: `internal/router/engine.go` (workflow), `store.go` (KV/session
locking), `openai.go` (LLM calls), `backend.go` (mock tools), `catalog.go` (data
loading/slot validation), and `http.go` (integration API).
