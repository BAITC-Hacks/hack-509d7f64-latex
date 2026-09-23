# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Hackathon project (HackAlem, case 2): a hybrid Russian/Kazakh voice AI agent for the fictional "Saqta Insurance" contact center. The system has three layers: (1) STT + normalization, (2) an **LLM-based scenario router** that picks a scenario, collects slots and runs tools, (3) TTS/frontend. Layer 2 is the Go HTTP service in `backend/`. `stt/`, `tts/` and `chat/` hold the teammates' speech services and web UI. The brief is the PDF at the repo root. `README.md` holds the full API contract (`POST /v1/turns`, statuses, trace fields, operator API), so read it before changing request or response shapes. `DESIGN.md` records the design, what is implemented, and the deviations.

`voice_router_dataset/` is the organizer-supplied dataset: 40 scenarios + 3 system intents, slots, actions, knowledge base, mock backend fixtures, dev utterances and a Python scorer. Its "today" is **2026-10-01**.

## Commands

All Go commands run from `backend/` (module `voice-router`, Go 1.25; the dataset is a separate module wired by a `replace` directive; the only runtime dependency is pure-Go `modernc.org/sqlite`):

```bash
go build ./...
go test ./...                      # fake models / local mock HTTP server; SQLite tests on temp files and :memory:; no API key, no DB server
go test ./internal/router -run TestName
go vet ./...
go test -race ./...                # needs cgo
go run ./cmd/router                # reads backend/.env; 127.0.0.1:8080; creates + migrates DB_PATH (default voice_router.db)
go run ./cmd/migrate               # optional: migrate/seed DB_PATH without serving
go run ./cmd/evaluate -mode retrieval             # offline, free: shortlist recall@1/3/8
go run ./cmd/evaluate -limit 5                    # paid; -mode single (default, dev utterances + evaluate.py) | dialogs | probes; -ids U001,D03,P05; -out report.json
python voice_router_dataset/evaluate.py backend/predictions.json voice_router_dataset/dev_utterances.json   # from repo root
docker compose up --build          # from repo root: router + STT + TTS + chat; compose reads the ROOT .env
```

`cmd/router`, `cmd/migrate` and `cmd/evaluate` load `.env` from the working directory (existing env vars win). `backend/.env` is gitignored and holds `OPENAI_API_KEY`. Never print, copy or commit it. Config: `OPENAI_MODEL`, `OPENAI_FAST_MODEL`, `OPENAI_FALLBACK_MODEL`, `DB_PATH`, `LISTEN_ADDR`, `API_TOKEN`, `OPERATOR_API_TOKEN`, `OPERATOR_WEBHOOK_URL` (see `backend/.env.example`). There is no `DATABASE_URL` and no PostgreSQL. `make run|test|up|eval` wrap the same commands.

## Current state (2026-09-23)

The hybrid turn loop from `DESIGN.md` is implemented. `DESIGN.md` §0 has the per-stage status, and its deviations are deliberate: uncertain turns clarify without auto re-route, boundary conflicts come only from model alternatives, FullTimeout is 12 s, and layer-1 `slots` are still accepted. State is durable in SQLite. Operator takeover works end to end through the API.

Open items:
- τ_exec/τ_handoff are not swept by the harness;
- the chat HUD doesn't render path/shortlist/uncertainty/fallback;
- `docker-compose.yml` doesn't pass `OPERATOR_*` or `OPENAI_FALLBACK_MODEL`;
- `/readyz` still labels the store `postgresql`, and some Go comments still say PostgreSQL.

## Architecture (`backend/internal/router`)

- **catalog.go**: loads the embedded dataset JSON into `Catalog` (scenarios incl. `fast_path_eligible`, slots, actions, KB, mock seed, queues, `Today`) and validates slot values against `slots.json`. The dataset is the only source of scenarios, and the service has no endpoint for editing them.
- **engine.go**: `Engine.Process` handles intake: validate, lock the session, idempotency on `request_id` (same input replays, different input → `ErrConflict`/409), and hold while an operator owns the call. Then `run()` steps the phase machine. `identify()` runs pre-router → retrieval shortlist → gate (`fastEligible`) → `route()` (fast, escalation to full, L1 fallback model), with L2 retrieval clarification and L3 handoff on failure. `propose()` validates and merges slots. `decide()` takes its verdict from `assess()`: ≤0.25 execute, ≤0.55 clarify, two consecutive `handoff` verdicts → handoff. `Policy` holds the tunables (`DefaultPolicy`). Budget: 3 proposals, 24 steps, 60 s per input.
- **prerouter.go**: `preRoute` gives the `bypass` path with no model call. The whole utterance must be the awaited slot (typed parsers, ru/kk synonyms, dates) or an explicit yes/no to a pending preview.
- **retrieval.go**: `Retriever` ranks scenarios by char 3–5-gram TF-IDF (examples + centroid). It returns the shortlist and never picks a scenario.
- **uncertainty.go**: `assess` computes the weighted mean of the model, retrieval_margin, disagreement and boundary components. A boundary conflict fires only from the model's own alternatives.
- **openai.go / context.go**: the `Model` implementation. It calls the OpenAI **Responses API** with Structured Outputs (strict schema, `store: false`). The full route (`voice-router.intent.v3`) sends a scenario index in the instructions, candidate details, workflow state and the last 10 turns, plus a read-only `get_scenario` tool. The fast route (`voice-router.fast.v1`) sends candidates only and no history. `Respond` words answers from tool facts and the KB. `dev_utterances.json` must **never** be sent to the model.
- **workflow.go / workflow_helpers.go**: the frozen executor (`prepare`/`execute`/`toolError`/`generate`). Per-scenario `Frame`: identify → fill slots → run catalog `actions`. Multi-intent: urgent first, the rest in `Session.Queue`. Topic switch pushes to `Session.Stack`. Irreversible actions need a preview, then an explicit yes on the *next* turn (`explicitYes`/`explicitNo`) with unchanged inputs.
- **templates.go**: speaks the dataset `closing` for 25 allowlisted scenarios when a frame completes, and declines when the facts don't back it. `trace.response_source` records `template`/`llm`/`fallback`/`operator`.
- **sanitize.go**: `sanitizeSlots` drops invalid model slots instead of rejecting the decision. `maskAnswer` masks IIN/phone/e-mail in LLM answers.
- **operator.go**: every give-up path goes through `work.handoff(reason…)` → `transfer()`, which opens a ticket with a context packet in `Session.Operator`. It also implements claim/message/close runs, the handback note, and the async `OPERATOR_WEBHOOK_URL` notification.
- **review_prompt.go**: the intent-review question for `review_mode` `user`/`operator`.
- **backend.go**: synthetic implementations of all 31 catalog actions over `mock_backend.json`/`knowledge_base.json` (pricing, mutations, `mock: true` receipts). The model can't name tools, and the Go executor decides which run.
- **repository.go**: `Repository`/`Lease` interfaces, `Run` (the per-input checkpoint), and errors.
- **store.go** (+ `migrations/*.sql`): the SQLite store. It auto-migrates and seeds once. It uses a single connection, WAL and `BEGIN IMMEDIATE`, with in-process per-session gates and optimistic `version` checks. A tool's mutation + receipt + checkpoint commit in one transaction. It also implements `HandoffLister`.
- **memory_store.go**: the in-memory `Repository` used by tests and `cmd/evaluate` (no `HandoffLister`, so the listing returns 501).
- **http.go**: `/healthz`, `/readyz`, `/v1/turns`, `/v1/scenarios`, `/v1/sessions/{id}`, `…/turns/{request_id}`, `…/reviews`, `…/messages`, `/v1/operator/handoffs…`. `API_TOKEN` gates everything but the health probes. The operator routes need `OPERATOR_API_TOKEN`. Unknown JSON fields return 400.
- **dotenv.go**: `LoadDotEnv`.
- Tests script the model with a `fakeModel` (`engine_test.go`) that pops queued `Decision`s. Follow this pattern for new engine tests instead of calling OpenAI. SQLite tests (`*_sqlite_test.go`) use `t.TempDir()` or `:memory:`.

`cmd/evaluate` replays dev utterances, dialogs or probes through `Engine.Process` with the real model and prints accuracy, per-stage latency and an irreversible-action safety check.

User-facing strings are bilingual. Use `local(lang, ru, kk)`, and read scenario/system responses from the catalog.
