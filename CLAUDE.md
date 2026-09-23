# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Hackathon project (HackAlem, case 2): a hybrid Russian/Kazakh voice AI agent for the fictional "Saqta Insurance" contact center. The system has three layers: (1) STT + normalization, (2) an **LLM-based scenario router** that picks a scenario, collects slots and runs tools, (3) TTS/frontend. **This repo implements only layer 2**, a Go HTTP service in `backend/`. The brief is the PDF at the repo root. `README.md` holds the full API contract (`POST /v1/turns`, statuses, trace format), so read it before changing request or response shapes.

`voice_router_dataset/` is the organizer-supplied dataset: 40 scenarios + 3 system intents, slots, actions, knowledge base, mock backend fixtures, dev utterances and a Python scorer. Its "today" is **2026-10-01**.

## Commands

All Go commands run from `backend/` (module `voice-router`, Go 1.25; the dataset is a separate module wired by a `replace` directive):

```bash
go build ./...
go test ./...                      # uses fake models / local mock HTTP server; no API key needed
go test ./internal/router -run TestName
go vet ./...
go test -race ./...                # needs cgo
OPENAI_API_KEY=... go run ./cmd/router          # serves on 127.0.0.1:8080 (LISTEN_ADDR, OPENAI_MODEL, API_TOKEN)
OPENAI_API_KEY=... go run ./cmd/evaluate -limit 5 -input ../voice_router_dataset/dev_utterances.json   # paid calls
python voice_router_dataset/evaluate.py predictions.json voice_router_dataset/dev_utterances.json      # from repo root
```

`.env` files are not loaded automatically. `cmd/evaluate`'s default `-input` path assumes the dataset is under the current directory.

## Current state (2026-09-23)

Work in progress: implementing the hybrid turn loop from `DESIGN.md` (visual walkthrough: `docs/agentic-loop.html`). `engine.go` `identify()` runs pre-router → retrieval shortlist → fast/full gate → fallback ladder (L0 route, L1 fallback model, L2 retrieval-only clarification, L3 handoff); `decide()` takes its verdict from `assess()` (Go-side uncertainty). Stage logic lives in `prerouter.go`, `retrieval.go`, `uncertainty.go`, `templates.go`. Persistence is moving from PostgreSQL (`store.go`, pgx, advisory locks) to SQLite (pure-Go `modernc.org/sqlite`) behind the unchanged `Repository`/`Lease` interfaces. `backend/.env` (gitignored, auto-loaded by `cmd/router` via `LoadDotEnv`) holds `OPENAI_API_KEY`; never print or commit it.

## Architecture (`backend/internal/router`)

- **catalog.go**: loads the embedded dataset JSON into `Catalog` (scenarios, slots, actions, KB, mock seed, queues, `Today`) and validates slot values against `slots.json`. The dataset is the only source of scenarios, and the service has no endpoint for editing them.
- **openai.go**: the `Model` implementation. It calls the OpenAI **Responses API** with Structured Outputs (strict JSON schema, `store: false`) over the stdlib HTTP client. `Route` returns a `Decision`: ranked scenarios with confidence, reason, slots and alternatives. `Respond` phrases answers constrained to tool facts and the KB.
- **context.go**: builds the routing prompt from scenario descriptions, boundaries (`not_this_if`), examples, slot defs, compacted session state and the last 10 turns. `dev_utterances.json` must **never** be sent to the model.
- **engine.go**: `Engine.Process` runs the turn workflow: validate → lock session → idempotency check on `request_id` (same input replays the stored output, different input returns `ErrConflict`/409) → route → `validateDecision` → policy → per-scenario `Frame` execution (identify → fill slots → run the scenario's `actions` in order) → store answer. Key policies:
  - Confidence ≥0.75 executes. Lower confidence asks for clarification. Two consecutive scores <0.45 hand off to an operator.
  - Multi-intent turns: urgent scenarios go first, the rest go to `Session.Queue`. Topic switches push the active frame to `Session.Stack`, and each suspended frame keeps its own slots and action index.
  - Irreversible actions (`actions.json` `irreversible`) need a preview first, then an explicit yes/no on the *next* turn (`explicitYes`/`explicitNo`). Any intervening turn invalidates `Frame.Pending`.
  - A turn is limited to 24 steps and 60s. Every accepted input ends with a stored answer, including model failures, which produce a fallback answer plus a handoff.
- **backend.go**: synthetic implementations of all catalog actions over `mock_backend.json`/`knowledge_base.json`, including pricing formulas, mutations under a lock, and `mock: true` receipts. The model can't name arbitrary tools. The Go executor decides which tools run.
- **store.go / repository.go**: durable phase machine. Each turn is a `Run` checkpointed after every phase (`received → identifying → validating → proposed → accepted → executing → generating_answer → finished`, plus `handoff`, `tool_error`, `retrieving`, `awaiting_intent_confirmation`), so a crashed turn resumes without repeating model calls or tool mutations. Tests use `memoryTestRepo` (`repository_test.go`).
- **http.go**: `/healthz`, `/v1/turns`, `/v1/scenarios`, `/v1/sessions/{id}`. When `API_TOKEN` is set, every endpoint except `/healthz` needs a bearer token. Unknown JSON fields return 400.
- Tests script the model with a `fakeModel` (`engine_test.go`) that pops queued `Decision`s. Follow this pattern for new engine tests instead of calling OpenAI.

User-facing strings are bilingual. Use `local(lang, ru, kk)`, and read scenario/system responses from the catalog.
