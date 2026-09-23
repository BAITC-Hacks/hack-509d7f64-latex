package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// OpenHandoffs makes the in-memory test repository a HandoffLister. Declared
// here so it keeps compiling wherever memoryTestRepo itself lives.
func (m *memoryTestRepo) OpenHandoffs(ctx context.Context) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Session{}
	for _, s := range m.sessions {
		if s.Operator.Open() {
			c := clone(s)
			c.Turns = []Turn{}
			out = append(out, c)
		}
	}
	return out, nil
}

// spyModel counts every model call and keeps the state each route received.
type spyModel struct {
	*fakeModel
	mu       sync.Mutex
	routes   int
	responds int
	states   []Session
}

func (m *spyModel) Route(ctx context.Context, in Input, s Session, o RouteOptions) (Decision, error) {
	m.mu.Lock()
	m.routes++
	m.states = append(m.states, clone(s))
	m.mu.Unlock()
	return m.fakeModel.Route(ctx, in, s, o)
}
func (m *spyModel) Respond(ctx context.Context, lang string, facts Values) (string, error) {
	m.mu.Lock()
	m.responds++
	m.mu.Unlock()
	return m.fakeModel.Respond(ctx, lang, facts)
}
func (m *spyModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes + m.responds
}

func setupOperator(t *testing.T, ds ...Decision) (*Engine, *spyModel) {
	t.Helper()
	e, fm := setup(t, ds...)
	spy := &spyModel{fakeModel: fm}
	e.Model = spy
	e.Operators.WebhookURL = ""
	return e, spy
}

func turn(t *testing.T, e *Engine, id, text string) Output {
	t.Helper()
	o, err := e.Process(context.Background(), input(id, text))
	if err != nil {
		t.Fatal(id, err)
	}
	return o
}

func session(t *testing.T, e *Engine, id string) Session {
	t.Helper()
	s, err := e.Store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claim(t *testing.T, e *Engine, id, name string) Output {
	t.Helper()
	o, err := e.ClaimHandoff(context.Background(), "call-1", OperatorClaimRequest{RequestID: id, Operator: name})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func say(t *testing.T, e *Engine, id, text string) Output {
	t.Helper()
	o, err := e.SendOperatorMessage(context.Background(), "call-1", OperatorMessageRequest{RequestID: id, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func closeTicket(t *testing.T, e *Engine, id string, back bool) Output {
	t.Helper()
	o, err := e.CloseHandoff(context.Background(), "call-1", OperatorCloseRequest{RequestID: id, Resolution: "решено", ReturnToBot: back})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func needsHandoff(id string, slots Values) Decision {
	d := decision(id, slots)
	d.NeedsHandoff = true
	return d
}

func lowConfidence() Decision {
	d := decision("SC25", Values{})
	d.Scenarios[0].Confidence = .3
	return d
}

func TestEveryGiveUpPathOpensWaitingTicket(t *testing.T) {
	missingPolicy := decision("SC25", Values{"phone": "+77010000001", "policy_number": "SQ-OGPO-999999"})
	unknownPhone := decision("SC25", Values{"phone": "+77010000099"})
	cases := []struct {
		name, reason, queue string
		decisions           []Decision
		turns               []string
		prepare             func(*Engine, *spyModel)
	}{
		{name: "explicit request SC37", reason: ReasonOperatorRequested, queue: "operator_general", decisions: []Decision{decision("SC37", Values{})}, turns: []string{"Соедините с оператором"}},
		{name: "two low-confidence verdicts", reason: ReasonLowConfidence, queue: "operator_general", decisions: []Decision{lowConfidence(), lowConfidence()}, turns: []string{"ну там", "это вот"}},
		{name: "fallback ladder bottomed out", reason: ReasonRoutingFailed, queue: "operator_general", turns: []string{"привет"}, prepare: func(_ *Engine, m *spyModel) { m.routeErr = errors.New("provider unavailable") }},
		{name: "processing budget", reason: ReasonProcessingBudget, queue: "operator_general", decisions: []Decision{decision("SC33", Values{"city": "Almaty"})}, turns: []string{"Офис"}, prepare: func(e *Engine, _ *spyModel) { e.MaxSteps = 0 }},
		{name: "tool failed twice", reason: ReasonToolFailed, queue: "operator_general", decisions: []Decision{missingPolicy, missingPolicy}, turns: []string{"мой полис SQ-OGPO-999999", "тот же полис"}},
		{name: "identification failed twice", reason: ReasonIdentificationFailed, queue: "operator_general", decisions: []Decision{unknownPhone, unknownPhone}, turns: []string{"мой полис", "номер тот же"}},
		{name: "SC11 injured", reason: ReasonUrgentScenario, queue: "claims_team", decisions: []Decision{decision("SC11", Values{"injured": true, "location": "Алматы, Абая 10"})}, turns: []string{"ДТП, есть пострадавшие"}},
		{name: "SC15 needs handoff", reason: ReasonUrgentScenario, queue: "medical_assistance_24_7", decisions: []Decision{needsHandoff("SC15", Values{})}, turns: []string{"Мне плохо в Турции"}},
		{name: "SC38 needs handoff", reason: ReasonUrgentScenario, queue: "security_team", decisions: []Decision{needsHandoff("SC38", Values{"fraud_details": "Сообщил код из SMS"})}, turns: []string{"Я продиктовал код мошенникам"}},
		{name: "SC30 charged but not issued", reason: ReasonScenarioHandoff, queue: "operator_general", decisions: []Decision{decision("SC30", Values{"phone": "+77010000003", "payment_date": "2026-09-30"})}, turns: []string{"Деньги списали, полиса нет"}},
		{name: "SC10 always hands off", reason: ReasonScenarioHandoff, queue: "corporate_sales", decisions: []Decision{decision("SC10", Values{"company_name": "ТОО Альфа", "employees_count": float64(50), "phone": "+77010000001"})}, turns: []string{"Страховка для компании"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, m := setupOperator(t, tc.decisions...)
			if tc.prepare != nil {
				tc.prepare(e, m)
			}
			var out Output
			for i, text := range tc.turns {
				out = turn(t, e, "turn-"+string(rune('a'+i)), text)
			}
			if out.Status != "handoff" || out.Answer == "" {
				t.Fatalf("status %q answer %q", out.Status, out.Answer)
			}
			if out.Handoff == nil || out.Handoff.Reason != tc.reason || out.Handoff.Status != "waiting" || out.Handoff.Queue != tc.queue || !strings.HasPrefix(out.Handoff.TicketID, "HO-") {
				t.Fatalf("output handoff %+v", out.Handoff)
			}
			if out.Trace.Handoff == nil || out.Trace.Handoff.Reason != tc.reason || out.Trace.Handoff.Detail == "" {
				t.Fatalf("trace handoff %+v", out.Trace.Handoff)
			}
			if !actionExecuted(out, "transfer_to_operator") {
				t.Fatal("transfer_to_operator receipt missing")
			}
			s := session(t, e, "call-1")
			if !s.Operator.Open() || s.Operator.Status != "waiting" || s.Operator.Reason != tc.reason || s.Operator.TicketID != out.Handoff.TicketID || s.Operator.RequestID != out.RequestID {
				t.Fatalf("ticket %+v", s.Operator)
			}
			if s.Operator.Context.CurrentInput.Text != tc.turns[len(tc.turns)-1] || len(s.Operator.Context.Transcript) != len(tc.turns)-1 {
				t.Fatalf("context packet %+v", s.Operator.Context)
			}
		})
	}
}

func TestInvalidStoredDecisionHandsOff(t *testing.T) {
	e, m := setupOperator(t)
	ctx := context.Background()
	in := input("one", "anything")
	in.ReviewMode = "auto"
	lease, err := e.Store.Lock(ctx, in.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := lease.Load(ctx)
	r, _, err := lease.Begin(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// A worker stopped right after the model proposal was recorded.
	s.TurnCount = 1
	bad := decision("SC999", Values{})
	r.Phase, r.Decision = "validating", &bad
	r.Output = Output{SessionID: s.ID, RequestID: in.RequestID, PendingScenarios: []string{}, Trace: Trace{Turn: 1, Transcript: in.Text, Actions: []ActionCall{}, LatencyMS: map[string]int64{}}}
	if err := lease.Save(ctx, &s, &r, nil); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	out := turn(t, e, "one", "anything")
	if out.Status != "handoff" || out.Handoff == nil || out.Handoff.Reason != ReasonInvalidDecision || m.calls() != 0 {
		t.Fatalf("%+v %+v", out.Status, out.Handoff)
	}
	if c := session(t, e, "call-1").Operator.Context; c.Decision == nil || c.Decision.Scenarios[0].ScenarioID != "SC999" {
		t.Fatal("supervisor cannot see the rejected decision", c.Decision)
	}
}

func TestHandoffContextShowsWhereBotDoubted(t *testing.T) {
	first, second := lowConfidence(), lowConfidence()
	second.Alternatives = []Candidate{{ScenarioID: "SC26", Confidence: .2, Reason: "maybe claim status"}}
	e, _ := setupOperator(t, first, second)
	turn(t, e, "one", "ну там")
	out := turn(t, e, "two", "это вот")
	c := session(t, e, "call-1").Operator.Context
	if c.Decision == nil || c.Decision.Scenarios[0].ScenarioID != "SC25" || len(c.Alternatives) != 1 || c.Alternatives[0].ScenarioID != "SC26" {
		t.Fatalf("decision/alternatives missing: %+v", c)
	}
	if c.Uncertainty == nil || c.Uncertainty.Verdict != "handoff" || c.Path == "" || c.LowConfidenceTurns != 2 {
		t.Fatalf("uncertainty/path missing: %+v", c)
	}
	if out.Trace.Uncertainty == nil || out.Trace.Handoff.Reason != ReasonLowConfidence || !strings.Contains(out.Trace.Handoff.Detail, "consecutive") {
		t.Fatalf("trace %+v", out.Trace.Handoff)
	}
}

func TestOperatorTakeoverAndHandback(t *testing.T) {
	e, m := setupOperator(t, decision("SC37", Values{}), decision("SC33", Values{"city": "Almaty"}))
	ctx := context.Background()
	handoff := turn(t, e, "one", "Позовите человека")
	if handoff.Status != "handoff" {
		t.Fatal(handoff.Status)
	}
	calls := m.calls()

	waiting := turn(t, e, "two", "Алло, вы здесь?")
	if waiting.Status != "with_operator" || waiting.Answer != holdAnswer("ru") || waiting.Handoff == nil || waiting.Handoff.Status != "waiting" || waiting.Trace.Path != "operator" {
		t.Fatalf("waiting turn %+v", waiting)
	}
	if again := turn(t, e, "two", "Алло, вы здесь?"); !equalJSON(again, waiting) {
		t.Fatal("with_operator retry was not idempotent")
	}

	connected := claim(t, e, "op-claim", "Айгерим")
	if connected.Status != "operator_connected" || connected.Handoff.Status != "connected" {
		t.Fatalf("claim %+v", connected)
	}
	silent := turn(t, e, "three", "Мне нужен возврат")
	if silent.Status != "with_operator" || silent.Answer != "" || len(silent.OperatorMessages) != 0 || silent.Handoff.Status != "connected" {
		t.Fatalf("connected turn %+v", silent)
	}

	msg := say(t, e, "op-msg-1", "Здравствуйте, я оформлю возврат вручную.")
	if msg.Status != "operator_message" || msg.Answer != "Здравствуйте, я оформлю возврат вручную." {
		t.Fatalf("operator message %+v", msg)
	}
	delivered := turn(t, e, "four", "Спасибо")
	if delivered.Status != "with_operator" || len(delivered.OperatorMessages) != 1 || delivered.OperatorMessages[0].Text != msg.Answer || delivered.OperatorMessages[0].Operator != "Айгерим" {
		t.Fatalf("operator message not delivered: %+v", delivered.OperatorMessages)
	}
	if next := turn(t, e, "five", "Жду"); len(next.OperatorMessages) != 0 {
		t.Fatal("operator message delivered twice")
	}
	if m.calls() != calls {
		t.Fatalf("with_operator turns called the model %d times", m.calls()-calls)
	}

	feed, err := e.OperatorMessages(ctx, "call-1", 0)
	if err != nil || len(feed.Messages) != 2 || feed.Messages[0].Role != "system" || feed.Messages[1].Text != msg.Answer || feed.Handoff.Status != "connected" {
		t.Fatalf("feed %+v %v", feed, err)
	}
	if after, _ := e.OperatorMessages(ctx, "call-1", msg.Trace.Turn); len(after.Messages) != 0 {
		t.Fatal("after filter ignored")
	}
	view, err := e.Handoff(ctx, "call-1")
	if err != nil || len(view.ClientTurns) != 4 || view.ClientTurns[0].Text != "Алло, вы здесь?" || len(view.RecentTurns) == 0 {
		t.Fatalf("view %+v %v", view.ClientTurns, err)
	}

	closed := closeTicket(t, e, "op-close", true)
	if closed.Status != "operator_closed" || closed.Answer != "" || closed.Handoff == nil || closed.Handoff.Status != "closed" || !closed.Handoff.ReturnToBot {
		t.Fatalf("close %+v", closed)
	}
	s := session(t, e, "call-1")
	if s.Operator != nil || len(s.OperatorHistory) != 1 || s.OperatorHistory[0].Status != "closed" {
		t.Fatal("ticket not archived")
	}

	back := turn(t, e, "six", "Где ваш офис в Алматы?")
	if back.Status != "completed" || back.Handoff != nil || m.routes != 2 {
		t.Fatalf("bot did not resume: %+v routes=%d", back, m.routes)
	}
	state := m.states[len(m.states)-1]
	sawMessage := false
	for _, tr := range state.Turns {
		if tr.Output != nil && tr.Output.Status == "operator_message" && tr.Input.Text == msg.Answer {
			sawMessage = true
		}
	}
	if !sawMessage {
		t.Fatal("model history lacks the operator message")
	}
	messages, err := routingMessages(input("six", "Где ваш офис в Алматы?"), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(messages)
	if !strings.Contains(string(encoded), msg.Answer) || !strings.Contains(string(encoded), "operator_handback") {
		t.Fatal("model input lacks operator message or handback note")
	}
}

func TestCloseWithoutReturnClearsWorkflow(t *testing.T) {
	e, _ := setupOperator(t, decision("SC01", Values{"region": "almaty"}), decision("SC37", Values{}), decision("SYS_GOODBYE", Values{}))
	if o := turn(t, e, "one", "Посчитайте ОГПО"); o.Status != "awaiting_slot" {
		t.Fatal(o.Status)
	}
	turn(t, e, "two", "Дайте оператора")
	if s := session(t, e, "call-1"); s.Active == nil || s.Active.ScenarioID != "SC01" {
		t.Fatal("handoff must keep the workflow for the operator")
	}
	claim(t, e, "op-claim", "Ерлан")
	closed := closeTicket(t, e, "op-close", false)
	if closed.Status != "operator_closed" || !strings.Contains(closed.Answer, "Оператор завершил разговор") || closed.Handoff.ReturnToBot {
		t.Fatalf("close %+v", closed)
	}
	s := session(t, e, "call-1")
	if s.Active != nil || len(s.Stack) != 0 || len(s.Queue) != 0 || s.Operator != nil {
		t.Fatal("workflow not cleared")
	}
	feed, _ := e.OperatorMessages(context.Background(), "call-1", closed.Trace.Turn-1)
	if len(feed.Messages) != 1 || feed.Messages[0].Text != closed.Answer || feed.Handoff.Status != "closed" {
		t.Fatalf("closing notice not published: %+v", feed)
	}
	if o := turn(t, e, "three", "До свидания"); o.Status != "completed" {
		t.Fatal("bot did not take the next turn", o.Status)
	}
}

func TestCloseWithReturnKeepsWorkflow(t *testing.T) {
	e, _ := setupOperator(t, decision("SC01", Values{"region": "almaty"}), decision("SC37", Values{}))
	turn(t, e, "one", "Посчитайте ОГПО")
	turn(t, e, "two", "Дайте оператора")
	closeTicket(t, e, "op-close", true)
	s := session(t, e, "call-1")
	if s.Active == nil || s.Active.ScenarioID != "SC01" || s.LowConfidence != 0 {
		t.Fatal("return_to_bot must keep the workflow")
	}
}

func TestOperatorActionsAreIdempotent(t *testing.T) {
	e, _ := setupOperator(t, decision("SC37", Values{}))
	ctx := context.Background()
	if _, err := e.ClaimHandoff(ctx, "call-1", OperatorClaimRequest{RequestID: "op-1", Operator: "Айгерим"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("claim without ticket", err)
	}
	turn(t, e, "one", "Оператора")
	if _, err := e.SendOperatorMessage(ctx, "call-1", OperatorMessageRequest{RequestID: "op-0", Text: "hi"}); !errors.Is(err, ErrConflict) {
		t.Fatal("message before claim", err)
	}
	first := claim(t, e, "op-1", "Айгерим")
	if again := claim(t, e, "op-1", "Айгерим"); !equalJSON(first, again) {
		t.Fatal("claim retry differs")
	}
	if _, err := e.ClaimHandoff(ctx, "call-1", OperatorClaimRequest{RequestID: "op-1", Operator: "Ерлан"}); !errors.Is(err, ErrConflict) {
		t.Fatal("changed claim payload", err)
	}
	if _, err := e.ClaimHandoff(ctx, "call-1", OperatorClaimRequest{RequestID: "op-2", Operator: "Ерлан"}); !errors.Is(err, ErrConflict) {
		t.Fatal("second claim", err)
	}
	msg := say(t, e, "op-3", "Слушаю вас")
	if again := say(t, e, "op-3", "Слушаю вас"); !equalJSON(msg, again) {
		t.Fatal("message retry differs")
	}
	if _, err := e.SendOperatorMessage(ctx, "call-1", OperatorMessageRequest{RequestID: "op-3", Text: "другое"}); !errors.Is(err, ErrConflict) {
		t.Fatal("changed message payload", err)
	}
	if _, err := e.Process(ctx, input("op-3", "Слушаю вас")); !errors.Is(err, ErrConflict) {
		t.Fatal("user turn reused an operator request_id", err)
	}
	feed, _ := e.OperatorMessages(ctx, "call-1", 0)
	if len(feed.Messages) != 2 {
		t.Fatalf("retries duplicated messages: %+v", feed.Messages)
	}
	closed := closeTicket(t, e, "op-4", true)
	if again := closeTicket(t, e, "op-4", true); !equalJSON(closed, again) {
		t.Fatal("close retry differs")
	}
	if _, err := e.CloseHandoff(ctx, "call-1", OperatorCloseRequest{RequestID: "op-4", ReturnToBot: false}); !errors.Is(err, ErrConflict) {
		t.Fatal("changed close payload", err)
	}
	if _, err := e.CloseHandoff(ctx, "call-1", OperatorCloseRequest{RequestID: "op-5", ReturnToBot: true}); !errors.Is(err, ErrNotFound) {
		t.Fatal("close without open ticket", err)
	}
	bad := input("x", "hi")
	bad.Operator = &OperatorAction{Kind: "message"}
	if _, err := e.Process(ctx, bad); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("layer 1 must not create operator runs", err)
	}
}

func TestOperatorActionWhileUserTurnInFlight(t *testing.T) {
	e, _ := setupOperator(t, decision("SC37", Values{}))
	ctx := context.Background()
	turn(t, e, "one", "Оператора")
	lease, err := e.Store.Lock(ctx, "call-1")
	if err != nil {
		t.Fatal(err)
	}
	in := input("two", "ещё вопрос")
	in.ReviewMode = "auto"
	if _, _, err := lease.Begin(ctx, in); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if _, err := e.ClaimHandoff(ctx, "call-1", OperatorClaimRequest{RequestID: "op-1", Operator: "Айгерим"}); !errors.Is(err, ErrBusy) {
		t.Fatal("operator action must not interleave with an open user turn", err)
	}
}

func TestOpenHandoffsListingAndUnsupportedStore(t *testing.T) {
	e, _ := setupOperator(t, decision("SC11", Values{"injured": true, "location": "Алматы"}), decision("SC37", Values{}))
	ctx := context.Background()
	turn(t, e, "one", "Авария, есть раненые")
	other := input("one", "Оператора")
	other.SessionID = "call-2"
	if _, err := e.Process(ctx, other); err != nil {
		t.Fatal(err)
	}
	views, err := e.OpenHandoffs(ctx, "")
	if err != nil || len(views) != 2 || views[0].SessionID != "call-1" || views[1].SessionID != "call-2" {
		t.Fatalf("listing %+v %v", views, err)
	}
	if views[0].Ticket.Context.CurrentInput.Text != "Авария, есть раненые" || views[0].ClientTurns == nil {
		t.Fatal("listing lacks context")
	}
	if claims, _ := e.OpenHandoffs(ctx, "claims_team"); len(claims) != 1 || claims[0].Ticket.Queue != "claims_team" {
		t.Fatal("queue filter")
	}
	if _, err := e.OpenHandoffs(ctx, "nowhere"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("unknown queue", err)
	}
	e.Store = struct{ Repository }{e.Store}
	if _, err := e.OpenHandoffs(ctx, ""); !errors.Is(err, ErrNotImplemented) {
		t.Fatal("expected ErrNotImplemented", err)
	}
}

func TestFailedTransferDoesNotStrandTheCaller(t *testing.T) {
	e, m := setupOperator(t, decision("SC37", Values{}), decision("SC33", Values{"city": "Almaty"}))
	e.Catalog.Queues = []string{} // the mock transfer now rejects every queue
	out := turn(t, e, "one", "Оператора")
	if out.Status != "handoff" || out.Handoff == nil || out.Handoff.Status != "failed" || out.Handoff.Reason != ReasonOperatorRequested || out.Trace.Error != "handoff failed" {
		t.Fatalf("%+v", out)
	}
	if session(t, e, "call-1").Operator != nil {
		t.Fatal("no ticket may exist when the transfer failed")
	}
	if next := turn(t, e, "two", "Где офис?"); next.Status != "completed" || m.routes != 2 {
		t.Fatal("bot must keep serving when no operator could be reached", next.Status)
	}
}

func TestMaskPII(t *testing.T) {
	masked := maskPII(generic(Values{"phone": "+77010000003", "drivers_iin": []any{"850314300121"}, "note": "звоните +77010000003, ИИН 850314300121", "policy_number": "SQ-OGPO-104501"}))
	b, _ := json.Marshal(masked)
	text := string(b)
	if strings.Contains(text, "+77010000003") || strings.Contains(text, "850314300121") || !strings.Contains(text, "***0003") || !strings.Contains(text, "***0121") || !strings.Contains(text, "SQ-OGPO-104501") {
		t.Fatal(text)
	}
}
