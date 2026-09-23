# Voice Router — Design

Layer 2 of the HackAlem case 2 solution: the decision layer between speech recognition and business scenarios for the fictional Saqta Insurance contact center. This document is the target design; the code in `backend/` is the starting point and differs where the "Changes from current code" sections say so. A visual walkthrough of the per-turn loop is in `docs/agentic-loop.html` (open it locally in a browser).

Status: proposal, 2026-09-23. Dataset snapshot date is 2026-10-01.

## 1. Goal and what is scored

The brief scores routing quality, explainability and reproducibility, not STT/TTS quality. Latency is a bonus: 500 ms for the scenario choice, 1.5 s from end of speech to start of reply. Must-haves: voice in the browser, LLM at the point of the scenario decision (no encoder intent classifier), a trace panel after every utterance (scenario, reason, alternatives, time per stage), Russian and Kazakh including mixed speech, explicit confirmation before irreversible actions, handoff to an operator with context.

Optional items the design deliberately targets: hybrid architecture with a measurable latency gain, context retention and return to an interrupted topic, clarification instead of guessing, slot extraction from speech.

## 2. Principles

1. **The LLM chooses every scenario.** No other component ever maps an utterance to a scenario ID. Retrieval narrows the candidates; deterministic code only continues a scenario the model already chose.
2. **The LLM proposes, Go verifies and executes.** Model output is validated against the catalog and the session state before anything runs. Tools are Go functions; the model cannot name a tool, a storage key or a scenario outside the catalog.
3. **Every stage emits a trace row** with milliseconds, candidates, choice, reason, alternatives and the path taken. The supervisor panel renders the trace; it is not a separate feature.
4. **No dead ends.** Every model failure drops one rung down a fallback ladder. The caller always receives a stored answer within the turn budget.
5. **Measure before tuning.** Every threshold comes from an offline harness over the dataset, is frozen, and is written down.

## 3. System context

```
mic ──► Layer 1: STT + normalization ──► Layer 2: router (this repo) ──► Layer 3: TTS + web UI
                                              │
                                              └─► trace ──► supervisor panel
```

Layer 1 → layer 2: `POST /v1/turns` with `session_id`, `request_id`, `text` (normalized transcript), `language` (`ru` | `kk` | `mixed`), optional `reply_language`. **Change from current code:** the optional `slots` field is removed. The router owns slot extraction; two extractors with an override rule were two sources of truth.

Layer 2 → layer 3: `answer`, `language`, `status`, `active_scenario`, `pending_scenarios`, `trace`. Unchanged; `trace` gains fields listed in §5.10.

## 4. State

Per session, in memory, per-session lock (unrelated sessions run concurrently):

- `language`, `identity` (phone / IIN once verified)
- `active` frame: `scenario_id`, `slots`, `next_action`, `results`, `pending` preview
- `stack`: suspended frames (topic switch), each with its own slots and action position
- `queue`: intents from a multi-intent turn not yet started
- `turns`: last inputs and outputs, 100 max; the model sees the last 10
- `low_confidence` counter

Limits: 1000 sessions, 1 h idle TTL. Nothing survives a restart. Durable persistence (the parked `repository.go` PostgreSQL design) is out of scope for the hackathon.

## 5. The turn loop

One utterance = one pass. Stages in order; three paths through them converge on the same executor, response and trace.

### 5.0 Transcript arrives
Validate, lock the session, apply idempotency: the same `request_id` with identical input replays the stored output; with different input returns 409.

### 5.1 Load session
Read the state above. Determine the reply language: explicit `reply_language` wins, then a non-mixed `language`, otherwise the model decides from context.

### 5.2 Deterministic pre-router (Go, ~1 ms)
Handles exactly two situations, both continuations of a scenario the LLM chose earlier:

- The active frame awaits a slot and the utterance parses cleanly as that slot type per `slots.json` (phone `^\+7\d{10}$`, 12-digit IIN, date, enum value). Fill the slot, go to 5.8.
- A preview is pending and the utterance is an explicit yes/no from a fixed ru/kk lexicon. Resolve, go to 5.8.

Anything else, including "да, но телефон другой", goes on to 5.3. **Ownership rule:** on the LLM path an irreversible action executes only if the model returned `is_continuation=true` *and* the text contains an explicit affirmative. The regex alone never executes anything; the model alone never executes anything.

**Change from current code:** today every turn, including a bare phone number, pays a full LLM round-trip.

### 5.3 Retrieval shortlist (Go, ~10 ms)
Score the utterance against each scenario's `examples` (ru + kk), `description` and `not_this_if` conditions. Embeddings are computed once at startup (or BM25 if we want zero external calls). Output: top-8 scenario IDs with scores, plus the active/suspended/queued scenario IDs, plus the three system intents.

Purpose: shrink the routing prompt from all 43 scenarios (~90 KB) to about ten, and provide a second, independent signal for the uncertainty score. Retrieval never assigns a scenario; the trace shows the shortlist next to the model's choice.

### 5.4 Gate (Go)
Fast path when all hold, full path otherwise:

- top-1 retrieval score leads top-2 by ≥ `τ_fast`;
- top-1 has `fast_path_eligible: true` in `scenarios.json` (nine scenarios: SC18, SC23, SC24, SC26, SC31, SC33, SC34, SC36, SC37);
- no active frame awaits input or holds a preview, and the stack is empty.

**Change from current code:** the `fast_path_eligible` flag shipped with the dataset is not read today.

### 5.5 LLM route
**Fast route** (~350 ms): small, low-latency model. Prompt: utterance, three candidate scenarios (name, description, `not_this_if`), system intents, dataset date. No history. Output: one scenario, one-line reason, reply language, slots.

**Full route** (~900 ms): main model. Prompt: shortlist with examples and boundaries, slot definitions for those scenarios only, compact state (active, suspended, queued, identity), last 10 turns. Output: scenarios in spoken order, alternatives, reason, slots, `is_continuation`, `needs_handoff`. Strict JSON schema, validated again in Go, as today.

Both models implement the existing `Model` interface; which one runs is configuration.

**Changes from current code:** prompt shrinks 5–8×. Per-scenario exceptions currently hard-coded in the system prompt ("small approved payout dispute is SC19, not SC17", …) move into the dataset's `not_this_if`, where the model reads them for shortlisted scenarios. `dev_utterances.json` is never sent to the model (unchanged).

### 5.6 Uncertainty score (Go)
```
u = w1·(1 − margin_retrieval)
  + w2·[retrieval_top1 ≠ llm_top1]
  + w3·(1 − llm_confidence)                       # one vote, not the verdict
  + w4·[a not_this_if condition of llm_top1 retrieved strongly]
```
`τ_exec` and `τ_handoff` are set by the harness in §7, then frozen and recorded in the README. All components go into the trace.

**Change from current code:** replaces the model's self-reported confidence with 0.75 / 0.45 cut-offs that were chosen rather than measured.

### 5.7 Policy (Go)
- **Confident** (`u < τ_exec`): execute. Urgent scenarios (SC11, SC15, SC38) first; remaining intents of a multi-intent turn are queued and the answer offers to return to them.
- **Uncertain**: clarification with two named options from the model's alternatives ∪ retrieval top-2. Skips the executor.
- **Hand off** with a context packet (transcript, chosen + alternatives, slots, queue) when: two consecutive uncertain turns (`u > τ_handoff`), `needs_handoff` for a scenario whose `handoff.when` applies, SC37 requested, or the fallback ladder bottomed out. Skips the executor.

### 5.8 Executor (Go, ~10 ms on mocks)
Identify if `requires_identification` → collect required slots in catalog order → run `actions` in catalog order. An `irreversible` action produces a preview and a pending frame; the next turn's verified yes runs it. Corrections invalidate the preview and recompute read-only work. Topic switches push the active frame to the stack; returning pops it.

**Frozen.** No new frame semantics, no persistence, no new mock rules for the rest of the hackathon.

### 5.9 Response
Templated from the dataset for standard states: slot question from `slots.json → prompt`, scenario `responses.opening` / `closing` with placeholders filled from tool results, a fixed confirmation preview. Deterministic, 0 ms, cannot claim something that did not happen; TTS can start immediately.

The LLM `Respond` call runs only for clarification wording, handoff messages, tool-error explanations and free-form knowledge-base answers.

**Change from current code:** removes the second serial LLM call from most turns.

### 5.10 Trace + persist
Per turn: ms per stage; `path` (`bypass` | `fast` | `full`); shortlist with scores; chosen scenario(s) with reason; alternatives; uncertainty components; the `not_this_if` boundary involved, if any; `fallback_level`; actions with `preview` / `execute` mode and results; `error` without secrets. Store under `request_id`, unlock, return.

## 6. Latency budget

Targets for the demo network, to be replaced by measurements from §7.

| Stage | Bypass | Fast | Full | Notes |
|---|---:|---:|---:|---|
| Load session + pre-router | 1 | 1 | 1 | in-process map |
| Retrieval shortlist | – | 10 | 10 | embeddings precomputed |
| LLM route | – | 350 | 900 | timeouts 600 / 1200 ms, then fallback |
| Executor + mock tools | 10 | 10 | 10 | synthetic backend |
| Response | 0 | 0 | 0–350 | template; LLM only when needed |
| **Total to first byte** | **≈15** | **≈380** | **≈925–1275** | brief: 500 route / 1500 reply |

The two model calls are the whole story; all Go stages together stay under 30 ms.

## 7. Fallback ladder

Any model call that times out, errors or fails schema validation drops one rung. The rung is recorded as `fallback_level`.

| Level | Behaviour |
|---|---|
| L0 | Primary model per the gate. Timeout 600 ms fast, 1200 ms full. |
| L1 | Secondary model or provider, same prompt, one attempt. |
| L2 | Retrieval-only, **never execute**: if the margin is high, ask a clarification naming the top candidate; the next turn confirms through the LLM. |
| L3 | Hand off with context. Today's behaviour on any error, now the last resort. |

## 8. Offline harness (the outer loop)

Four tunables: `τ_fast`, `τ_exec`, `τ_handoff`, shortlist size. All come from one command that runs offline against the dataset and prints:

1. **Single-turn accuracy** on `dev_utterances.json` (104), per path, with confusion pairs (SC17↔SC19, SC11↔SC12↔SC13). `evaluate.py` for the official breakdown.
2. **Multi-turn replay** of `dialogs_sample.json` (10 dialogs tagged scenario_switch, topic_switch, context_return, multi_intent, mixed_language, clarification, handoff, irreversible_action, language_switch): client turns through `Engine.Process`, compare chosen scenario per turn and the resulting status. This is the only test of context retention and confirmation flows.
3. **Latency histograms** per path and stage, p50 / p95. The fast-path vs. full-path p50 difference is the hybrid gain shown on the slide.
4. **Calibration**: sweep each τ over 1 + 2, maximise accuracy subject to zero wrong executions of an irreversible action. Freeze, record, re-run before the demo.

## 9. Explainability

The supervisor panel shows, per turn: transcript; path; the shortlist with scores; the chosen scenario and the model's reason; alternatives; the uncertainty components and which threshold fired; the boundary rule involved; actions with mode and result; ms per stage; fallback level. The reason is the model's, the evidence is Go's.

## 10. Safety and limits

- Irreversible actions require a preview and an explicit yes on the next turn, verified by both the model and the lexicon.
- All data is synthetic. No real service is called; mock receipts carry `mock: true`.
- API keys stay on the server and never appear in traces.
- Utterances, history and slot values are treated as untrusted data in prompts.
- Not claimed: durable state across restarts, measured STT/TTS latency, production insurance pricing, routing accuracy until the harness has run.

## 11. Scope for the remaining time

**In:** move the dataset package under `backend/` so the module builds; stages 5.2, 5.3, 5.4, fast route, 5.6, template responses, new trace fields; fallback ladder; the harness in §8 as one command; web UI with mic → STT → `/v1/turns` → TTS and the trace panel; `docker compose up` or a single start script.

**Out:** PostgreSQL persistence and leases; new executor semantics; catalog editing endpoints; emotion detection; streaming partial routing; fine-tuning or custom models.

## 12. Changes from current code, in one list

| Critique | Stage | Change |
|---|---|---|
| Not hybrid | 5.3–5.5 | Retrieval shortlist, gate on `fast_path_eligible`, small-model fast route |
| Latency structurally bad | 5.2, 5.5, 5.9 | Bypass skips the model; prompt 5–8× smaller; template responses drop the second call |
| Confidence is invented | 5.6 | Uncertainty from observable signals; thresholds calibrated |
| Multi-turn never evaluated | §8 | Dialog replay harness |
| Layer boundary too thin | §3 | Layer 1 sends transcript only |
| Effort inverted | 5.8, §11 | Executor frozen, Postgres parked, UI and one-command launch in |
| Rules leaking into the prompt | 5.5 | Boundaries live in `not_this_if` data |
| No degraded mode | §7 | Fallback ladder, visible in the trace |
| Split decision ownership | 5.2, 5.8 | LLM proposes, Go verifies |
