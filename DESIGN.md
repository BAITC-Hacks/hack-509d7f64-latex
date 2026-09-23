# Voice Router — Design

Layer 2 of the HackAlem case 2 solution: the decision layer between speech recognition and business scenarios for the fictional Saqta Insurance contact center. This document is the design, and it records how the code in `backend/` implements it. Each stage is marked **Implemented**, **Implemented, with deviations** or **Not implemented**, and every deviation gives its reason. `README.md` has the run instructions and the API contract.

`docs/agentic-loop.html` is the original visual proposal of the per-turn loop (open it locally in a browser). Its "Change vs. today" boxes describe the code as it was before this work. Where the page and this file disagree, this file is current.

Status: implemented on `main`, 2026-09-23, except where marked. Dataset snapshot date is 2026-10-01.

## 0. Status at a glance

| Item | Status | Code |
|---|---|---|
| 5.0 intake, idempotency | Implemented | `engine.go` `Process` |
| 5.1 session state | Implemented, **SQLite** instead of in-memory | `store.go`, `migrations/` |
| 5.2 deterministic pre-router | Implemented | `prerouter.go` |
| 5.3 retrieval shortlist | Implemented, with deviations | `retrieval.go` |
| 5.4 fast/full gate | Implemented, slightly stricter | `engine.go` `fastEligible` |
| 5.5 fast and full LLM route | Implemented, with deviations (timeouts) | `openai.go`, `context.go`, `engine.go` `route` |
| 5.6 uncertainty score | Implemented, with deviations | `uncertainty.go` |
| 5.7 policy | Implemented, with deviations | `engine.go` `decide` |
| 5.8 executor | Frozen as planned | `workflow.go`, `workflow_helpers.go` |
| 5.9 templated responses | Implemented for completed frames of 25/40 scenarios | `templates.go`, `sanitize.go` |
| 5.10 trace and persistence | Implemented | `types.go`, `store.go` |
| §3 drop layer-1 `slots` | **Not implemented** | `engine.go` `propose` |
| §7 fallback ladder | Implemented | `engine.go` `identify`, `route` |
| §8 offline harness | Implemented, except the threshold sweep | `cmd/evaluate` |
| Operator takeover with context | Implemented (added to scope) | `operator.go`, `http.go` |
| Supervisor panel | Data complete in the trace; `chat/` renders a subset | `chat/` |

## 1. Goal and what is scored

The brief scores routing quality, explainability and reproducibility, not STT/TTS quality. Latency is a bonus: 500 ms for the scenario choice, 1.5 s from end of speech to start of reply. Must-haves: voice in the browser, LLM at the point of the scenario decision (no encoder intent classifier), a trace panel after every utterance (scenario, reason, alternatives, time per stage), Russian and Kazakh including mixed speech, explicit confirmation before irreversible actions, handoff to an operator with context.

Optional items the design deliberately targets: hybrid architecture with a measurable latency gain, context retention and return to an interrupted topic, clarification instead of guessing, slot extraction from speech.

## 2. Principles

1. **The LLM chooses every scenario.** No other component ever maps a new utterance to a scenario ID. Retrieval narrows the candidates. Deterministic code only continues a scenario the model already chose.
2. **The LLM proposes, Go verifies and executes.** Model output is validated against the catalog and the session state before anything runs. Tools are Go functions. The model cannot name a tool, a storage key or a scenario outside the catalog.
3. **Every stage emits a trace row** with milliseconds, candidates, choice, reason, alternatives and the path taken. The supervisor panel renders the trace. It is not a separate feature.
4. **No dead ends.** Every model failure drops one rung down a fallback ladder. The caller always receives a stored answer within the turn budget.
5. **Measure before tuning.** Thresholds come from an offline harness over the dataset, are frozen, and are written down. Only the retrieval-side thresholds meet this so far (see §8).

## 3. System context

```
mic ──► Layer 1: STT + normalization ──► Layer 2: router (this repo) ──► Layer 3: TTS + web UI
                                              │
                                              └─► trace ──► supervisor panel
```

Layer 1 → layer 2: `POST /v1/turns` with `session_id`, `request_id`, `text` (normalized transcript), `language` (`ru` | `kk` | `mixed`), optional `reply_language`, optional `review_mode`.

**Not implemented:** the proposal removed the optional `slots` field, because two extractors with an override rule are two sources of truth. It is still accepted and validated against the catalog, and it overrides the model's extraction. After an intent-review rejection, the model's newer corrections win. The chat gateway does not send it.

Layer 2 → layer 3: `answer`, `language`, `status`, `active_scenario`, `pending_scenarios`, `trace`, plus `review`, `handoff` and `operator_messages` when they apply. `trace` gained the fields listed in §5.10.

## 4. State

**Implemented, with deviation: SQLite, not in-memory.** Per session, in one SQLite file (`DB_PATH`, pure-Go `modernc.org/sqlite`), migrated and seeded at startup:

- `language`, `identity` (phone / IIN / client once verified)
- `active` frame: `scenario_id`, `slots`, `next_action`, `results`, `pending` preview, `failures`, `awaiting` (the slots the last question asked for, which the pre-router reads)
- `last_completed` frame: used for follow-ups (SC01 quote → SC02 purchase) and the uncertainty in-play check
- `stack`: suspended frames (topic switch), each with its own slots and action position
- `queue`: intents from a multi-intent turn not yet started
- `low_confidence`: consecutive turns with an uncertainty verdict of `handoff`
- `operator` ticket and `operator_history`
- `version` for optimistic concurrency, `turn_count`, pending intent review

Each input is a run in the `turns` table, checkpointed after every phase. Tool receipts (`tool_executions`) and the mock business state (`mock_backend_state`) commit in the same transaction as the checkpoint. A resubmitted interrupted request resumes without repeating a model result or a mutation. The model and `GET /v1/sessions/{id}` read the last 10 finished turns, and the full history stays in the database. An in-process per-session lock serializes turns, and unrelated sessions run concurrently.

Why the deviation: the proposal kept the in-memory map (1000 sessions, 1 h TTL, nothing durable) and parked the PostgreSQL lease layer. SQLite gives durability with no server to run, behind the same `Repository`/`Lease` interfaces, so the engine did not change. PostgreSQL, `pgx` and advisory locks are gone. One router process per database file: a second process is not locked out, and version checks reject its stale writes. The in-memory repository (`memory_store.go`) remains for tests and the harness.

## 5. The turn loop

One utterance = one pass. Stages in order. Three paths through them converge on the same executor, response and trace.

### 5.0 Transcript arrives — Implemented
Validate, lock the session, apply idempotency: the same `request_id` with identical input replays the stored output; with different input returns 409. While an operator ticket is open, the turn ends here with `with_operator` and no model call.

### 5.1 Load session — Implemented
Read the state above. Reply language: explicit `reply_language` wins, then a non-mixed `language`, otherwise the model decides from context.

### 5.2 Deterministic pre-router (Go, ~1 ms) — Implemented
Handles exactly two situations, both continuations of a scenario the LLM chose earlier. The last finished turn must have left this same frame active:

- The last turn ended `awaiting_slot` and the whole normalized utterance parses as exactly one of the awaited slots per `slots.json`: phone (`+7`/`7`/`8` forms), IIN and IIN lists, policy and claim numbers, plates (Cyrillic look-alikes mapped to Latin), e-mail, integers, enums with a small ru/kk synonym table, dates (numeric, month names, relative to the dataset's today; a yearless date takes the nearest year on the slot's side of today) and booleans. Fill the slot, go to 5.8.
- The last turn ended `awaiting_confirmation` with a pending preview and the utterance is an explicit yes/no from a fixed ru/kk lexicon. Resolve, go to 5.8.

Anything else goes on to 5.3, including "да, но телефон другой", text that two awaited slots would both accept, and any turn after the model set `needs_handoff`. **Ownership rule, as designed:** an irreversible action executes only when the decision has `is_continuation=true`, the text is an explicit affirmative, and the previewed inputs are unchanged. A bypass decision carries `is_continuation=true` with confidence 1. On the model path, the model has to say so.

### 5.3 Retrieval shortlist (Go, ~30 µs) — Implemented, with deviations
Character 3–5-gram TF-IDF with cosine similarity. A scenario scores 0.2 × its best single example (ru + kk) + 0.8 × its centroid. The centroid also includes the name, description and the opening/closing response templates. Output: up to 8 scenario IDs with a positive score, plus the active, suspended and queued scenario IDs.

Deviations:
- No embeddings and no BM25. The n-grams tolerate Russian/Kazakh inflection and mixed speech without a stemmer or network call, and scores are bit-for-bit reproducible.
- `not_this_if` conditions are **not** indexed, because they describe other scenarios.
- System intents are not retrieved. The model always sees them: the full route through its scenario index, the fast route as a list.
- Text with no catalog evidence yields no candidates, and the model then reads the whole catalog.

Weights were tuned by leave-one-example-out over the catalog's own examples. Dev utterances only measure the result: on the 97 dev utterances with a business label, recall@1 is 72.2%, @3 96.9% and @8 100% (`go run ./cmd/evaluate -mode retrieval`). Retrieval never assigns a scenario, and the trace shows the shortlist next to the model's choice.

### 5.4 Gate (Go) — Implemented, slightly stricter
Fast path when all of these hold, full path otherwise:

- top-1 has `fast_path_eligible: true` (SC18, SC23, SC24, SC26, SC31, SC33, SC34, SC36, SC37);
- top-1 score ≥ `FastMinScore` 0.30 and top-1 leads top-2 by ≥ `τ_fast` = `FastMargin` 0.10;
- **nothing is in play**: no active frame, empty stack **and queue**;
- the first routing attempt of a fresh input, with no intent-review feedback and no operator handback note.

The stricter third condition exists because the fast route reads no dialog state, so it must never be the one to continue a workflow. Thresholds come from `TestRetrievalCalibration`, set to keep top-1 precision ≥ 0.97 among admitted utterances. At the defaults, 7 of the 104 dev utterances pass the gate, and retrieval's top-1 is right for all 7. The fast path is therefore rare.

### 5.5 LLM route — Implemented, with deviations
**Fast route** (`OPENAI_FAST_MODEL`, default `gpt-4.1-mini` — nano measured 2.5–3 s, mini 1.6–2.6 s on this prompt; prompt `voice-router.fast.v1`): the utterance, up to 3 candidates with full details and slot definitions, the system intents, the dataset date. No history and no tools. The schema's scenario enum is narrowed to those IDs. Go **escalates** to the full route (`path` = `fast>full`) unless the answer is a single scenario equal to retrieval's top-1 with confidence ≥ 1 − τ_exec = 0.75.

**Full route** (`OPENAI_MODEL`, default `gpt-4.1-mini`, prompt `voice-router.intent.v3`): stable instructions with the rules plus a one-line index of all 40 scenarios and 3 system intents. That prefix is shared across requests for provider prompt caching, which correctness does not depend on. The input carries, in order:
- the last 10 turns as chat messages;
- the compact `workflow_context` (active, last completed, suspended, queued, identity, low-confidence count);
- `candidate_scenarios`: full details (`not_this_if`, examples, slots, actions, handoff) and slot definitions for the shortlist only;
- `current_input`;
- routing events from this input.

A read-only `get_scenario` tool lets the model read a scenario outside the shortlist, for up to 4 requests per call. Output: scenarios in spoken order, alternatives, reason, slots, `is_continuation`, `needs_handoff`. The schema is strict, slot names are restricted to catalog names, and Go validates everything again. An invalid extracted slot is dropped (enum case canonicalized first) instead of discarding a correct scenario choice.

Deviations:
- **Timeouts are 1.5 s fast and 12 s full**, not 600 / 1200 ms. Full-route calls that used `get_scenario` rounds took 8–11 s, and a shorter limit turned them into fallbacks.
- Two per-scenario hints stay in the instructions (illness abroad → SC15, fraud → SC38), because SC16/SC21 and SC30/SC34 have no matching `not_this_if` rule. The other exceptions moved to the dataset's `not_this_if`, as designed.
- The prompt is smaller than the old all-scenario prompt, but the planned 5–8× reduction was not measured.

`dev_utterances.json` is never sent to the model as examples.

### 5.6 Uncertainty score (Go) — Implemented, with deviations
```
u = Σ wᵢ·cᵢ / Σ wᵢ   over the components that apply this turn
  model             1 − llm_confidence                             w = 1.00   one vote, not the verdict
  retrieval_margin  1 − min(1, (top1 − top2) / 0.3)                w = 0.08
  disagreement      0 if retrieval ranks a chosen ID 1st,          w = 0.12
                    .5 if in its top 3, else 1
  boundary          1 when it fires                                w = 0.30   joins only when it fires
```
Verdict: `u ≤ τ_exec = 0.25` execute, `u ≤ τ_handoff = 0.55` clarify, above that handoff. All components, the score, the verdict and the boundary rule go into `trace.uncertainty`.

Deviations:
- **Weighted mean, not a sum**, over only the components that apply. Retrieval components are skipped for system intents, for continuations of a scenario in play, and when retrieval's leader scores below 0.1 (no evidence). "No boundary conflict" is not evidence that a choice is right, so it does not dilute the model's vote.
- **Boundary conflicts come only from the model's alternatives.** The design fired a conflict when a `not_this_if` neighbour of the chosen scenario was "retrieved strongly". Lexical retrieval ranks look-alike neighbours (quote vs purchase) above the right one too often (dev recall@1 ≈ 0.72), and it made correct confident choices clarify. Now the rule fires only when the model itself offers the `use_instead` scenario at confidence ≥ 0.5, as an alternative or as a secondary intent too weak to queue (< 0.75). Retrieval still counts through `disagreement`.
- **τ_exec and τ_handoff are set from the weights, not calibrated.** Examples: a 0.95 choice that retrieval ranks outside its top 3 still executes (≈ 0.21), a 0.6 choice clarifies (≈ 0.33), and a 0.3 choice counts toward handoff (≈ 0.58). The §8 sweep that should set them is not built.

### 5.7 Policy (Go) — Implemented, with deviations
- **Confident:** execute. Urgent scenarios (SC11, SC15, SC38) sort first. Secondary intents at confidence ≥ 0.75 are queued, and the answer offers to return to them. System intents answer from the catalog. `review_mode` `user` or `operator` pauses here for intent review.
- **Uncertain:** status `clarification` with up to two named options from the model's scenarios and alternatives, worded by the LLM with a fixed fallback. Skips the executor. A primary `SYS_UNCLEAR` always lands here.
- **Hand off** through one ticket path with a reason code (`operator.go`) when:
  - two consecutive verdicts are `handoff` (a clarify or execute verdict resets the count);
  - an urgent scenario applies (SC11 with injured people, or SC11/SC15/SC38 with `needs_handoff`);
  - a scenario's own `handoff.when` rule fires at its `transfer_to_operator` step;
  - SC37 is chosen at ≥ 0.75;
  - the fallback ladder bottoms out;
  - a stored decision is invalid;
  - the step or time budget is exceeded;
  - a tool keeps failing, or identification fails twice.

  The ticket carries the context packet: transcript, current input, chosen scenario and alternatives, slots, queue, identity, uncertainty, path, shortlist, fallback level and last tool call. Skips the executor.

Deviations:
- **Uncertain turns clarify without an automatic re-route.** The code before this work re-identified once with retrieved scenario details on low confidence. Now the clarification goes straight to the caller, and the next utterance is routed again with the full history. Re-routing with retrieved details still happens after an intent-review rejection (at most 3 proposals per input).
- Clarification options come from the model only, not from the model ∪ retrieval top-2, for the same reason as the boundary rule.

### 5.8 Executor (Go, ~10 ms on mocks) — Frozen as planned
Identify if `requires_identification` → collect required slots in catalog order → run `actions` in catalog order. An `irreversible` action produces a preview and a pending frame, and the next turn's verified yes runs it. Corrections invalidate the preview and recompute read-only work. Topic switches push the active frame to the stack, and returning pops it.

The executor gained no new semantics. The only changes support other stages: `Frame.Awaiting` for the pre-router, handoffs routed into the common ticket path, and slot sanitization before validation.

### 5.9 Response — Implemented
Templated where the facts fully back the wording, which is deterministic, costs 0 ms and cannot claim something that did not happen:
- slot questions from `slots.json → prompt`;
- the fixed confirmation preview, with identifiers masked;
- system intent responses;
- handoff and hold messages;
- the L2 question;
- for 25 of the 40 scenarios, the dataset's `responses.closing` once the frame completes, with placeholders filled from tool results (latest first, top-level scalars only) and then slots.

The template declines when an action failed, was only previewed or was skipped, a transfer ran, or a value is not a speakable scalar. The 15 other scenarios are listed in `templates.go` with a reason per group: knowledge-base answers, English backend text, lists, always-handoff, and closings that assert more than the actions do.

The LLM `Respond` call runs for:
- clarification wording;
- safety guidance with urgent slot questions;
- tool-error explanations;
- scenario-rule handoff confirmations;
- the untemplated closings.

IINs, phone numbers and e-mail local parts are masked in every LLM answer. `trace.response_source` is `template`, `llm`, `fallback` (wording failed, fixed text used) or `operator`.

Deviation: `responses.opening` is not spoken; retrieval indexes it.

### 5.10 Trace + persist — Implemented
Per turn:
- `path` (`bypass` | `fast` | `full` | `fast>full` | `operator`);
- `shortlist` with scores;
- `decision` with each scenario's reason and the alternatives, plus every proposal;
- `uncertainty` (score, components, verdict, `boundary` rule);
- `fallback_level`;
- `actions` with `preview` / `execute` mode, inputs and results;
- `response_source`;
- `latency_ms` per stage (`prerouter`, `retrieval`, `router`, `tools`, `response`, `total`);
- `error` without secrets;
- `handoff`.

The trace is stored with the run under `request_id` in SQLite, with an event log in `turn_events`. Then the session unlocks and the output returns.

## 6. Latency budget

**Targets, not measurements.** `cmd/evaluate` reports p50 / p95 per path and stage, but no numbers are frozen yet. The table below is still the plan.

| Stage | Bypass | Fast | Full | Notes |
|---|---:|---:|---:|---|
| Load session + pre-router | 1 | 1 | 1 | SQLite row + last 10 turns |
| Retrieval shortlist | – | 10 | 10 | measured ≈ 0.03 ms |
| LLM route | – | 350 | 900 | timeouts are 1.5 s / 12 s, not 600 / 1200 ms |
| Executor + mock tools | 10 | 10 | 10 | synthetic backend, SQLite transaction per tool |
| Response | 0 | 0 | 0–350 | template; LLM only when needed |
| **Total to first byte** | **≈15** | **≈380** | **≈925–1275** | brief: 500 route / 1500 reply |

The two model calls are the whole story. What is known: full-route calls with `get_scenario` rounds took 8–11 s, and the fast path admits few requests (§5.4). The bypass path is the only one reliably inside the brief's marks. The hybrid latency gain is not demonstrated.

## 7. Fallback ladder — Implemented

Any model call that times out, errors or fails schema/catalog validation drops one rung. `trace.fallback_level` records the deepest rung reached.

| Level | Behaviour |
|---|---|
| L0 | The route chosen by the gate. Timeout 1.5 s fast, 12 s full. The HTTP client retries once on 429/5xx after 250 ms. Fast → full escalation stays at L0. |
| L1 | The full route failed: repeat it once with the same prompt on `OPENAI_FALLBACK_MODEL` (default: the fast model), 12 s. |
| L2 | Retrieval-only, **never executes**. If retrieval's top-1 leads by ≥ `L2Margin` 0.20, ask whether the request matches that scenario's first catalog example. The next turn goes through the LLM. On dev, the top-1 is right for 12 of the 13 utterances that clear this margin. |
| L3 | Hand off with reason `routing_failed` and context. |

A lost database or session lease aborts the turn with 503 instead of descending the ladder.

## 8. Offline harness (the outer loop) — Implemented, except the sweep

`cmd/evaluate` drives `Engine.Process` in-process with the real OpenAI model and an in-memory repository, one fresh session and synthetic backend per case (see `README.md` for commands).

1. **Single-turn accuracy** on `dev_utterances.json` (104), `-mode single`. Reports primary and exact-set accuracy, multi-intent recall, and breakdowns by language, path and type. It writes `predictions.json` and runs `evaluate.py`. Implemented.
2. **Multi-turn replay** of `dialogs_sample.json` (10 dialogs), `-mode dialogs`. Client turns go through `Engine.Process`, with per-turn and per-tag accuracy, the status sequence per dialog, and whether reference irreversible actions ran on the same turn. Implemented. `-mode probes` adds 17 jury-style probes (`cmd/evaluate/testdata/probes.json`): topic switch with return, mixed ru/kk, `not_this_if` boundaries, Kazakh.
3. **Latency** per stage, p50 / p95 / max, and accuracy per path. Implemented. The fast-vs-full comparison is available per run but is not recorded here.
4. **Calibration.** **Not implemented as one command.** The retrieval-side thresholds (`FastMinScore`, `FastMargin`, `L2Margin`) and the retrieval weights are calibrated by `TestRetrievalCalibration` and the held-out-example tests. τ_exec and τ_handoff are not swept. A safety check flags any irreversible action executed without a previous-turn preview of the same inputs and an explicit consent, so the "zero wrong irreversible executions" constraint is visible per run.

`-mode retrieval` measures shortlist recall offline, with no API calls.

## 9. Explainability

Everything the supervisor panel needs is in `trace`, per turn:
- transcript and path;
- the shortlist with scores;
- the chosen scenario and the model's reason, plus the alternatives;
- the uncertainty components and which threshold fired;
- the boundary rule involved;
- actions with mode and result;
- ms per stage;
- fallback level and response source.

The reason is the model's, the evidence is Go's. Operator tickets carry the same evidence, and `GET /v1/operator/handoffs/{session_id}` returns the recent turns with full traces.

**Partial:** the `chat/` trace HUD renders language, scenarios with confidence, reason, actions and latency. It does not yet render path, shortlist, uncertainty, boundary, fallback level or response source.

## 10. Safety and limits

- Irreversible actions require a preview and an explicit yes on the next turn, with unchanged inputs, verified by both the model (`is_continuation`) and the lexicon, or by the pre-router's two witnesses.
- All data is synthetic. No real service is called, and mock receipts carry `mock: true`.
- API keys stay on the server and never appear in traces. `backend/.env` is gitignored.
- Utterances, history, slot values and review feedback are treated as untrusted data in prompts.
- IINs, phones and e-mail local parts are masked in LLM answers, previews, template read-backs and the operator webhook.
- Not claimed:
  - measured STT/TTS latency, or meeting the brief's latency marks;
  - production insurance pricing;
  - routing accuracy (the harness exists, and results are not frozen here);
  - more than one router process per database file.

## 11. Scope for the remaining time

**In, and done:**
- the dataset is wired into the module (local `replace`), so the module builds;
- stages 5.2, 5.3, 5.4, the fast route, 5.6, template responses and the new trace fields;
- the fallback ladder;
- the harness in §8 (without the sweep);
- the web UI with mic → STT → `/v1/turns` → TTS and a trace panel (`chat/`, subset of the trace);
- `docker compose up --build` and `make run`/`make up`.

**Added:** durable SQLite store (instead of in-memory); operator takeover with reason-coded tickets, operator API and optional webhook; slot sanitization and answer masking.

**Still open:** the τ_exec / τ_handoff sweep; rendering the new trace fields in the chat HUD; passing `OPERATOR_*` and `OPENAI_FALLBACK_MODEL` through `docker-compose.yml`; dropping layer-1 `slots`.

**Out:** PostgreSQL persistence and leases (dropped for SQLite); new executor semantics; catalog editing endpoints; emotion detection; streaming partial routing; fine-tuning or custom models.

## 12. Critique → change → status

| Critique | Stage | Change | Status |
|---|---|---|---|
| Not hybrid | 5.3–5.5 | Retrieval shortlist, gate on `fast_path_eligible`, small-model fast route | Done. The fast path admits few requests. |
| Latency structurally bad | 5.2, 5.5, 5.9 | Bypass skips the model; smaller prompt; template responses drop the second call | Done. Full-route latency is still high (12 s timeout). |
| Confidence is invented | 5.6 | Uncertainty from observable signals; thresholds calibrated | Signals done. τ_exec / τ_handoff not yet calibrated. |
| Multi-turn never evaluated | §8 | Dialog replay harness | Done (`-mode dialogs`, `-mode probes`). |
| Layer boundary too thin | §3 | Layer 1 sends transcript only | Not done. `slots` still accepted. |
| Effort inverted | 5.8, §11 | Executor frozen, persistence parked, UI and one-command launch in | Done. SQLite replaced parked PostgreSQL at low cost. |
| Rules leaking into the prompt | 5.5 | Boundaries live in `not_this_if` data | Done, except the SC15 / SC38 hints. |
| No degraded mode | §7 | Fallback ladder, visible in the trace | Done. |
| Split decision ownership | 5.2, 5.8 | LLM proposes, Go verifies | Done. |
