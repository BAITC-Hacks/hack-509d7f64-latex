# Voice Router — classifier layer

Go implementation of **layer 2** of the voice assistant. It accepts normalized
Russian, Kazakh or mixed-language text from layer 1 and picks a scenario with a
hybrid loop: a deterministic pre-router, a lexical retrieval shortlist, a fast
or full OpenAI routing call, and a Go-side uncertainty check. It can ask a user
or operator to review the intent, runs the scenario's synthetic tools, and
saves the answer in SQLite before returning it to layer 3. Speech recognition,
normalization, TTS and the operator frontend are separate.

The Go module and commands live in `backend/`. The fixed 40 scenarios, three
system intents, slot definitions, knowledge base and initial synthetic business
records come from `voice_router_dataset/`. Scenarios are hardcoded. There is
no scenario-editing API and no classifier trained on the evaluation labels.
Design rationale, and what differs from the original proposal, is in
[`DESIGN.md`](DESIGN.md).

## Run

3 main layers:
S2T -> normalize (Ayat)
Classfier -> SQLite-backed state -> scenario tools -> tools (Sanzhar)

### Repository layout

| Folder | What | Port |
|---|---|---|
| `backend/` | **Layer 2**: Go LLM router (OpenAI Responses API + Structured Outputs), synthetic backend, SQLite-backed sessions | 8080 |
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

Router container notes: compose reads the **root** `.env`, not `backend/.env`. It passes
`OPENAI_API_KEY`, `OPENAI_MODEL`, `OPENAI_FAST_MODEL` and `API_TOKEN` to the router, sets
`LISTEN_ADDR=0.0.0.0:8080` and keeps the SQLite file at `/data/voice_router.db` in the
`router-data` volume. Sessions and mock business records therefore survive `docker compose down`.
To reset them, remove the `<project>_router-data` volume. `docker compose down -v` also deletes the
downloaded speech weights. `OPENAI_FALLBACK_MODEL`, `OPERATOR_API_TOKEN` and
`OPERATOR_WEBHOOK_URL` are **not** passed through. In Docker, L1 therefore uses the fast model, the
operator API answers 403, and no webhook fires until you add these variables to the router's
`environment:` block. `make up` runs `docker compose up --build` in the foreground.

### Run locally (venvs, GPU)

```bash
cd backend && go run ./cmd/router                           # reads backend/.env (see below); http://127.0.0.1:8080
scripts/speech_services.sh start                             # STT + TTS + chat; the chat looks for the router at :8080 (add --ui for the STT console)
scripts/speech_services.sh status | stop | logs
```

First-time setup (venvs, weights) is described in `stt/README.md` and `tts/README.md`;
the gateway and its router contract in `chat/README.md`.

### Run the Go router

Requires Go 1.25+ and an OpenAI API key. You don't need a database server. State lives in
one SQLite file through a pure-Go driver (`modernc.org/sqlite`), so cgo isn't needed either.

```bash
cd backend
cp .env.example .env        # put OPENAI_API_KEY in it; backend/.env is gitignored
go run ./cmd/router         # http://127.0.0.1:8080; creates and migrates voice_router.db on first start
curl -s localhost:8080/readyz
```

`make run` from the repository root does the same. `cmd/router`, `cmd/migrate` and
`cmd/evaluate` read `.env` from their working directory at startup. Variables already set in
the environment take precedence, and a missing file is fine. Keep the key in `backend/.env`,
and never commit it or paste it into logs.

On startup the router opens `DB_PATH`, applies the embedded migrations and seeds the synthetic
business records once; later starts never overwrite mutated records. `DB_PATH` (default `voice_router.db`, relative to the working directory) selects
the file. The migrations are embedded in the binary, applied in order and
recorded in `router_schema_migrations`:

| Version | File (`backend/internal/router/migrations/`) | Effect |
|---|---|---|
| 001 | `001_durable_router.sql` | sessions, turns, events, reviews, tool receipts, `mock_backend_state` |
| 002 | `002_seed_mock_backend.sql` | seeds the synthetic backend (clients, policies, claims, payments) once from the embedded `voice_router_dataset/mock_backend.json` and records its SHA-256; restarts never reseed, so tool mutations persist |
| 003 | `003_mock_backend_views.sql` | read-only views `mock_clients`, `mock_policies`, `mock_claims`, `mock_payments`, `mock_records` over the stored JSON |

`cmd/migrate` prints the applied migrations and each view's row count next to
the dataset's count. For repeatable demos:

```bash
go run ./cmd/migrate -reset-data    # re-seed the mock backend and restart generated IDs; sessions and turns stay
go run ./cmd/migrate -reset-all     # also delete sessions, turns, events, reviews, receipts (stop the router first)
sqlite3 -readonly -header -column voice_router.db 'SELECT policy_number, client_id, product, status FROM mock_policies'
```

In Docker the router seeds the `router-data` volume on first start, and the
image ships the same command: `docker compose exec router voice-router-migrate -reset-data`.

Build a binary with `go build -o voice-router ./cmd/router` from `backend/`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `OPENAI_API_KEY` | required | The router exits without it. It stays on the server and never appears in traces. |
| `OPENAI_MODEL` | `gpt-4.1-mini` | Main model: full route and answer wording. |
| `OPENAI_FAST_MODEL` | `gpt-4.1-mini` | Fast-route model (candidates-only prompt; nano measured slower). |
| `OPENAI_FALLBACK_MODEL` | `OPENAI_FAST_MODEL` | Fallback rung L1: repeats the failed full route on this model. |
| `DB_PATH` | `voice_router.db` | SQLite file, relative to the working directory (`/data/voice_router.db` in Docker). Must not contain `?`. |
| `LISTEN_ADDR` | `127.0.0.1:8080` | Listen address (`0.0.0.0:8080` in Docker). |
| `API_TOKEN` | empty | Bearer token for layer-1/user clients. Empty permits unauthenticated local use. |
| `OPERATOR_API_TOKEN` | empty | Enables `review_mode: "operator"` and the `/v1/operator/*` API. Must differ from `API_TOKEN`. |
| `OPERATOR_WEBHOOK_URL` | empty | After each handoff commits, POST the masked ticket here. Empty disables it. |

`backend/.env.example` lists the same variables. PostgreSQL, `DATABASE_URL` and advisory locks
are gone. The earlier PostgreSQL store was replaced by SQLite behind the same
`Repository`/`Lease` interfaces.

## How a turn is routed

One utterance is one pass through the loop below. The LLM is the only component that assigns a
scenario to a new request. Go narrows the candidates, verifies the proposal and executes it. The
pre-router only continues a scenario the model chose on an earlier turn.

```
POST /v1/turns ─▶ validate · lock session · idempotency on request_id
   │   operator ticket open? ──▶ status with_operator, no model call
   ▼
02 pre-router (Go) ── whole utterance = awaited slot value, or explicit yes/no to a preview ─▶ path=bypass ─┐
   │ otherwise                                                                                              │
   ▼                                                                                                        │
03 retrieval shortlist (Go): top-8 scenarios by character n-gram TF-IDF, plus scenarios already in play     │
   ▼                                                                                                        │
04 gate: nothing in play, top-1 is fast_path_eligible, score ≥ 0.30, lead over #2 ≥ 0.10                    │
   │ yes                                              │ no                                                  │
   ▼                                                  ▼                                                     │
05 fast route: small model, ≤ 3 candidates,       05 full route: main model, shortlist details,             │
   no history ── not one confident choice ──▶        one-line index of all 43 IDs, workflow state,          │
   of retrieval's top-1: path=fast>full              last 10 turns, read-only get_scenario tool             │
   └──────────────────────┬───────────────────────────┘      (failures drop down the fallback ladder)       │
                          ▼                                                                                 │
   validate against the catalog (bad slots dropped, decision kept) · urgent intents first                   │
                          ▼                                                                                 │
06 uncertainty (Go) ──▶ 07 policy: execute · clarify · hand off                                             │
                          ▼                                                                                 │
08 executor: identify → required slots → catalog actions; irreversible = preview, then explicit yes  ◀──────┘
                          ▼
09 response: template (slot prompt, preview, dataset closing) or LLM wording
                          ▼
10 trace + SQLite checkpoint · return
```

### Paths

| `trace.path` | When | Routing model calls |
| --- | --- | --- |
| `bypass` | The last turn left the active scenario awaiting a slot (or a yes/no on a preview), and the whole normalized utterance is exactly that: a phone, IIN, policy/claim number, plate, e-mail, number, enum value with ru/kk synonyms, date (`15.10`, `15 октября`, `завтра`) or boolean, or an explicit yes/no. Extra words ("да, но телефон другой"), two slots that both accept the text, and a previous `needs_handoff` all go to the model. | none |
| `fast` | A fresh input with nothing in play (no active, suspended or queued scenario), and retrieval clearly favours one of the nine `fast_path_eligible` scenarios (SC18, SC23, SC24, SC26, SC31, SC33, SC34, SC36, SC37). The small model chooses among up to three candidates plus the system intents. | 1 |
| `fast>full` | The fast answer failed, or wasn't a single choice of retrieval's top-1 with confidence ≥ 0.75. The full route then decides. | 2+ |
| `full` | Everything else. When the model reads extra scenarios with `get_scenario`, the call takes up to four requests. | 1–4 |
| `operator` | A human owns the call, or the turn is an operator action. | none |

The shortlist narrows what the model reads in detail and gives the uncertainty check an
independent opinion. It never picks a scenario. The model still sees a one-line index of every
scenario and system intent, and may choose outside the shortlist. Text with no catalog evidence
yields no candidates beyond the scenarios in play. With no candidates at all, the model reads
every scenario in full.

### Uncertainty

`trace.uncertainty.score` is a weighted mean over the components that apply to the turn.
The model's self-reported confidence is one vote, not the verdict:

| Component | Value | Weight | Applies |
| --- | --- | --- | --- |
| `model` | 1 − confidence of the primary scenario | 1.00 | always (except bypass) |
| `retrieval_margin` | 1 − min(1, (top-1 − top-2 score) / 0.3) | 0.08 | new catalog topic, ≥ 2 candidates |
| `disagreement` | 0 if retrieval ranks a chosen scenario first, 0.5 if in its top 3, otherwise 1 | 0.12 | new catalog topic |
| `boundary` | 1 | 0.30 | only when it fires |

The retrieval components are skipped for system intents, for continuations of a scenario already
in play (active, suspended, queued, last completed, or SC01 → SC02), and when retrieval's leader
scores below 0.1. **Boundary** fires when a `not_this_if` rule of the chosen scenario names a
neighbour that the model itself offers at confidence ≥ 0.5, either as an alternative or as a
secondary intent too weak to queue. `trace.uncertainty.boundary` then shows the rule, e.g.
`SC17→SC19: <condition>`. Bypass turns record `{"bypass": 0}` and execute.

| Score | Verdict | Effect |
| --- | --- | --- |
| ≤ 0.25 | `execute` | System intents answer from the catalog. SC37 at confidence ≥ 0.75 hands off (`operator_requested`). `review_mode` `user`/`operator` pauses for intent review. Anything else goes to the executor, with secondary intents at confidence ≥ 0.75 queued. |
| ≤ 0.55 | `clarify` | Status `clarification`. The question names up to two scenarios from the model's choice and alternatives. The turn does not re-route on its own; the next user turn is routed again with the history. |
| > 0.55 | `handoff` | Clarifies the first time. A second consecutive `handoff` verdict hands off with reason `low_confidence`. |

A primary `SYS_UNCLEAR` always clarifies. As reference points, a 0.95 choice that retrieval ranks
outside its top 3 still executes (≈ 0.21), a 0.6 choice clarifies (≈ 0.33), and a 0.3 choice
counts toward handoff (≈ 0.58).

### Fallback ladder

`trace.fallback_level` records the deepest rung the turn reached:

| Level | Behaviour |
| --- | --- |
| L0 | The route chosen by the gate. Fast route timeout 1.5 s, full route 12 s. The OpenAI client retries once on HTTP 429/5xx. Fast-to-full escalation stays at L0. |
| L1 | The full route failed (error, timeout, or schema/catalog validation). The same prompt is repeated once on `OPENAI_FALLBACK_MODEL`, again with a 12 s limit. |
| L2 | Every model call failed. If retrieval's top-1 leads by ≥ 0.20, the bot asks whether the request matches one of that scenario's catalog examples. Status `clarification`. This rung never executes anything. |
| L3 | Handoff with reason `routing_failed`. |

A lost database or session lease is not a model failure. It aborts the turn with HTTP 503 instead
of falling back.

### Responses

`trace.response_source` says where the answer text came from:

- `template`: slot questions (`slots.json` prompts), irreversible-action previews (fixed wording,
  identifiers masked), system intents, handoff and hold messages, the L2 question, and completed
  scenarios. For 25 of the 40 scenarios, completion speaks the dataset's `responses.closing` with
  placeholders filled from tool results and slots. The template declines when a tool failed or
  was skipped, a transfer ran, or a value isn't a speakable scalar. In those cases the LLM words
  the answer.
- `llm`: clarification questions, safety guidance with urgent slot questions (SC11, SC15, SC38),
  tool-error explanations, knowledge-base answers and closings for the other 15 scenarios, and
  scenario-rule handoff confirmations. IINs, phone numbers and e-mail local parts are masked in
  every LLM answer.
- `fallback`: LLM wording failed, so a fixed bilingual text was used (`trace.error` =
  `response generation failed`).
- `operator`: `with_operator` turns and operator actions.

### Durable execution

- Every input is a checkpointed run: `received → identifying → validating → proposed → accepted →
  executing → generating_answer → finished`, plus `retrieving`, `awaiting_intent_confirmation`,
  `tool_error` and `handoff`. Resubmitting an interrupted request resumes from its checkpoint
  without repeating a saved model result or a tool mutation. A tool's mutation, receipt and
  checkpoint commit in one transaction.
- Limits per input: 3 intent proposals (after that the bot asks a clarification question), 24
  steps and 60 s of active processing. Time spent waiting for a human is excluded. Exceeding the
  step or time budget hands off with `processing_budget`.
- Tables: `sessions` (session JSON with frames, stack, queue, identity, operator ticket, and a
  version), `turns` (one checkpoint per `request_id`; at most one unfinished turn per session),
  `turn_events`, `intent_reviews`, `tool_executions` (receipts) and `mock_backend_state`.
- Full history stays in the database. The model and `GET /v1/sessions/{id}` read the last ten
  finished turns.
- Sessions are serialized by an in-process per-session lock, and unrelated sessions run
  concurrently. A second process on the same file isn't locked out, but optimistic version
  checks reject its stale writes. Run one router process per database file.
- Model requests use the OpenAI Responses API with Structured Outputs and `store: false`. The
  history comes from SQLite, not provider-side storage.

## Trace

Every output carries `trace`. It's what a supervisor panel renders after each utterance:

| Field | Content |
| --- | --- |
| `turn`, `transcript`, `language` | Turn number in the session, the text as received, the layer-1 language hint. |
| `path` | `bypass`, `fast`, `full`, `fast>full` or `operator` (see [Paths](#paths)). |
| `shortlist` | Retrieval candidates `[{scenario_id, score}]`, best first: up to 8 with a positive score plus scenarios in play. Absent on bypass. |
| `decision` | The validated proposal: `scenarios` in spoken order (urgent first), each with `confidence` and `reason`; `alternatives`; reply `language`; extracted `slots`; `is_continuation`; `needs_handoff`. |
| `proposals` | Every proposal this input produced (more than one after an intent-review rejection). |
| `uncertainty` | `score`, `components` (`model`, `retrieval_margin`, `disagreement`, `boundary`, or `bypass`), `verdict` (`execute`, `clarify`, `handoff`) and `boundary` (the `not_this_if` rule that fired). |
| `fallback_level` | 0–3, see [Fallback ladder](#fallback-ladder). |
| `actions` | Tool calls with `name`, `mode` (`preview` or `execute`), `inputs` and `result`. `get_scenario` rows are the router model's read-only lookups. |
| `response_source` | `template`, `llm`, `fallback` or `operator`. |
| `latency_ms` | Layer-2 milliseconds per stage: `prerouter`, `retrieval`, `router` (all routing calls summed), `tools`, `response` (LLM wording) and `total` (active processing). STT and TTS are not measured here. |
| `error` | The last model or validation error, without secrets (e.g. `fast route: …`, `response generation failed`). |
| `handoff` | On a handoff or `with_operator` turn: `reason`, `detail`, `ticket_id`, `queue`, `scenario_id`, `status`, `notification`. |

The same trace is stored with the turn and returned by `GET /v1/sessions/{id}/turns/{request_id}`.
The operator ticket's context packet copies path, shortlist, uncertainty, fallback level, error
and proposals, so the operator sees where the bot doubted.

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
bytes; JSON bodies to 64 KiB. Unknown JSON fields return 400.

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
`pending_scenarios`, and `trace` (see [Trace](#trace)), plus `review` while an
intent review is pending and `handoff` and `operator_messages` when a human
operator is involved (see [Operator handoff](#operator-handoff)).

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

Multi-intent turns run urgent scenarios first (SC11, SC15, SC38) and queue the rest in
`pending_scenarios`. A topic switch suspends the active scenario with its own slots and action
position, and the bot returns to it later. An irreversible action (`actions.json`
`irreversible`) is previewed first and runs only after an explicit yes (`да`, `подтверждаю`,
`верно`, `иә`, `растаймын`, …) on the next turn, with unchanged inputs. Any other turn in between
invalidates the preview.

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
| `operator_requested` | SC37 chosen with confidence ≥ 0.75 and verdict `execute` (the client asked for a person). |
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
progress 409 (busy). This repository has no operator UI, only this API.

| Endpoint | Result |
| --- | --- |
| `GET /v1/operator/handoffs?queue=` | Open tickets, oldest first, with context packet and `client_turns` since the handoff. `queue` must be a catalog queue. The SQLite store implements the listing; 501 only with the in-memory store used by tests and the harness. |
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
| `GET /readyz` | Public SQLite readiness: schema migrated and mock data seeded; 503 otherwise. The success body still labels the store `postgresql`, a leftover in `http.go`. |
| `GET /v1/scenarios` | Fixed scenario catalog and system intents. |
| `GET /v1/sessions/{id}` | Session state (frames, stack, queue, identity, operator ticket) and the last ten finished turns with traces; older turns remain stored. |
| `GET /v1/sessions/{id}/turns/{request_id}` | `{ "phase": "...", "output": { ... } }` from the latest committed checkpoint. |
| `GET /v1/sessions/{id}/messages?after=N` | Operator messages for layer 3; see [Operator handoff](#operator-handoff). |

When `API_TOKEN` is set, every endpoint except `/healthz` and `/readyz` needs a bearer token
(the user or the operator token). With `API_TOKEN` empty, requests without `Authorization` act
as a user, and any bearer value other than the operator token returns 401.

The internal terminal turn phase is `finished`; `output.status` explains its
user-facing outcome. Other phases include `received`, `identifying`,
`retrieving`, `awaiting_intent_confirmation`, and `generating_answer`.
An incomplete checkpoint may have no final answer yet.

HTTP 400 means invalid input or JSON, 401 invalid credentials, 403 credentials
without operator rights, 404 missing state, 409 a conflicting/stale request or
busy session, 501 a store feature that is not available (only the in-memory store),
and 503 unavailable persistence or interrupted processing. Retry transient failures
using the same IDs. The `turn_events` table preserves model decisions, retrieval,
review outcomes, tool operations and workflow progression for diagnosis.

## Synthetic backend

All 31 catalog actions use the supplied mock business records and knowledge
base. The fixture reference date is **2026-10-01**. Mutable mock records and ID
allocation live in SQLite as one JSON document (`mock_backend_state`), seeded
once from the dataset by migration 002. Every tool call reads and writes it inside
the same transaction as its receipt and the turn checkpoint; the read-only
`mock_*` views expose it to `sqlite3`.

The chat frontend displays the returned scenarios and trace (`chat/README.md`
describes how the gateway maps them). Actual telephony transfers are outside
this implementation.

SMS, callbacks, payments, policies, medical bookings, and operator handoffs do
not reach real services; synthetic creation results include `mock: true`.
Phone/IIN lookup is demo identification, not production authentication. Known
mock simplifications include Standard-only CASCO quotes, explicit supported
travel destinations, one daily appointment slot per location, and synthetic
renewal/update pricing.

## Validation and evaluation

Run from `backend/`:

```bash
go test ./...          # scripted fake model, local mock OpenAI server, SQLite on temp files and :memory:; no API key or DB server
go vet ./...
go test -race ./...    # needs a C compiler for cgo
```

`make test` runs `go test ./...` from the repository root. Tests cover the
pre-router parsers, retrieval recall and gate calibration, uncertainty verdicts,
templates, model failures ending in a stored handoff, restart recovery and
duplicate-operation prevention on SQLite, concurrent migration, review
authorization, operator takeover, language, multi-intent/topic return and
separate action consent.

The evaluation harness `cmd/evaluate` drives the real turn loop in-process
(`Engine.Process` with the OpenAI model and an in-memory repository), one fresh
session and synthetic backend per case. It reads `OPENAI_*` from `backend/.env`
(or `-env <file>`):

```bash
go run ./cmd/evaluate -mode retrieval              # offline and free: shortlist recall@1/3/8 on dev utterances
go run ./cmd/evaluate -limit 5                     # paid; -mode single (default): dev utterances
go run ./cmd/evaluate                              # all 104; writes predictions.json and runs ../voice_router_dataset/evaluate.py
go run ./cmd/evaluate -mode dialogs                # paid; the 10 sample dialogs replayed turn by turn
go run ./cmd/evaluate -mode probes                 # paid; 17 jury-style probes in cmd/evaluate/testdata/probes.json
go run ./cmd/evaluate -mode dialogs -ids D03 -out report.json   # selected cases, per-turn JSON report
make eval ARGS="-mode probes"                      # the same from the repository root
```

Each model mode prints primary accuracy, exact-set match, intent recall on
multi-intent turns, breakdowns by language and path (and by type or tag), status
and fallback-level counts, latency p50/p95/max per stage (`prerouter`,
`retrieval`, `router`, `tools`, `response`, `total`, `wall`), the misses with
the model's reason, and a safety check. The check flags an irreversible action
executed without a preview of the same inputs on the previous turn followed by
an explicit consent. Other flags: `-concurrency` (4), `-fast-timeout`/`-full-timeout`
(override the policy), `-turn-timeout`, `-scorer=false`, `-quiet`, `-k`.
Evaluation labels and `dev_utterances.json` are never sent to the routing model
as examples. The official scorer also runs standalone from the repository root:
`python voice_router_dataset/evaluate.py backend/predictions.json voice_router_dataset/dev_utterances.json`.

## Honest limits

- **Latency targets are not claimed.** The brief asks for 500 ms to the scenario and 1.5 s to the
  reply. The bypass path makes no routing call, but the full route has a 12 s timeout because
  requests with `get_scenario` rounds were observed at 8–11 s. The harness prints per-stage
  p50/p95; no measured numbers are frozen here.
- **The fast path is narrow.** With the default gate, 7 of the 104 dev utterances qualify
  (retrieval's top-1 was right for all 7). Most new requests take the full route.
- **Retrieval is lexical.** On the 97 dev utterances with a business label it reaches recall@1
  72.2%, @3 96.9% and @8 100%. That's too weak to veto the model, so it only adds small
  uncertainty components and never raises a boundary conflict on its own.
- **Thresholds are partly reasoned, not swept.** The fast gate (0.30 / 0.10) and the L2 margin
  (0.20) are calibrated on retrieval (`TestRetrievalCalibration`). τ_exec 0.25 and τ_handoff 0.55
  follow from the uncertainty weights. The routing accuracy sweep over the harness output in
  `DESIGN.md` §8 is not built yet.
- **The chat panel shows part of the trace.** `chat/` renders language, scenarios, reason,
  actions and latency. `path`, `shortlist`, `uncertainty`, `fallback_level` and
  `response_source` are in the JSON response but aren't rendered yet. The chat gateway doesn't
  poll operator messages.
- **Layer-1 `slots` are still accepted** and override the model's extraction. The design
  proposed dropping them.
- **One router process per SQLite file.** No multi-instance locking. Docker compose doesn't pass
  the operator variables (see [Run everything with Docker](#run-everything-with-docker)).
- **All business data is synthetic**, and model wording and routing accuracy need evaluation.
