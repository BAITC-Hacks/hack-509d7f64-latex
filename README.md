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
`pending_scenarios`, and `trace`. The trace includes the routing decision,
alternatives, explanation, actions/results, and layer-2 timing. It does not
measure speech recognition or TTS latency.

| Status | Client action |
| --- | --- |
| `awaiting_intent_confirmation` | Display `review.question` and submit approval or rejection. |
| `awaiting_slot` | Ask the returned question; submit the next user turn. |
| `awaiting_confirmation` | Read the proposed business action; wait for explicit user consent. |
| `clarification` | Ask the returned clarification question. |
| `completed` | Present the answer; queued topics remain available. |
| `cancelled` | The user declined the proposed business operation. |
| `handoff` | A synthetic operator handoff was recorded with context. |

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

## Other endpoints and failures

| Endpoint | Result |
| --- | --- |
| `GET /healthz` | Public process health; independent of the database and OpenAI. |
| `GET /readyz` | Public database connectivity and schema readiness; 503 when unavailable. |
| `GET /v1/scenarios` | Fixed scenario catalog and system intents. |
| `GET /v1/sessions/{id}` | Current workflow and the last ten finished turns; older turns remain stored. |
| `GET /v1/sessions/{id}/turns/{request_id}` | `{ "phase": "...", "output": { ... } }` from the latest committed checkpoint. |

The internal terminal turn phase is `finished`; `output.status` explains its
user-facing outcome. Other phases include `received`, `identifying`,
`retrieving`, `awaiting_intent_confirmation`, and `generating_answer`.
An incomplete checkpoint may have no final answer yet.

HTTP 400 means invalid input or JSON, 401 invalid credentials, 404 missing state,
409 a conflicting/stale request or busy session, and 503 unavailable persistence
or interrupted processing. Retry transient failures using the same IDs. Stored
events preserve model decisions, retrieval, review outcomes, tool operations,
and workflow progression for diagnosis.

## Synthetic backend

All 31 catalog actions use the supplied mock business records and knowledge
base. The fixture reference date is **2026-10-01**. Mutable mock records and ID
allocation live in PostgreSQL, initially seeded from the dataset. A locked
JSONB backend-state row provides transactional updates for this small dataset.

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
