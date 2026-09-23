# Voice Router — classifier layer

Go implementation of **layer 2** of the voice assistant. It accepts normalized
Russian, Kazakh, or mixed-language text from layer 1, identifies a scenario with
the OpenAI API, optionally asks a user or operator to review that intent, executes
the scenario's tools, and saves the answer before returning it to layer 3.
Speech recognition, normalization, TTS, and the operator frontend are separate.

The Go module and commands live in `backend/`. The fixed 40 scenarios, three
system intents, slot definitions, knowledge base, and initial synthetic business
records come from `voice_router_dataset/`. Scenarios remain hardcoded; there is
no scenario-editing API and no classifier trained on the evaluation labels.

## Run

3 main layers:
S2T -> normalize (Ayat)
Classfier -> PostgreSQL-backed state -> scenario tools -> tools (Sanzhar)

### Repository layout

| Folder | What | Port |
|---|---|---|
| `backend/` | **Layer 2**: Go LLM router (OpenAI Responses API + Structured Outputs), synthetic backend, PostgreSQL-backed sessions | 8080 |
| `stt/` | **Layer 1 input**: wav2vec2-CTC Kazakh/Russian STT (`alibiserikbay/kazakh-russian-mixed-stt`) + browser test console | 9100 |
| `tts/` | **Layer 3 output**: Silero v5 (Russian) + ISSAI KazakhTTS (Kazakh), routed per sentence | 9101 |
| `chat/` | **Neonic Samurais** frontend + gateway: mic → STT → router → TTS, answer-language picker, trace panel | 9102 |
| `docker/`, `docker-compose.yml` | the four services as containers (`docker-compose.gpu.yml` = NVIDIA overlay) | |
| `scripts/` | `speech_services.sh` start/stop/status for the Python services, `docker_seed_models.sh` for offline weights | |
| `voice_router_dataset/` | the case dataset: embedded into the Go binary, read by the chat mock and the STT console | |

```
browser ──ws──▶ chat gateway (:9102) ──▶ STT (:9100)              transcript + language
                                     ──▶ Go router (:8080)  POST /v1/turns  ──▶ answer + trace
                                     ──▶ TTS (:9101)              spoken reply ──▶ browser
```

### Run everything with Docker

```bash
cp .env.example .env              # set OPENAI_API_KEY (the router exits without it; the chat then uses its keyword mock)
docker compose up --build -d      # CPU images; first start downloads ~1 GB of speech weights into the "speech-models" volume
open http://localhost:9102        # chat  (http://localhost:9100 = STT test console, http://localhost:8080/healthz = router)
docker compose logs -f router     # scenario decisions and latency per turn
docker compose down               # stop (weights stay in the volume)
```

GPU hosts with the NVIDIA container toolkit:
`docker compose -f docker-compose.yml -f docker-compose.gpu.yml up --build -d`.

If the `stt` container keeps restarting with a Hugging Face `401`, anonymous downloads are being
blocked from your network: either set `HF_TOKEN=hf_...` in `.env`, or download once on the host
(`.venv/bin/python stt/download_model.py`, `.venv-tts/bin/python tts/download_models.py`) and run
`scripts/docker_seed_models.sh` to copy the weights into the volume.

To use a router running outside Docker: `ROUTER_URL=http://host.docker.internal:8080/v1/turns docker compose up -d`.
With `API_TOKEN` set in `.env`, the router requires it as a bearer token and the chat sends it automatically.

### Run locally (venvs, GPU)

```bash
cd backend && OPENAI_API_KEY=sk-... go run ./cmd/router     # http://127.0.0.1:8080
scripts/speech_services.sh start                             # STT + TTS + chat; the chat looks for the router at :8080 (add --ui for the STT console)
scripts/speech_services.sh status | stop | logs
```

First-time setup (venvs, weights) is described in `stt/README.md` and `tts/README.md`;
the gateway and its router contract in `chat/README.md`.

### Run the Go router

Requires Go 1.25+, PostgreSQL (development baseline: 17), and an OpenAI API key.
Create an empty `voice_router` database using your PostgreSQL installation, then
run from the repository root in PowerShell:

```powershell
cd backend
$env:DATABASE_URL = "postgres://postgres:postgres@127.0.0.1:5432/voice_router?sslmode=disable"
go run ./cmd/migrate
$env:OPENAI_API_KEY = "your-key"
$env:OPENAI_MODEL = "gpt-4.1-mini" # optional
$env:API_TOKEN = "your-user-client-token" # optional for local development
$env:OPERATOR_API_TOKEN = "a-different-operator-token" # required for operator review
go run ./cmd/router
```

Use your database credentials in `DATABASE_URL`. Migration creates the schema
and seeds mock records once; re-running it preserves subsequent mutations.
Server startup checks database connectivity and schema readiness and fails if
migration is needed. There is no in-memory production fallback.

The default address is `http://127.0.0.1:8080`; set `LISTEN_ADDR` to override it.
`backend/.env.example` lists the configuration. `.env` files are **not loaded
automatically**. API credentials stay on the server. Build a binary with
`go build -o voice-router.exe ./cmd/router` from `backend/`.

## Logical components and durable execution

```text
Stable system prompt + PostgreSQL history + current input
                         |
                  Identify intent
                         |
             Automatic or human review
                 /                 \
             Rejected             Accepted
                |                     |
      Retrieve scenario details   Run allowed business tools
                |                     |
           Identify again         Generate answer
                                      |
                              Commit answer -> Return
```

The implementation separates these responsibilities:

| Component | Responsibility |
| --- | --- |
| Repository | PostgreSQL sessions, turns, checkpoints, reviews, events, tool receipts, and mock business records. |
| Context builder | Stable assistant instructions, last ten completed turns, active workflow, current input once, and ordered retrieval/review feedback. |
| Intent policy | Validate structured routing output, confidence, language, scenarios, and extracted slots. |
| Intent review | Persist a proposal revision, pause without an open HTTP request, and resume after approval or rejection. |
| Tools | Retrieve hardcoded scenario details; execute only catalog-approved business actions. |
| Coordinator | Advance persisted phases and enforce retry, concurrency, and processing limits. |
| HTTP API | Connect layer 1 and the operator/user interface to that workflow. |

“Model cache” means constructing each model request from durable conversation
state. PostgreSQL owns the history. Requests use the OpenAI Responses API with
structured output and `store: false`; provider-side conversation storage is not
required. Repeated system-prompt prefixes may benefit from provider prompt
caching, but application correctness does not depend on it.

Confidence of at least 0.75 permits automatic acceptance. Lower confidence
retrieves scenario details and retries once, then asks for clarification if still
uncertain. Two consecutive user turns below 0.45 trigger a synthetic handoff.
Urgent scenarios go first; other intents retain spoken order. Queued and
suspended workflows preserve their own slots and action positions.

Each input permits at most three intent proposals, 24 execution steps, and
60 seconds of active processing; human waiting time is excluded. Counters and
checkpoints survive restart. A PostgreSQL advisory lock serializes a session
across server instances; short transactions commit changes between model calls.
No transaction or connection remains held while waiting for human review.

Mock mutations, their operation receipts, and workflow advancement commit
together. Replaying a completed operation does not repeat its mutation. Final
answers and session updates commit before a successful response. If PostgreSQL
is unavailable, the API returns an error; retry the same request after recovery.
Restart recovery is driven by resubmitting the original request or review.

There is no one-hour session eviction or 100-turn lifetime limit. Full history
is stored in PostgreSQL while model context remains bounded. Prior volatile
sessions from the old in-memory implementation are not migrated.

## Turn API

`POST /v1/turns` accepts one JSON object:

```json
{
  "session_id": "call-001",
  "request_id": "turn-001",
  "text": "Подскажите адрес офиса в Алматы",
  "language": "ru",
  "reply_language": "ru",
  "review_mode": "auto",
  "slots": {"city": "Almaty"}
}
```

`session_id`, `request_id`, `text`, and `language` are required. IDs allow
letters, digits, `_`, and `-`, with at most 100 characters. `language` is `ru`,
`kk`, or `mixed`; optional `reply_language` is `ru` or `kk`. The normalizer's
explicit reply language takes precedence; otherwise the model chooses a reply
language for mixed speech. Optional `slots` must match the catalog and override
the initial model extraction. After a rejected proposal, newly extracted
corrections take precedence over the original slots. Text is limited to 8,000
bytes; JSON bodies to 64 KiB.

`review_mode` is `auto` (default), `user`, or `operator`. Operator mode requires
the server's `OPERATOR_API_TOKEN` to be configured. Intent review is separate
from mandatory consent for irreversible business actions.
System goodbye and out-of-scope replies, unclear-intent clarification, and an
explicit operator-handoff request resolve without an intent-review pause.

Use the same session ID throughout the conversation and a new request ID for
each user input. An identical completed request returns its saved response.
Retrying a request awaiting intent review returns its current proposal. Retrying
an interrupted request resumes its checkpoint. Reusing an ID with different
input returns HTTP 409.

Responses include `answer`, `language`, `status`, `active_scenario`,
`pending_scenarios`, and `trace`, plus `handoff` and `operator_messages` when a
human operator is involved (see [Operator handoff](#operator-handoff)). The
trace includes the routing decision, alternatives, explanation, actions/results,
layer-2 timing, and `trace.handoff` (reason code and ticket) on a handoff. It
does not measure speech recognition or TTS latency.

| Status | Client action |
| --- | --- |
| `awaiting_intent_confirmation` | Display `review.question` and submit approval or rejection. |
| `awaiting_slot` | Ask the returned question; submit the next user turn. |
| `awaiting_confirmation` | Read the proposed business action; wait for explicit user consent. |
| `clarification` | Ask the returned clarification question. |
| `completed` | Present the answer; queued topics remain available. |
| `cancelled` | The user declined the proposed business operation. |
| `handoff` | The bot gave up: `handoff` has `ticket_id`, `queue`, `reason`, `status` (`waiting`, or `failed` when no operator could be reached). |
| `with_operator` | A human owns the call; the bot did not answer. Speak `answer` (hold message while waiting, empty once connected) and every `operator_messages[].text`. |

## Intent review API

A pending response includes `review` with `proposal_id`, `revision`, `target`,
`question`, `decision`, and `status`. Submit feedback to
`POST /v1/sessions/{session_id}/reviews`:

```json
{
  "request_id": "review-001",
  "turn_request_id": "turn-001",
  "proposal_id": "copy-from-review-response",
  "revision": 1,
  "decision": "rejected",
  "feedback": "I need the office opening hours, not its address."
}
```

Use `approved` or `rejected` for `decision`. `feedback` is optional. Approval
accepts exactly that proposal revision. Rejection stores feedback, retrieves
scenario information, and identifies the intent again without executing the
rejected scenario's business actions. Repeated rejection eventually asks for
clarification rather than looping forever.

The response has the same shape as the turn response and may contain a revised
proposal. Stale proposals, mismatched reviewer credentials, or conflicting
request reuse return 409. Identical review retries return the saved result.
When a **user** review is pending, a clear yes/no in the next user turn can also
resolve it; corrections become rejection feedback. Operator reviews use the
review endpoint. A natural-language review reply updates the original turn, so
its response retains the original `request_id`; poll using `turn_request_id`,
not the feedback request ID.

Set `Authorization: Bearer <API_TOKEN>` for user clients, or
`Authorization: Bearer <OPERATOR_API_TOKEN>` for operator clients. Both tokens
can access protected endpoints, but only credentials matching a proposal's
target can review it. Reviewer identity is never accepted in JSON. Tokens must
differ. With `API_TOKEN` empty, unauthenticated local requests act as a user.

Irreversible actions still need their own preview and explicit Russian/Kazakh
confirmation, such as `Да` or `Иә`, on a later user turn. Intent approval cannot
provide that consent. Corrections invalidate action previews, and each
irreversible action is confirmed separately.

## Operator handoff

Every path on which the bot gives up opens the same operator ticket through
the synthetic `transfer_to_operator` action. The reason code is in
`output.handoff.reason`, `trace.handoff`, and the ticket:

| Reason | Trigger |
| --- | --- |
| `operator_requested` | SC37 chosen with confidence ≥ 0.75 (the client asked for a person). |
| `low_confidence` | Two consecutive turns with uncertainty verdict `handoff`. |
| `routing_failed` | Fallback ladder bottomed out (L3): every model call failed and retrieval could not ask. |
| `invalid_decision` | A stored model proposal failed catalog validation. |
| `processing_budget` | 24 steps or 60 s of active processing exceeded. |
| `tool_failed` | A tool failed twice, or stayed `service_unavailable` after one retry. |
| `identification_failed` | The client was not found twice. |
| `urgent_scenario` | SC11 with injured people or `needs_handoff`, SC15 or SC38 with `needs_handoff`. |
| `scenario_handoff` | The scenario's own `handoff.when` rule, e.g. SC10 always, SC30 `charged_policy_not_issued`, `needs_handoff`. |

The ticket's queue is the scenario's `handoff.queue` for scenario, urgent and
tool/identification failures, otherwise `operator_general`. The ticket lives in
the session JSON (`session.operator`, closed ones in `session.operator_history`)
with status `waiting` → `connected` → `closed` and a context packet: the last
ten turns, current input, active frame with slots, pending scenarios, identity,
the last decision with alternatives, uncertainty components, routing path,
shortlist, fallback level, error, and the last tool call. Supervisors see there
where the bot doubted.

While a ticket is open, user turns never reach the bot and make no model call.
`POST /v1/turns` still applies idempotency and returns `status: with_operator`,
the ticket in `handoff`, the client's words are appended to the ticket, and
`operator_messages` carries operator messages not delivered in an earlier
output. Operator actions are stored as ordinary turns (`operator_connected`,
`operator_message`, `operator_closed`), so the dialog history, and the bot
after a handback, contain them.

Operator endpoints require `Authorization: Bearer <OPERATOR_API_TOKEN>`: a
missing token returns 401, a user token 403, and 403 also when the server has
no `OPERATOR_API_TOKEN`. Each action takes a fresh `request_id`: an identical
retry returns the saved result, a changed payload 409, and a user turn still in
progress 409 (busy).

| Endpoint | Result |
| --- | --- |
| `GET /v1/operator/handoffs?queue=` | Open tickets, oldest first, with context packet and `client_turns` since the handoff. 501 when the store cannot list sessions. |
| `GET /v1/operator/handoffs/{session_id}` | The open ticket (or the latest closed one) with context, conversation, and `recent_turns` with full traces. |
| `POST /v1/operator/handoffs/{session_id}/claim` | `{"request_id":"op-1","operator":"Aigerim"}`: `waiting` → `connected`. |
| `POST /v1/operator/handoffs/{session_id}/messages` | `{"request_id":"op-2","text":"..."}`: an operator message; the ticket must be connected. |
| `POST /v1/operator/handoffs/{session_id}/close` | `{"request_id":"op-3","resolution":"...","return_to_bot":true}`: closes the ticket. |
| `GET /v1/sessions/{session_id}/messages?after=N` | Layer 3 polling (user or operator token): operator and system lines with turn number > `N`, plus the current ticket summary and `last_turn`. |

With `return_to_bot: true` the active workflow, stack and queue stay (stale
previews and failure counters are reset) and the bot answers the next user
turn, which also receives a routing note with the resolution. Without it the
workflow is cleared and the close result's `answer` (and a `system` line in the
message feed) is the bilingual closing notice.

```json
{"session_id":"call-001","request_id":"turn-007","answer":"","status":"with_operator",
 "handoff":{"ticket_id":"HO-900001","queue":"claims_team","reason":"urgent_scenario","status":"connected"},
 "operator_messages":[{"turn":6,"role":"operator","text":"Скорая уже едет.","operator":"Aigerim","request_id":"op-2","at":"2026-10-01T09:00:00Z"}],
 "trace":{"turn":7,"path":"operator","...":"..."}}
```

Optional `OPERATOR_WEBHOOK_URL` notifies real people: after a handoff commits,
the server POSTs `{"event":"handoff.created","session_id":...,"ticket":{...}}`
with phone/IIN/e-mail values masked to their last four characters and an
`Idempotency-Key: <ticket_id>` header, asynchronously with a 3 s timeout. No
API key is sent. Failures are logged and recorded as `ticket.notification`
(`pending`, `sent`, or `failed`) with an `operator_notification` event at the
session's next write; they never delay or fail the turn. Unset means no
notification.

## Other endpoints and failures

| Endpoint | Result |
| --- | --- |
| `GET /healthz` | Public process health; independent of the database and OpenAI. |
| `GET /readyz` | Public database connectivity and schema readiness; 503 when unavailable. |
| `GET /v1/scenarios` | Fixed scenario catalog and system intents. |
| `GET /v1/sessions/{id}` | Current workflow and the last ten finished turns; older turns remain stored. |
| `GET /v1/sessions/{id}/turns/{request_id}` | `{ "phase": "...", "output": { ... } }` from the latest committed checkpoint. |
| `GET /v1/sessions/{id}/messages?after=N` | Operator messages for layer 3; see [Operator handoff](#operator-handoff). |

The internal terminal turn phase is `finished`; `output.status` explains its
user-facing outcome. Other phases include `received`, `identifying`,
`retrieving`, `awaiting_intent_confirmation`, and `generating_answer`.
An incomplete checkpoint may have no final answer yet.

HTTP 400 means invalid input or JSON, 401 invalid credentials, 403 credentials
without operator rights, 404 missing state, 409 a conflicting/stale request or
busy session, 501 a store feature that is not available, and 503 unavailable
persistence or interrupted processing. Retry transient failures using the same IDs. Stored
events preserve model decisions, retrieval, review outcomes, tool operations,
and workflow progression for diagnosis.

## Synthetic backend

All 31 catalog actions use the supplied mock business records and knowledge
base. The fixture reference date is **2026-10-01**. Mutable mock records and ID
allocation live in PostgreSQL, initially seeded from the dataset. A locked
JSONB backend-state row provides transactional updates for this small dataset.

The chat frontend displays the returned scenarios and trace (`chat/README.md`
describes how the gateway maps them). Manual operator overrides and actual
telephony transfers are outside this implementation.

SMS, callbacks, payments, policies, medical bookings, and operator handoffs do
not reach real services; synthetic creation results include `mock: true`.
Phone/IIN lookup is demo identification, not production authentication. Known
mock simplifications include Standard-only CASCO quotes, explicit supported
travel destinations, one daily appointment slot per location, and synthetic
renewal/update pricing. Model wording and routing accuracy require evaluation;
this implementation does not claim the brief's latency target has been met.

## Validation and evaluation

Run from `backend/`:

```powershell
go test ./...
go vet ./...
go test -race ./... # requires a supported C compiler for cgo
$env:TEST_DATABASE_URL = "postgres://postgres:postgres@127.0.0.1:5432/voice_router_test?sslmode=disable"
go test ./internal/router -run Postgres -count=1
```

Ordinary tests use a fake repository/model or mock OpenAI HTTP server and do not
need API credentials. PostgreSQL integration tests run when
`TEST_DATABASE_URL` is configured; use a dedicated test database. Each integration
test creates and drops an isolated schema, so the test role needs schema-creation
permission. Tests cover
review authorization, stale/replayed feedback, restart recovery, duplicate
operation prevention, database failures, context assembly, language, confidence,
multi-intent/topic return, and separate action consent.

For live routing evaluation, set `OPENAI_API_KEY` and run from `backend/`:

```powershell
go run ./cmd/evaluate -input ../voice_router_dataset/dev_utterances.json -limit 5
go run ./cmd/evaluate -input ../voice_router_dataset/dev_utterances.json
python ../voice_router_dataset/evaluate.py predictions.json ../voice_router_dataset/dev_utterances.json
```

These commands make paid OpenAI requests. The evaluator measures routing only,
without business actions or full multi-turn quality. Evaluation labels are
never sent to the routing model.
