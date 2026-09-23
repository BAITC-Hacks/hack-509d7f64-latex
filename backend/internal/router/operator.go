package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrNotImplemented reports an optional store capability that is missing.
var ErrNotImplemented = errors.New("not supported by the configured store")

// Handoff reason codes. Every path on which the bot gives up sets exactly one
// of them through work.handoff (or the budget check in run) and ends in
// transfer(), so supervisors and the operator console see why.
const (
	ReasonOperatorRequested    = "operator_requested"    // SC37, or the client asked for a person
	ReasonLowConfidence        = "low_confidence"        // two consecutive "handoff" uncertainty verdicts
	ReasonRoutingFailed        = "routing_failed"        // fallback ladder bottomed out (L3)
	ReasonInvalidDecision      = "invalid_decision"      // a stored model proposal failed validation
	ReasonProcessingBudget     = "processing_budget"     // step limit or active-time budget exceeded
	ReasonToolFailed           = "tool_failed"           // a tool failed twice or stayed service_unavailable
	ReasonIdentificationFailed = "identification_failed" // client not found twice
	ReasonUrgentScenario       = "urgent_scenario"       // SC11 injured / needs_handoff, SC15, SC38
	ReasonScenarioHandoff      = "scenario_handoff"      // the scenario's own transfer_to_operator rule fired
)

const (
	ticketWaiting      = "waiting"
	ticketConnected    = "connected"
	ticketClosed       = "closed"
	maxConversation    = 200
	maxOperatorHistory = 20
	webhookTimeout     = 3 * time.Second
)

// HandoffLister is an optional Repository capability: list sessions whose
// Session.Operator is open (status waiting or connected). Turns may be empty.
type HandoffLister interface {
	OpenHandoffs(ctx context.Context) ([]Session, error)
}

// OperatorAction is the payload of a run created by the operator API. It is
// part of Input, so a retried request_id with a different payload conflicts.
type OperatorAction struct {
	Kind        string `json:"kind"` // claim | message | close
	Operator    string `json:"operator,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	ReturnToBot bool   `json:"return_to_bot,omitempty"`
}

// HandoffInfo is the compact ticket summary carried by outputs and traces.
type HandoffInfo struct {
	TicketID     string `json:"ticket_id,omitempty"`
	Queue        string `json:"queue,omitempty"`
	Reason       string `json:"reason"`
	Detail       string `json:"detail,omitempty"`
	Scenario     string `json:"scenario_id,omitempty"`
	Status       string `json:"status"` // requested | waiting | connected | closed | failed
	ReturnToBot  bool   `json:"return_to_bot,omitempty"`
	Notification string `json:"notification,omitempty"`
}

// OperatorHandoff is a human takeover ticket, persisted in the session JSON.
type OperatorHandoff struct {
	TicketID    string     `json:"ticket_id"`
	Queue       string     `json:"queue"`
	Reason      string     `json:"reason"`
	Detail      string     `json:"detail,omitempty"`
	Scenario    string     `json:"scenario_id,omitempty"`
	Status      string     `json:"status"` // waiting | connected | closed
	CreatedAt   time.Time  `json:"created_at"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	Operator    string     `json:"operator,omitempty"`
	// RequestID and Turn identify the run that opened the ticket.
	RequestID string         `json:"request_id"`
	Turn      int            `json:"turn"`
	Context   HandoffContext `json:"context"`
	// Conversation holds every client, operator and system line since the
	// handoff, in turn order.
	Conversation []OperatorEntry `json:"conversation"`
	// DeliveredTurn is the last operator message included in a client output.
	DeliveredTurn   int                  `json:"delivered_turn,omitempty"`
	Resolution      string               `json:"resolution,omitempty"`
	ReturnToBot     bool                 `json:"return_to_bot,omitempty"`
	HandbackPending bool                 `json:"handback_pending,omitempty"`
	Notification    *HandoffNotification `json:"notification,omitempty"`
}

// HandoffContext is the packet an operator needs to continue without asking
// the client to repeat: what was said, what the bot understood, how sure it
// was, and where the workflow stopped.
type HandoffContext struct {
	Language           string           `json:"language"`
	CurrentInput       Input            `json:"current_input"`
	Transcript         []Values         `json:"transcript"`
	Active             any              `json:"active,omitempty"`
	PendingScenarios   []string         `json:"pending_scenarios"`
	Identity           Values           `json:"identity,omitempty"`
	Decision           *Decision        `json:"decision,omitempty"`
	Alternatives       []Candidate      `json:"alternatives,omitempty"`
	Uncertainty        *Uncertainty     `json:"uncertainty,omitempty"`
	Path               string           `json:"path,omitempty"`
	Shortlist          []ScoredScenario `json:"shortlist,omitempty"`
	FallbackLevel      int              `json:"fallback_level"`
	Error              string           `json:"error,omitempty"`
	Proposals          []Decision       `json:"proposals,omitempty"`
	LastAction         *ActionCall      `json:"last_action,omitempty"`
	LowConfidenceTurns int              `json:"low_confidence_turns"`
}

type HandoffNotification struct {
	Status      string     `json:"status"` // pending | sent | failed
	Error       string     `json:"error,omitempty"`
	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
}

type OperatorEntry struct {
	Turn      int       `json:"turn"`
	Role      string    `json:"role"` // client | operator | system
	Text      string    `json:"text"`
	Operator  string    `json:"operator,omitempty"`
	RequestID string    `json:"request_id"`
	At        time.Time `json:"at"`
}

// OperatorHub holds the optional human-notification webhook and the
// in-process results of its asynchronous deliveries.
type OperatorHub struct {
	// WebhookURL receives a POST with the masked ticket after each handoff
	// commits. Empty disables notification.
	WebhookURL string
	Client     *http.Client
	results    sync.Map // session ID + "/" + ticket ID -> HandoffNotification
	pending    sync.WaitGroup
}

// WaitNotifications blocks until in-flight webhook deliveries finish.
func (e *Engine) WaitNotifications() { e.Operators.pending.Wait() }

func (t *OperatorHandoff) Open() bool {
	return t != nil && (t.Status == ticketWaiting || t.Status == ticketConnected)
}

func (t *OperatorHandoff) info() *HandoffInfo {
	if t == nil {
		return nil
	}
	i := &HandoffInfo{TicketID: t.TicketID, Queue: t.Queue, Reason: t.Reason, Detail: t.Detail, Scenario: t.Scenario, Status: t.Status, ReturnToBot: t.ReturnToBot}
	if t.Notification != nil {
		i.Notification = t.Notification.Status
	}
	return i
}

func tickets(s *Session) []*OperatorHandoff {
	out := []*OperatorHandoff{}
	for i := range s.OperatorHistory {
		out = append(out, &s.OperatorHistory[i])
	}
	if s.Operator != nil {
		out = append(out, s.Operator)
	}
	return out
}

func appendEntry(entries []OperatorEntry, entry OperatorEntry) []OperatorEntry {
	entries = append(entries, entry)
	if len(entries) > maxConversation {
		entries = entries[len(entries)-maxConversation:]
	}
	return entries
}

func handoffAnswer(lang string) string {
	return local(lang, "Передаю запрос оператору вместе с контекстом.", "Сұрауды мәліметтерімен бірге операторға беремін.")
}

func holdAnswer(lang string) string {
	return local(lang, "Оператор подключается, пожалуйста, оставайтесь на линии.", "Оператор қосылып жатыр, желіден шықпай күте тұрыңыз.")
}

// handoff is the single entry into the operator handoff: it records the
// machine-readable reason in the trace and routes the run to transfer().
func (w *work) handoff(reason, detail, scenario, event string, data Values) error {
	w.r.Output.Trace.Handoff = &HandoffInfo{Reason: reason, Detail: detail, Scenario: scenario, Status: "requested"}
	w.r.Phase = "handoff"
	if data == nil {
		data = Values{}
	}
	data["reason"] = reason
	return w.save(event, data)
}

// handoffQueue picks the scenario's own queue when its rule fired, when it is
// urgent, or when its tools failed; everything else goes to the general queue.
func (e *Engine) handoffQueue(s *Session, h *HandoffInfo) string {
	id := h.Scenario
	if id == "" && s.Active != nil {
		id = s.Active.ScenarioID
	}
	if sc, ok := e.Catalog.Scenarios[id]; ok && sc.Handoff != nil && slices.Contains(e.Catalog.Queues, sc.Handoff.Queue) {
		if h.Scenario != "" || sc.Priority == "urgent" || h.Reason == ReasonToolFailed || h.Reason == ReasonIdentificationFailed {
			return sc.Handoff.Queue
		}
	}
	return "operator_general"
}

func (e *Engine) handoffContext(s *Session, r *Run) HandoffContext {
	tr := r.Output.Trace
	c := HandoffContext{
		Language: s.Language, CurrentInput: clone(r.Input), Transcript: history(s.Turns),
		Active: compactFrame(s.Active), PendingScenarios: pendingIDs(s), Identity: clone(s.Identity),
		Uncertainty: clone(tr.Uncertainty), Path: tr.Path, Shortlist: clone(tr.Shortlist),
		FallbackLevel: tr.FallbackLevel, Error: tr.Error, Proposals: clone(tr.Proposals),
		LastAction: clone(r.LastTool), LowConfidenceTurns: s.LowConfidence,
	}
	c.CurrentInput.Operator = nil
	if r.Decision != nil {
		d := clone(*r.Decision)
		c.Decision, c.Alternatives = &d, d.Alternatives
	}
	return c
}

// transfer opens the operator ticket. The mock transfer_to_operator receipt,
// the ticket in the session and the turn's handoff answer commit together.
func (w *work) transfer() error {
	s, tr := w.s, &w.r.Output.Trace
	if s.Operator.Open() {
		// Opened earlier in this run (e.g. the budget ran out while wording
		// the answer): never open a second ticket.
		tr.Handoff = s.Operator.info()
		return w.finish(handoffAnswer(s.Language), "handoff")
	}
	h := tr.Handoff
	if h == nil {
		h = &HandoffInfo{Reason: ReasonRoutingFailed, Detail: "handoff without a recorded reason", Status: "requested"}
		tr.Handoff = h
	}
	h.Queue = w.e.handoffQueue(s, h)
	packet := w.e.handoffContext(s, w.r)
	summary := toValues(packet)
	summary["session_id"] = s.ID
	summary["detail"] = h.Detail
	args := Values{"queue": h.Queue, "reason": h.Reason, "context": summary}
	created := false
	err := w.tool("transfer_to_operator", args, func(result Values) {
		if s.Active != nil {
			s.Active.Pending = nil
		}
		s.PendingTurnID = ""
		if errorCode(result) != "" {
			h.Status = "failed"
			tr.Error = "handoff failed"
			w.r.Output.Handoff = clone(h)
			w.settle(local(s.Language, "Не удалось подключить оператора. Пожалуйста, повторите запрос позже.", "Операторға қосылу мүмкін болмады. Кейін қайталап көріңіз."), "handoff")
			return
		}
		if f := s.Active; f != nil && h.Scenario == f.ScenarioID {
			// The scenario's own transfer_to_operator action has now run.
			if sc := w.e.Catalog.Scenarios[f.ScenarioID]; f.NextAction < len(sc.Actions) && sc.Actions[f.NextAction] == "transfer_to_operator" {
				f.Results = append(f.Results, *clone(w.r.LastTool))
				f.NextAction++
			}
		}
		now := time.Now().UTC()
		t := &OperatorHandoff{TicketID: str(result["ticket_id"]), Queue: h.Queue, Reason: h.Reason, Detail: h.Detail, Scenario: h.Scenario,
			Status: ticketWaiting, CreatedAt: now, RequestID: w.r.Input.RequestID, Turn: tr.Turn, Context: packet, Conversation: []OperatorEntry{}}
		if w.e.Operators.WebhookURL != "" {
			t.Notification = &HandoffNotification{Status: "pending"}
		}
		s.Operator = t
		*h = *t.info()
		created = true
		if h.Reason == ReasonScenarioHandoff {
			// Scenario handoffs keep their LLM-worded confirmation.
			w.r.AnswerFacts = Values{"purpose": "confirm handoff to a human operator with context", "queue": h.Queue, "ticket_id": t.TicketID, "scenario_id": h.Scenario}
			w.r.Fallback = local(s.Language, "Передаю разговор профильному оператору с контекстом.", "Әңгімені мәліметтерімен бірге тиісті операторға беремін.")
			w.r.FinalStatus = "handoff"
			w.r.CompleteFrame = false
			w.r.Phase = "generating_answer"
			return
		}
		w.settle(handoffAnswer(s.Language), "handoff")
	})
	if err == nil && created {
		w.e.notifyHandoff(s)
	}
	return err
}

// hold finishes a user turn while a human owns the conversation. It never
// calls a model; the client's words go to the ticket for the operator.
func (e *Engine) hold(ctx context.Context, l Lease, s *Session, r *Run) (Output, error) {
	w := &work{e: e, l: l, s: s, r: r, ctx: ctx, clock: time.Now()}
	t := s.Operator
	t.Conversation = appendEntry(t.Conversation, OperatorEntry{Turn: r.Output.Trace.Turn, Role: "client", Text: r.Input.Text, RequestID: r.Input.RequestID, At: time.Now().UTC()})
	tr := &r.Output.Trace
	tr.Path, tr.ResponseSource = "operator", "operator"
	answer := ""
	if t.Status == ticketWaiting && len(r.Output.OperatorMessages) == 0 {
		answer, tr.ResponseSource = holdAnswer(s.Language), "template"
	}
	tr.Handoff = t.info()
	if err := w.finish(answer, "with_operator"); err != nil {
		return Output{}, err
	}
	return clone(r.Output), nil
}

// takeOperatorMessages returns operator messages not yet included in a
// client output and marks them delivered. The caller persists the session.
func takeOperatorMessages(s *Session) []OperatorEntry {
	var out []OperatorEntry
	for _, t := range tickets(s) {
		for _, m := range t.Conversation {
			if m.Role == "operator" && m.Turn > t.DeliveredTurn {
				out = append(out, m)
				t.DeliveredTurn = m.Turn
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Turn < out[j].Turn })
	return out
}

// takeHandback returns, once, a routing note for the first bot turn after an
// operator returned the conversation to the bot.
func takeHandback(s *Session) Values {
	n := len(s.OperatorHistory)
	if n == 0 || !s.OperatorHistory[n-1].HandbackPending {
		return nil
	}
	t := &s.OperatorHistory[n-1]
	t.HandbackPending = false
	said := []string{}
	for _, m := range t.Conversation {
		if m.Role == "operator" {
			said = append(said, m.Text)
		}
	}
	if len(said) > 5 {
		said = said[len(said)-5:]
	}
	return Values{"kind": "operator_handback", "ticket_id": t.TicketID, "reason": t.Reason, "resolution": t.Resolution, "operator_messages": said}
}

// absorbNotifications copies finished webhook deliveries into their tickets
// and forgets results that are already persisted.
func (e *Engine) absorbNotifications(s *Session) []Values {
	changed := []Values{}
	for _, t := range tickets(s) {
		key := s.ID + "/" + t.TicketID
		if t.Notification == nil || t.Notification.Status != "pending" {
			e.Operators.results.Delete(key)
			continue
		}
		if v, ok := e.Operators.results.Load(key); ok {
			result := v.(HandoffNotification)
			t.Notification = &result
			changed = append(changed, Values{"ticket_id": t.TicketID, "status": result.Status, "error": result.Error})
		}
	}
	return changed
}

// recordNotifications persists absorbed webhook results with an event, so a
// failed notification is visible in the ticket and the event log.
func (e *Engine) recordNotifications(l Lease, s *Session, r *Run) error {
	changed := e.absorbNotifications(s)
	if len(changed) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(l.Context(), 5*time.Second)
	defer cancel()
	return l.Save(ctx, s, r, &Event{Kind: "operator_notification", Data: Values{"notifications": changed}})
}

// notifyHandoff posts the masked ticket to OPERATOR_WEBHOOK_URL after the
// handoff committed. It never blocks or fails the turn; the result is logged
// and recorded on the ticket at the session's next write.
func (e *Engine) notifyHandoff(s *Session) {
	hub, t := &e.Operators, s.Operator
	if hub.WebhookURL == "" || t == nil {
		return
	}
	sessionID, ticketID := s.ID, t.TicketID
	payload, err := json.Marshal(Values{"event": "handoff.created", "session_id": sessionID, "ticket": maskPII(generic(t))})
	if err != nil {
		slog.Warn("operator webhook payload failed", "session_id", sessionID, "ticket_id", ticketID, "error", err)
		return
	}
	hub.pending.Add(1)
	go func() {
		defer hub.pending.Done()
		ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
		defer cancel()
		now := time.Now().UTC()
		result := HandoffNotification{Status: "sent", AttemptedAt: &now}
		if err := hub.post(ctx, payload, ticketID); err != nil {
			result.Status, result.Error = "failed", err.Error()
			slog.Warn("operator webhook failed", "session_id", sessionID, "ticket_id", ticketID, "error", err)
		} else {
			slog.Info("operator webhook delivered", "session_id", sessionID, "ticket_id", ticketID)
		}
		hub.results.Store(sessionID+"/"+ticketID, result)
	}()
}

func (h *OperatorHub) post(ctx context.Context, payload []byte, ticketID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return errors.New("invalid OPERATOR_WEBHOOK_URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", ticketID)
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: webhookTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL may carry a secret; report only the underlying cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("webhook request failed: %w", ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

var piiKeys = []string{"phone", "iin", "new_driver_iin", "drivers_iin", "email", "new_value"}
var longDigits = regexp.MustCompile(`\+?\d{10,}`)

func maskTail(v string) string {
	if len(v) > 4 {
		return "***" + v[len(v)-4:]
	}
	return "***"
}

// maskPII masks phone/IIN-like values the way confirmation() does, both under
// known slot keys and inside free text (spoken numbers in transcripts).
func maskPII(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if slices.Contains(piiKeys, k) {
				out[k] = maskValue(value)
			} else {
				out[k] = maskPII(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = maskPII(value)
		}
		return out
	case string:
		return longDigits.ReplaceAllStringFunc(x, maskTail)
	}
	return v
}

func maskValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = maskValue(value)
		}
		return out
	case map[string]any:
		return maskPII(x)
	}
	if s := str(v); s != "" {
		return maskTail(s)
	}
	return v
}

func generic(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func toValues(v any) Values {
	out := Values{}
	if m, ok := generic(v).(map[string]any); ok {
		for k, x := range m {
			out[k] = x
		}
	}
	return out
}

// ---- Operator actions -------------------------------------------------------

type OperatorClaimRequest struct {
	RequestID string `json:"request_id"`
	Operator  string `json:"operator"`
}
type OperatorMessageRequest struct {
	RequestID string `json:"request_id"`
	Text      string `json:"text"`
}
type OperatorCloseRequest struct {
	RequestID   string `json:"request_id"`
	Resolution  string `json:"resolution,omitempty"`
	ReturnToBot bool   `json:"return_to_bot"`
}

func validOperatorIDs(sessionID, requestID string) error {
	if !idPattern.MatchString(sessionID) || !idPattern.MatchString(requestID) {
		return fmt.Errorf("%w: invalid session_id or request_id", ErrInvalidInput)
	}
	return nil
}

func operatorInput(sessionID, requestID, text string, action OperatorAction) Input {
	return Input{SessionID: sessionID, RequestID: requestID, Text: text, Language: "mixed", ReviewMode: "auto", Operator: &action}
}

func openTicket(s *Session) error {
	if !s.Operator.Open() {
		return fmt.Errorf("%w: no open handoff for this session", ErrNotFound)
	}
	return nil
}

// ClaimHandoff connects a human operator to a waiting ticket.
func (e *Engine) ClaimHandoff(ctx context.Context, sessionID string, req OperatorClaimRequest) (Output, error) {
	name := strings.TrimSpace(req.Operator)
	if err := validOperatorIDs(sessionID, req.RequestID); err != nil {
		return Output{}, err
	}
	if name == "" || utf8.RuneCountInString(name) > 100 || strings.ContainsFunc(name, unicode.IsControl) {
		return Output{}, fmt.Errorf("%w: operator must contain 1-100 printable characters", ErrInvalidInput)
	}
	in := operatorInput(sessionID, req.RequestID, "[оператор подключился]", OperatorAction{Kind: "claim", Operator: name})
	return e.operatorRun(ctx, in, func(s *Session) error {
		if err := openTicket(s); err != nil {
			return err
		}
		if s.Operator.Status != ticketWaiting {
			return fmt.Errorf("%w: handoff already claimed by %s", ErrConflict, s.Operator.Operator)
		}
		return nil
	}, func(w *work, t *OperatorHandoff, now time.Time) (string, string) {
		t.Status, t.Operator, t.ConnectedAt = ticketConnected, name, &now
		t.Conversation = appendEntry(t.Conversation, OperatorEntry{Turn: w.r.Output.Trace.Turn, Role: "system", Text: local(w.s.Language, "Оператор подключился к разговору.", "Оператор әңгімеге қосылды."), Operator: name, RequestID: req.RequestID, At: now})
		return "", "operator_connected"
	})
}

// SendOperatorMessage stores an operator utterance as an ordinary finished
// run, so dialog history (and the bot after a handback) contains it.
func (e *Engine) SendOperatorMessage(ctx context.Context, sessionID string, req OperatorMessageRequest) (Output, error) {
	if err := validOperatorIDs(sessionID, req.RequestID); err != nil {
		return Output{}, err
	}
	if strings.TrimSpace(req.Text) == "" || len(req.Text) > 8000 {
		return Output{}, fmt.Errorf("%w: text must contain 1-8000 bytes", ErrInvalidInput)
	}
	in := operatorInput(sessionID, req.RequestID, req.Text, OperatorAction{Kind: "message"})
	return e.operatorRun(ctx, in, func(s *Session) error {
		if err := openTicket(s); err != nil {
			return err
		}
		if s.Operator.Status != ticketConnected {
			return fmt.Errorf("%w: claim the handoff before sending messages", ErrConflict)
		}
		return nil
	}, func(w *work, t *OperatorHandoff, now time.Time) (string, string) {
		t.Conversation = appendEntry(t.Conversation, OperatorEntry{Turn: w.r.Output.Trace.Turn, Role: "operator", Text: req.Text, Operator: t.Operator, RequestID: req.RequestID, At: now})
		return req.Text, "operator_message"
	})
}

// CloseHandoff ends the takeover. With ReturnToBot the workflow, stack and
// queue stay and the bot answers the next user turn; otherwise they are
// cleared and the client hears a closing notice.
func (e *Engine) CloseHandoff(ctx context.Context, sessionID string, req OperatorCloseRequest) (Output, error) {
	if err := validOperatorIDs(sessionID, req.RequestID); err != nil {
		return Output{}, err
	}
	if len(req.Resolution) > 2000 {
		return Output{}, fmt.Errorf("%w: resolution must contain at most 2000 bytes", ErrInvalidInput)
	}
	text := "[оператор завершил разговор]"
	if req.ReturnToBot {
		text = "[оператор вернул разговор ассистенту]"
	}
	if strings.TrimSpace(req.Resolution) != "" {
		text += " Итог: " + strings.TrimSpace(req.Resolution)
	}
	in := operatorInput(sessionID, req.RequestID, text, OperatorAction{Kind: "close", Resolution: req.Resolution, ReturnToBot: req.ReturnToBot})
	return e.operatorRun(ctx, in, openTicket, func(w *work, t *OperatorHandoff, now time.Time) (string, string) {
		s := w.s
		t.Status, t.ClosedAt, t.Resolution, t.ReturnToBot, t.HandbackPending = ticketClosed, &now, req.Resolution, req.ReturnToBot, req.ReturnToBot
		answer := ""
		if !req.ReturnToBot {
			answer = local(s.Language, "Оператор завершил разговор. Спасибо за обращение в Saqta Insurance!", "Оператор әңгімені аяқтады. Saqta Insurance-ке хабарласқаныңызға рахмет!")
			t.Conversation = appendEntry(t.Conversation, OperatorEntry{Turn: w.r.Output.Trace.Turn, Role: "system", Text: answer, Operator: t.Operator, RequestID: req.RequestID, At: now})
		}
		info := t.info()
		s.OperatorHistory = append(s.OperatorHistory, *t)
		if len(s.OperatorHistory) > maxOperatorHistory {
			s.OperatorHistory = s.OperatorHistory[len(s.OperatorHistory)-maxOperatorHistory:]
		}
		s.Operator = nil
		s.LowConfidence = 0
		s.PendingTurnID = ""
		if req.ReturnToBot {
			// The bot gets a fresh start on the same workflow: stale previews
			// and failure counters would re-trigger the handoff immediately.
			for _, f := range append(append([]*Frame{s.Active}, s.Stack...), s.Queue...) {
				if f != nil {
					f.Pending = nil
					f.Failures = map[string]int{}
				}
			}
			if f := s.Active; f != nil && f.NextAction >= len(w.e.Catalog.Scenarios[f.ScenarioID].Actions) {
				s.LastCompleted, s.Active = f, nil
			}
		} else {
			s.Active, s.Stack, s.Queue = nil, []*Frame{}, []*Frame{}
		}
		w.r.Output.Handoff, w.r.Output.Trace.Handoff = info, clone(info)
		return answer, "operator_closed"
	})
}

// operatorRun executes one operator action as a run: idempotent by
// request_id, 409 on a changed payload, ErrBusy while a user turn is open.
func (e *Engine) operatorRun(ctx context.Context, in Input, check func(*Session) error, apply func(*work, *OperatorHandoff, time.Time) (string, string)) (Output, error) {
	lease, err := e.Store.Lock(ctx, in.SessionID)
	if err != nil {
		return Output{}, err
	}
	defer lease.Release()
	s, err := lease.Load(ctx)
	if err != nil {
		return Output{}, err
	}
	if old, findErr := lease.GetRun(ctx, in.RequestID); findErr == nil {
		if !equalJSON(old.Input, in) {
			return Output{}, ErrConflict
		}
		if old.Phase == "finished" {
			return clone(old.Output), nil
		}
	} else if !errors.Is(findErr, ErrNotFound) {
		return Output{}, findErr
	}
	if err := check(&s); err != nil {
		return Output{}, err
	}
	r, resumed, err := lease.Begin(ctx, in)
	if err != nil {
		return Output{}, err
	}
	if resumed && r.Phase == "finished" {
		return clone(r.Output), nil
	}
	if err := e.recordNotifications(lease, &s, &r); err != nil {
		return Output{}, err
	}
	s.TurnCount++
	r.Output = Output{SessionID: s.ID, RequestID: in.RequestID, Language: s.Language, PendingScenarios: []string{},
		Trace: Trace{Turn: s.TurnCount, Transcript: in.Text, Language: in.Language, Actions: []ActionCall{}, LatencyMS: map[string]int64{}, Path: "operator", ResponseSource: "operator"}}
	w := &work{e: e, l: lease, s: &s, r: &r, ctx: ctx, clock: time.Now()}
	t := s.Operator
	answer, status := apply(w, t, time.Now().UTC())
	if s.Operator != nil {
		r.Output.Trace.Handoff = s.Operator.info()
	}
	if err := w.finish(answer, status); err != nil {
		return Output{}, err
	}
	return clone(r.Output), nil
}

// ---- Operator views ---------------------------------------------------------

type HandoffView struct {
	SessionID        string          `json:"session_id"`
	Language         string          `json:"language"`
	ActiveScenario   string          `json:"active_scenario,omitempty"`
	PendingScenarios []string        `json:"pending_scenarios"`
	Ticket           OperatorHandoff `json:"ticket"`
	// ClientTurns are the client's utterances since the handoff.
	ClientTurns []OperatorEntry `json:"client_turns"`
	// RecentTurns (single-ticket view only) are the last finished runs with
	// full traces, so a supervisor can see where the bot doubted.
	RecentTurns []Turn `json:"recent_turns,omitempty"`
}

type OperatorFeed struct {
	SessionID string          `json:"session_id"`
	Handoff   *HandoffInfo    `json:"handoff"`
	Messages  []OperatorEntry `json:"messages"`
	LastTurn  int             `json:"last_turn"`
}

func handoffView(s *Session, t *OperatorHandoff) HandoffView {
	v := HandoffView{SessionID: s.ID, Language: s.Language, PendingScenarios: pendingIDs(s), Ticket: clone(*t), ClientTurns: []OperatorEntry{}}
	if s.Active != nil {
		v.ActiveScenario = s.Active.ScenarioID
	}
	for _, m := range t.Conversation {
		if m.Role == "client" {
			v.ClientTurns = append(v.ClientTurns, m)
		}
	}
	return v
}

// OpenHandoffs lists open tickets, oldest first, optionally for one queue.
func (e *Engine) OpenHandoffs(ctx context.Context, queue string) ([]HandoffView, error) {
	if queue != "" && !slices.Contains(e.Catalog.Queues, queue) {
		return nil, fmt.Errorf("%w: unknown queue", ErrInvalidInput)
	}
	lister, ok := e.Store.(HandoffLister)
	if !ok {
		return nil, ErrNotImplemented
	}
	sessions, err := lister.OpenHandoffs(ctx)
	if err != nil {
		return nil, err
	}
	views := []HandoffView{}
	for i := range sessions {
		s := &sessions[i]
		if !s.Operator.Open() || (queue != "" && s.Operator.Queue != queue) {
			continue
		}
		e.absorbNotifications(s)
		views = append(views, handoffView(s, s.Operator))
	}
	sort.SliceStable(views, func(i, j int) bool {
		a, b := views[i].Ticket.CreatedAt, views[j].Ticket.CreatedAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return views[i].SessionID < views[j].SessionID
	})
	return views, nil
}

// Handoff returns the open ticket of a session, or its most recent closed one.
func (e *Engine) Handoff(ctx context.Context, sessionID string) (HandoffView, error) {
	if !idPattern.MatchString(sessionID) {
		return HandoffView{}, fmt.Errorf("%w: invalid session ID", ErrInvalidInput)
	}
	s, err := e.Store.Get(ctx, sessionID)
	if err != nil {
		return HandoffView{}, err
	}
	e.absorbNotifications(&s)
	t := s.Operator
	if t == nil && len(s.OperatorHistory) > 0 {
		t = &s.OperatorHistory[len(s.OperatorHistory)-1]
	}
	if t == nil {
		return HandoffView{}, ErrNotFound
	}
	v := handoffView(&s, t)
	v.RecentTurns = s.Turns
	return v, nil
}

// OperatorMessages returns operator and system lines after the given turn
// number, across every ticket of the session, for layer 3 to speak.
func (e *Engine) OperatorMessages(ctx context.Context, sessionID string, after int) (OperatorFeed, error) {
	if !idPattern.MatchString(sessionID) || after < 0 {
		return OperatorFeed{}, fmt.Errorf("%w: invalid session ID or after", ErrInvalidInput)
	}
	s, err := e.Store.Get(ctx, sessionID)
	if err != nil {
		return OperatorFeed{}, err
	}
	feed := OperatorFeed{SessionID: s.ID, Messages: []OperatorEntry{}, LastTurn: s.TurnCount}
	all := tickets(&s)
	for _, t := range all {
		for _, m := range t.Conversation {
			if m.Role != "client" && m.Turn > after {
				feed.Messages = append(feed.Messages, m)
			}
		}
	}
	sort.SliceStable(feed.Messages, func(i, j int) bool { return feed.Messages[i].Turn < feed.Messages[j].Turn })
	if len(all) > 0 {
		feed.Handoff = all[len(all)-1].info()
	}
	return feed, nil
}
