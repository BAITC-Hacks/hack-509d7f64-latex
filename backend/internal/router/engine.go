package router

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var ErrInvalidInput = errors.New("invalid input")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

type Engine struct {
	Catalog   *Catalog
	Store     Repository
	Model     Model
	Retriever *Retriever
	Policy    Policy
	MaxSteps  int
	Timeout   time.Duration
	// Operators configures human takeover notifications (OPERATOR_WEBHOOK_URL).
	Operators OperatorHub
}

// Policy holds the turn loop's tunables. Defaults are starting points that
// cmd/evaluate calibrates against the dataset.
type Policy struct {
	ShortlistSize  int           // scenarios the full route reads in detail
	FastCandidates int           // scenarios the fast route chooses between
	FastMargin     float64       // τ_fast: retrieval top-1 lead needed for the fast route
	FastMinScore   float64       // minimum top-1 retrieval score for the fast route
	Execute        float64       // τ_exec: uncertainty at or below this executes
	Handoff        float64       // τ_handoff: uncertainty above this counts toward handoff
	L2Margin       float64       // retrieval lead needed to clarify when every model failed
	FastTimeout    time.Duration // L0 timeout on the fast route
	FullTimeout    time.Duration // L0/L1 timeout on the full route
	FallbackModel  string        // L1 model; empty repeats the default model
}

func DefaultPolicy() Policy {
	return Policy{ShortlistSize: 8, FastCandidates: 3, FastMargin: .15, FastMinScore: .3, Execute: .25, Handoff: .55, L2Margin: .2, FastTimeout: 1500 * time.Millisecond, FullTimeout: 6 * time.Second}
}

func NewEngine(c *Catalog, m Model, repo Repository) *Engine {
	return &Engine{Catalog: c, Store: repo, Model: m, Retriever: NewRetriever(c), Policy: DefaultPolicy(), MaxSteps: 24, Timeout: 60 * time.Second, Operators: OperatorHub{WebhookURL: os.Getenv("OPERATOR_WEBHOOK_URL")}}
}

// workflowIDs lists the scenarios already in play, which every shortlist keeps.
func workflowIDs(s *Session) []string {
	ids := []string{}
	if s.Active != nil {
		ids = append(ids, s.Active.ScenarioID)
	}
	for _, f := range append(append([]*Frame{}, s.Stack...), s.Queue...) {
		ids = append(ids, f.ScenarioID)
	}
	return ids
}

// fastEligible is the gate: no workflow in progress, and retrieval clearly
// favours one scenario the dataset marks fast_path_eligible.
func (e *Engine) fastEligible(s *Session, shortlist []ScoredScenario) bool {
	if len(shortlist) == 0 || s.Active != nil || len(s.Stack) > 0 || len(s.Queue) > 0 {
		return false
	}
	top := shortlist[0]
	sc, ok := e.Catalog.Scenarios[top.ScenarioID]
	if !ok || !sc.FastPath || top.Score < e.Policy.FastMinScore {
		return false
	}
	margin := top.Score
	if len(shortlist) > 1 {
		margin -= shortlist[1].Score
	}
	return margin >= e.Policy.FastMargin
}

// escalate sends a fast-route answer to the full route unless it is a single,
// confident choice of the retrieval favourite.
func (e *Engine) escalate(d Decision, shortlist []ScoredScenario) bool {
	if len(d.Scenarios) != 1 || len(shortlist) == 0 {
		return true
	}
	primary := d.Scenarios[0]
	return primary.ScenarioID != shortlist[0].ScenarioID || primary.Confidence < 1-e.Policy.Execute
}

// retrievalClarification is fallback rung L2: when every model call failed,
// ask whether the request matches retrieval's clear favourite. It names the
// scenario through one of its own catalog examples and never executes.
func (e *Engine) retrievalClarification(lang string, shortlist []ScoredScenario) (string, bool) {
	if len(shortlist) == 0 {
		return "", false
	}
	margin := shortlist[0].Score
	if len(shortlist) > 1 {
		margin -= shortlist[1].Score
	}
	sc, ok := e.Catalog.Scenarios[shortlist[0].ScenarioID]
	if !ok || margin < e.Policy.L2Margin || len(sc.Examples[lang]) == 0 {
		return "", false
	}
	example := sc.Examples[lang][0]
	return local(lang, "Правильно понимаю, ваш вопрос такой: «"+example+"»? Ответьте «да» или уточните.", "Дұрыс түсіндім бе, сұрағыңыз мынадай ма: «"+example+"»? «Иә» деңіз немесе нақтылаңыз."), true
}
func (e *Engine) ValidateInput(in Input) error {
	if in.Operator != nil {
		return fmt.Errorf("%w: operator actions use the operator API", ErrInvalidInput)
	}
	if !idPattern.MatchString(in.SessionID) || !idPattern.MatchString(in.RequestID) {
		return fmt.Errorf("%w: invalid session_id or request_id", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Text) == "" || len(in.Text) > 8000 {
		return fmt.Errorf("%w: text must contain 1-8000 bytes", ErrInvalidInput)
	}
	if !slices.Contains([]string{"ru", "kk", "mixed"}, in.Language) {
		return fmt.Errorf("%w: invalid language", ErrInvalidInput)
	}
	if in.ReplyLanguage != "" && in.ReplyLanguage != "ru" && in.ReplyLanguage != "kk" {
		return fmt.Errorf("%w: invalid reply_language", ErrInvalidInput)
	}
	if !slices.Contains([]string{"", "auto", "user", "operator"}, in.ReviewMode) {
		return fmt.Errorf("%w: invalid review_mode", ErrInvalidInput)
	}
	if err := e.Catalog.ValidateSlots(in.Slots); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return nil
}
func (e *Engine) validateDecision(d Decision) error {
	if len(d.Scenarios) == 0 || len(d.Scenarios) > 8 {
		return errors.New("invalid scenario count")
	}
	if d.Language != "ru" && d.Language != "kk" {
		return errors.New("invalid model language")
	}
	seen := map[string]bool{}
	for _, c := range d.Scenarios {
		if !e.Catalog.ValidID(c.ScenarioID) || seen[c.ScenarioID] {
			return errors.New("unknown or duplicate scenario")
		}
		seen[c.ScenarioID] = true
	}
	for _, c := range append(append([]Candidate{}, d.Scenarios...), d.Alternatives...) {
		if !e.Catalog.ValidID(c.ScenarioID) || math.IsNaN(c.Confidence) || c.Confidence < 0 || c.Confidence > 1 {
			return errors.New("invalid candidate")
		}
	}
	return e.Catalog.ValidateSlots(d.Slots)
}
func (e *Engine) Process(ctx context.Context, in Input) (Output, error) {
	if err := e.ValidateInput(in); err != nil {
		return Output{}, err
	}
	if in.ReviewMode == "" {
		in.ReviewMode = "auto"
	}
	lease, err := e.Store.Lock(ctx, in.SessionID)
	if err != nil {
		return Output{}, err
	}
	defer lease.Release()
	s, err := lease.Load(ctx)
	if err != nil {
		return Output{}, err
	}
	// A retry of an earlier turn must not become feedback for a newer review.
	if old, findErr := lease.GetRun(ctx, in.RequestID); findErr == nil {
		if !equalJSON(old.Input, in) {
			return Output{}, ErrConflict
		}
		if old.Phase == "finished" || old.Phase == "awaiting_intent_confirmation" {
			return clone(old.Output), nil
		}
	} else if !errors.Is(findErr, ErrNotFound) {
		return Output{}, findErr
	}
	if link, ok := s.ReviewReplies[in.RequestID]; ok {
		if link.InputHash != inputDigest(in) {
			return Output{}, ErrConflict
		}
		previous, err := lease.GetRun(ctx, link.TurnRequestID)
		if err != nil {
			return Output{}, err
		}
		for _, receipt := range previous.Reviews {
			if receipt.Input.RequestID == in.RequestID {
				return e.reviewLocked(ctx, lease, &s, &previous, receipt.Input, "user")
			}
		}
		return Output{}, ErrConflict
	}
	if s.PendingTurnID != "" && s.PendingTurnID != in.RequestID {
		waiting, err := lease.GetRun(ctx, s.PendingTurnID)
		if err != nil {
			return Output{}, err
		}
		if waiting.Review == nil || waiting.Review.Target != "user" {
			return Output{}, ErrBusy
		}
		choice := "rejected"
		if explicitYes(in.Text) {
			choice = "approved"
		}
		feedback := ReviewInput{RequestID: in.RequestID, TurnRequestID: waiting.Input.RequestID, ProposalID: waiting.Review.ProposalID, Revision: waiting.Review.Revision, Decision: choice, Feedback: in.Text}
		if s.ReviewReplies == nil {
			s.ReviewReplies = map[string]UserReviewLink{}
		}
		s.ReviewReplies[in.RequestID] = UserReviewLink{TurnRequestID: waiting.Input.RequestID, InputHash: inputDigest(in)}
		return e.reviewLocked(ctx, lease, &s, &waiting, feedback, "user")
	}
	r, resumed, err := lease.Begin(ctx, in)
	if err != nil {
		return Output{}, err
	}
	if resumed && (r.Phase == "finished" || r.Phase == "awaiting_intent_confirmation") {
		return clone(r.Output), nil
	}
	if !resumed || r.Output.Trace.Turn == 0 {
		if s.Active != nil && s.Active.Pending != nil && len(s.Turns) > 0 {
			last := s.Turns[len(s.Turns)-1].Output
			if last == nil || last.Status != "awaiting_confirmation" || last.ActiveScenario != s.Active.ScenarioID {
				s.Active.Pending = nil
			}
		}
		s.TurnCount++
		if in.ReplyLanguage != "" {
			s.Language = in.ReplyLanguage
		} else if in.Language != "mixed" {
			s.Language = in.Language
		}
		r.Output = Output{SessionID: s.ID, RequestID: in.RequestID, Language: s.Language, PendingScenarios: []string{}, Trace: Trace{Turn: s.TurnCount, Transcript: in.Text, Language: in.Language, Actions: []ActionCall{}, LatencyMS: map[string]int64{}}}
		r.Output.OperatorMessages = takeOperatorMessages(&s)
		if s.Operator == nil {
			if note := takeHandback(&s); note != nil {
				r.RoutingContext = append(r.RoutingContext, note)
			}
		}
	}
	if err := e.recordNotifications(lease, &s, &r); err != nil {
		return Output{}, err
	}
	// While a human owns the conversation the bot stays silent: no routing,
	// no tools, no model call. A run that itself opened the ticket (and was
	// interrupted afterwards) is past "received" and finishes normally.
	if r.Phase == "received" && s.Operator.Open() {
		return e.hold(ctx, lease, &s, &r)
	}
	return e.run(ctx, lease, &s, &r)
}
func inputDigest(in Input) string {
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (e *Engine) Review(ctx context.Context, sessionID string, in ReviewInput, actor string) (Output, error) {
	if !idPattern.MatchString(sessionID) || !idPattern.MatchString(in.RequestID) || !idPattern.MatchString(in.TurnRequestID) || in.ProposalID == "" || in.Revision < 1 || !slices.Contains([]string{"approved", "rejected"}, in.Decision) || len(in.Feedback) > 8000 {
		return Output{}, fmt.Errorf("%w: invalid review", ErrInvalidInput)
	}
	lease, err := e.Store.Lock(ctx, sessionID)
	if err != nil {
		return Output{}, err
	}
	defer lease.Release()
	s, err := lease.Load(ctx)
	if err != nil {
		return Output{}, err
	}
	r, err := lease.GetRun(ctx, in.TurnRequestID)
	if err != nil {
		return Output{}, err
	}
	return e.reviewLocked(ctx, lease, &s, &r, in, actor)
}
func (e *Engine) reviewLocked(ctx context.Context, lease Lease, s *Session, r *Run, in ReviewInput, actor string) (Output, error) {
	for _, receipt := range r.Reviews {
		if receipt.Input.RequestID == in.RequestID {
			if !equalJSON(receipt.Input, in) || receipt.Actor != actor {
				return Output{}, ErrConflict
			}
			if receipt.Output != nil {
				return clone(*receipt.Output), nil
			}
			return e.run(ctx, lease, s, r)
		}
	}
	if r.Phase != "awaiting_intent_confirmation" || r.Review == nil || r.Review.Status != "pending" || r.Review.ProposalID != in.ProposalID || r.Review.Revision != in.Revision || r.Review.Target != actor {
		return Output{}, ErrConflict
	}
	r.Reviews = append(r.Reviews, ReviewReceipt{Input: in, Actor: actor})
	r.Review.Status = in.Decision
	r.Output.Review = clone(r.Review)
	s.PendingTurnID = ""
	r.RoutingContext = append(r.RoutingContext, Values{"kind": "review_feedback", "proposal_id": in.ProposalID, "decision": in.Decision, "feedback": in.Feedback, "scenarios": r.Review.Decision.Scenarios})
	if in.Decision == "approved" {
		r.Phase = "accepted"
	} else {
		for _, candidate := range r.Review.Decision.Scenarios {
			if !slices.Contains(r.Rejected, candidate.ScenarioID) {
				r.Rejected = append(r.Rejected, candidate.ScenarioID)
			}
		}
		r.Phase = "retrieving"
	}
	if err := lease.Save(ctx, s, r, &Event{Kind: "intent_reviewed", Data: Values{"review": in, "actor": actor}}); err != nil {
		return Output{}, err
	}
	return e.run(ctx, lease, s, r)
}

type work struct {
	e     *Engine
	l     Lease
	s     *Session
	r     *Run
	ctx   context.Context
	clock time.Time
}

func (e *Engine) run(ctx context.Context, l Lease, s *Session, r *Run) (Output, error) {
	if r.InFlightAt != nil {
		r.ActiveMS += time.Since(*r.InFlightAt).Milliseconds()
		r.InFlightAt = nil
	}
	remaining := e.Timeout - time.Duration(r.ActiveMS)*time.Millisecond
	if remaining < 0 {
		remaining = 0
	}
	ctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	stop := context.AfterFunc(l.Context(), cancel)
	defer stop()
	w := &work{e: e, l: l, s: s, r: r, ctx: ctx, clock: time.Now()}
	if err := w.save("resumed", Values{"phase": r.Phase}); err != nil {
		return Output{}, err
	}
	for {
		if r.Phase == "finished" || r.Phase == "awaiting_intent_confirmation" {
			return clone(r.Output), nil
		}
		if (ctx.Err() != nil || r.Steps >= e.MaxSteps) && r.Phase != "handoff" {
			r.Output.Trace.Error = "processing budget exceeded"
			r.Output.Trace.Handoff = &HandoffInfo{Reason: ReasonProcessingBudget, Detail: fmt.Sprintf("%d of %d steps, %d ms active", r.Steps, e.MaxSteps, r.ActiveMS), Status: "requested"}
			r.Phase = "handoff"
			r.Fallback = local(s.Language, "Для завершения запроса подключаю оператора.", "Сұрауды аяқтау үшін операторды қосамын.")
		}
		switch r.Phase {
		case "received":
			r.Phase = "identifying"
			if err := w.save("context_loaded", nil); err != nil {
				return Output{}, err
			}
		case "identifying":
			if err := w.identify(); err != nil {
				return Output{}, err
			}
		case "validating":
			if err := w.propose(); err != nil {
				return Output{}, err
			}
		case "proposed":
			if err := w.decide(); err != nil {
				return Output{}, err
			}
		case "retrieving":
			if err := w.retrieve(); err != nil {
				return Output{}, err
			}
		case "accepted":
			if err := w.prepare(); err != nil {
				return Output{}, err
			}
		case "executing":
			if err := w.execute(); err != nil {
				return Output{}, err
			}
		case "tool_error":
			if err := w.toolError(); err != nil {
				return Output{}, err
			}
		case "generating_answer":
			if err := w.generate(); err != nil {
				return Output{}, err
			}
		case "handoff":
			if err := w.transfer(); err != nil {
				return Output{}, err
			}
		default:
			return Output{}, fmt.Errorf("%w: unknown execution phase", ErrConflict)
		}
	}
}
func (w *work) account() {
	w.r.ActiveMS += time.Since(w.clock).Milliseconds()
	w.clock = time.Now()
	if w.r.InFlightAt != nil {
		now := w.clock
		w.r.InFlightAt = &now
	}
	w.r.Output.Trace.LatencyMS["total"] = w.r.ActiveMS
}
func (w *work) dbContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(w.l.Context(), 5*time.Second)
}
func (w *work) save(kind string, data Values) error {
	w.account()
	ctx, cancel := w.dbContext()
	defer cancel()
	return w.l.Save(ctx, w.s, w.r, &Event{Kind: kind, Data: data})
}
func (w *work) beforeModel(kind string) error {
	w.r.Steps++
	now := time.Now()
	w.r.InFlightAt = &now
	return w.save(kind, nil)
}
func (w *work) afterModel() { w.r.InFlightAt = nil; w.account() }
func (w *work) finish(answer, status string) error {
	w.settle(answer, status)
	ctx, cancel := w.dbContext()
	defer cancel()
	return w.l.Save(ctx, w.s, w.r, &Event{Kind: "turn_output", Data: Values{"status": status}})
}

// settle fills the final output without saving; a tool transaction or finish
// commits it.
func (w *work) settle(answer, status string) {
	w.r.Phase = "finished"
	if status == "awaiting_intent_confirmation" {
		w.r.Phase = status
		w.s.PendingTurnID = w.r.Input.RequestID
	} else if w.s.PendingTurnID == w.r.Input.RequestID {
		w.s.PendingTurnID = ""
	}
	o := &w.r.Output
	o.Answer = answer
	o.Status = status
	if o.Trace.ResponseSource == "" {
		o.Trace.ResponseSource = "template"
	}
	o.Language = w.s.Language
	o.ActiveScenario = ""
	if w.s.Active != nil {
		o.ActiveScenario = w.s.Active.ScenarioID
	}
	o.PendingScenarios = pendingIDs(w.s)
	o.Review = clone(w.r.Review)
	if w.s.Operator.Open() {
		o.Handoff = w.s.Operator.info()
	}
	w.account()
	for i := range w.r.Reviews {
		if w.r.Reviews[i].Output == nil {
			w.r.Reviews[i].Output = clone(o)
		}
	}
}
func (w *work) identify() error {
	if w.r.Attempts >= 3 {
		return w.finish(local(w.s.Language, "Уточните, пожалуйста, какой вопрос нужно решить?", "Қандай мәселені шешу керегін нақтылаңызшы?"), "clarification")
	}
	w.r.Attempts++
	tr := &w.r.Output.Trace
	fresh := w.r.Attempts == 1 && len(w.r.RoutingContext) == 0
	// Stage 02: a continuation of the scenario the model chose on an earlier
	// turn, fully consumed by the expected slot parser or yes/no lexicon.
	if fresh {
		start := time.Now()
		d, ok := w.e.preRoute(w.s, w.r.Input)
		tr.LatencyMS["prerouter"] += time.Since(start).Milliseconds()
		if ok {
			tr.Path = "bypass"
			w.r.Decision = &d
			w.r.Phase = "validating"
			return w.save("routing_bypass", Values{"decision": d})
		}
	}
	// Stage 03: retrieval shortlist. It narrows what the model reads in
	// detail and provides an independent signal; it never picks a scenario.
	start := time.Now()
	shortlist := w.e.Retriever.Shortlist(w.r.Input.Text, w.e.Policy.ShortlistSize, workflowIDs(w.s)...)
	tr.LatencyMS["retrieval"] += time.Since(start).Milliseconds()
	tr.Shortlist = shortlist
	// Stage 04: gate between the fast and the full route.
	fast := fresh && w.e.fastEligible(w.s, shortlist)
	if err := w.beforeModel("routing_started"); err != nil {
		return err
	}
	state := modelState(*w.s)
	state.RoutingContext = clone(w.r.RoutingContext)
	ctx := withRouteRecorder(w.ctx, func(kind string, data Values) error {
		w.r.Steps++
		w.r.RoutingContext = append(w.r.RoutingContext, Values{"kind": kind, "data": data})
		if kind == "scenario_retrieved" {
			result := asMap(data["result"])
			w.r.Output.Trace.Actions = append(w.r.Output.Trace.Actions, ActionCall{Name: "get_scenario", Mode: "execute", Inputs: Values{"scenario_id": result["scenario_id"]}, Result: clone(result)})
		}
		if kind == "model_proposal" {
			data, err := json.Marshal(data["decision"])
			if err != nil {
				return err
			}
			var decision Decision
			if err := json.Unmarshal(data, &decision); err != nil {
				return err
			}
			w.r.Decision = &decision
			w.r.Phase = "validating"
			w.r.InFlightAt = nil
		}
		if w.r.Steps > w.e.MaxSteps {
			return errors.New("routing step limit")
		}
		return w.save(kind, data)
	})
	d, err := w.route(ctx, state, shortlist, fast)
	w.afterModel()
	if err != nil {
		if hard := w.hardError(err); hard != nil {
			return hard
		}
		tr.Error = err.Error()
		// L2: retrieval alone may ask about its top candidate; it never executes.
		if question, ok := w.e.retrievalClarification(w.s.Language, shortlist); ok {
			tr.FallbackLevel = 2
			return w.finish(question, "clarification")
		}
		// L3: hand off with context.
		tr.FallbackLevel = 3
		return w.handoff(ReasonRoutingFailed, err.Error(), "", "routing_failed", Values{"error": err.Error()})
	}
	w.r.Decision = &d
	w.r.Phase = "validating"
	return w.save("routing_received", nil)
}

// hardError reports failures that must abort the turn instead of falling back:
// lost persistence or a lost session lease.
func (w *work) hardError(err error) error {
	if errors.Is(err, ErrDatabase) || errors.Is(err, ErrConflict) {
		return err
	}
	if w.l.Context().Err() != nil {
		return fmt.Errorf("%w: lease lost", ErrDatabase)
	}
	return nil
}

// route runs rungs L0 and L1 of the fallback ladder. L0 is the route chosen by
// the gate; a fast route escalates to the full route when its answer is not
// clearly the retrieval favourite. L1 repeats the full route on the fallback
// model. The caller handles L2 (retrieval-only clarification) and L3 (handoff).
func (w *work) route(ctx context.Context, state Session, shortlist []ScoredScenario, fast bool) (Decision, error) {
	p, tr := w.e.Policy, &w.r.Output.Trace
	candidates := make([]string, 0, len(shortlist))
	for _, c := range shortlist {
		candidates = append(candidates, c.ScenarioID)
	}
	call := func(opts RouteOptions, timeout time.Duration) (Decision, error) {
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		d, err := w.e.Model.Route(c, w.r.Input, state, opts)
		tr.LatencyMS["router"] += time.Since(start).Milliseconds()
		if err == nil {
			err = w.e.validateDecision(d)
		}
		return d, err
	}
	tr.Path = "full"
	if fast {
		tr.Path = "fast"
		few := candidates[:min(p.FastCandidates, len(candidates))]
		d, err := call(RouteOptions{Candidates: few, Fast: true}, p.FastTimeout)
		if err == nil && !w.e.escalate(d, shortlist) {
			return d, nil
		}
		if err != nil {
			if hard := w.hardError(err); hard != nil {
				return d, hard
			}
			tr.Error = "fast route: " + err.Error()
		}
		tr.Path = "fast>full"
	}
	d, err := call(RouteOptions{Candidates: candidates}, p.FullTimeout)
	if err == nil || w.hardError(err) != nil || w.ctx.Err() != nil {
		return d, err
	}
	tr.Error = err.Error()
	tr.FallbackLevel = 1
	return call(RouteOptions{Candidates: candidates, Model: p.FallbackModel}, p.FullTimeout)
}

// A saved model result resumes here, without another provider call or proposal
// attempt, even if the worker stopped immediately after receiving the result.
func (w *work) propose() error {
	if w.r.Decision == nil {
		return fmt.Errorf("%w: missing intent proposal", ErrConflict)
	}
	d := clone(*w.r.Decision)
	if err := w.e.validateDecision(d); err != nil {
		w.r.Output.Trace.Error = err.Error()
		return w.handoff(ReasonInvalidDecision, err.Error(), "", "routing_failed", Values{"error": err.Error()})
	}
	if w.r.Input.ReplyLanguage != "" {
		d.Language = w.r.Input.ReplyLanguage
	} else if w.r.Input.Language != "mixed" {
		d.Language = w.r.Input.Language
	}
	w.s.Language = d.Language
	if d.Slots == nil {
		d.Slots = Values{}
	}
	if len(w.r.Rejected) > 0 {
		// Explicit corrections supplied during review are newer than layer 1's
		// original slots. Both sources have passed catalog validation.
		slots := Values{}
		merge(slots, w.r.Input.Slots)
		merge(slots, d.Slots)
		d.Slots = slots
	} else {
		merge(d.Slots, w.r.Input.Slots)
	}
	sort.SliceStable(d.Scenarios, func(i, j int) bool {
		return w.e.Catalog.Scenarios[d.Scenarios[i].ScenarioID].Priority == "urgent" && w.e.Catalog.Scenarios[d.Scenarios[j].ScenarioID].Priority != "urgent"
	})
	w.r.Decision = &d
	w.r.Output.Trace.Decision = clone(d)
	w.r.Output.Trace.Proposals = append(w.r.Output.Trace.Proposals, clone(d))
	w.r.Phase = "proposed"
	return w.save("intent_proposed", Values{"decision": d})
}
func (w *work) decide() error {
	d := w.r.Decision
	primary := d.Scenarios[0]
	u := w.e.assess(*d, w.s, w.r.Output.Trace.Shortlist, w.r.Output.Trace.Path)
	w.r.Output.Trace.Uncertainty = &u
	if u.Verdict != "execute" || primary.ScenarioID == "SYS_UNCLEAR" {
		if !w.r.LowCounted {
			if u.Verdict == "handoff" {
				w.s.LowConfidence++
			} else {
				w.s.LowConfidence = 0
			}
			w.r.LowCounted = true
		}
		if w.s.LowConfidence >= 2 {
			detail := fmt.Sprintf("uncertainty %.2f above %.2f on %d consecutive turns", u.Score, w.e.Policy.Handoff, w.s.LowConfidence)
			return w.handoff(ReasonLowConfidence, detail, "", "low_confidence_handoff", Values{"uncertainty": u})
		}
		options := []string{}
		for _, c := range append(append([]Candidate{}, d.Scenarios...), d.Alternatives...) {
			if sc, ok := w.e.Catalog.Scenarios[c.ScenarioID]; ok && !slices.Contains(options, sc.Name) {
				options = append(options, sc.Name)
				if len(options) == 2 {
					break
				}
			}
		}
		return w.queueAnswer(Values{"purpose": "clarify", "options": options, "utterance": w.r.Input.Text}, local(w.s.Language, "Уточните, пожалуйста, что нужно сделать со страховкой?", "Сақтандыру бойынша не істеу керегін нақтылаңызшы?"), "clarification", false)
	}
	w.s.LowConfidence = 0
	if response, ok := w.e.Catalog.System[primary.ScenarioID]; ok {
		if primary.ScenarioID == "SYS_GOODBYE" {
			w.s.Active = nil
			w.s.Stack = nil
			w.s.Queue = nil
		}
		return w.finish(response[w.s.Language], "completed")
	}
	for _, c := range d.Scenarios {
		if c.ScenarioID == "SC37" && c.Confidence >= .75 {
			return w.handoff(ReasonOperatorRequested, "client asked for a human operator: "+c.Reason, "SC37", "operator_requested", nil)
		}
	}
	if w.r.Input.ReviewMode != "auto" {
		bytes := make([]byte, 16)
		if _, err := rand.Read(bytes); err != nil {
			return err
		}
		question := w.e.intentQuestion(w.s.Language, *d)
		w.r.Review = &IntentReview{ProposalID: hex.EncodeToString(bytes), Revision: w.r.Attempts, Target: w.r.Input.ReviewMode, Question: question, Decision: clone(*d), Status: "pending"}
		answer := question
		if w.r.Input.ReviewMode == "operator" {
			answer = local(w.s.Language, "Оператор проверяет запрос.", "Оператор сұрауды тексеріп жатыр.")
		}
		return w.finish(answer, "awaiting_intent_confirmation")
	}
	w.r.Phase = "accepted"
	return w.save("intent_accepted", nil)
}
func (w *work) retrieve() error {
	if w.r.Attempts >= 3 {
		return w.finish(local(w.s.Language, "Уточните, пожалуйста, какой вопрос нужно решить?", "Қандай мәселені шешу керегін нақтылаңызшы?"), "clarification")
	}
	ids := []string{}
	if w.r.Decision != nil {
		for _, c := range w.r.Decision.Alternatives {
			ids = append(ids, c.ScenarioID)
		}
		for _, c := range w.r.Decision.Scenarios {
			sc := w.e.Catalog.Scenarios[c.ScenarioID]
			for _, rule := range sc.Boundaries {
				ids = append(ids, str(rule["use_instead"]))
			}
			ids = append(ids, c.ScenarioID)
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		sc, ok := w.e.Catalog.Scenarios[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		w.r.Steps++
		data := Values{"scenario": sc}
		w.r.RoutingContext = append(w.r.RoutingContext, Values{"kind": "scenario_retrieval", "scenario": sc})
		w.r.Output.Trace.Actions = append(w.r.Output.Trace.Actions, ActionCall{Name: "get_scenario", Mode: "execute", Inputs: Values{"scenario_id": id}, Result: data})
		if err := w.save("scenario_retrieved", data); err != nil {
			return err
		}
		if len(seen) >= 3 || w.r.Steps >= w.e.MaxSteps {
			break
		}
	}
	w.r.Review = nil
	w.r.Output.Review = nil
	w.r.Phase = "identifying"
	return w.save("rerouting", Values{"rejected": w.r.Rejected})
}
