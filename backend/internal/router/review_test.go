package router

import (
	"context"
	"errors"
	"testing"
)

func feedback(out Output, id, choice string) ReviewInput {
	return ReviewInput{RequestID: id, TurnRequestID: out.RequestID, ProposalID: out.Review.ProposalID, Revision: out.Review.Revision, Decision: choice}
}
func TestOperatorReviewGatesToolsAndIsIdempotent(t *testing.T) {
	e, m := setup(t, decision("SC33", Values{"city": "Almaty"}))
	in := input("one", "Адрес офиса")
	in.ReviewMode = "operator"
	first := process(t, e, in)
	if first.Status != "awaiting_intent_confirmation" || first.Review == nil || actionExecuted(first, "get_offices") {
		t.Fatalf("tools ran before review: %+v", first)
	}
	review := feedback(first, "review-one", "approved")
	if _, err := e.Review(context.Background(), in.SessionID, review, "user"); !errors.Is(err, ErrConflict) {
		t.Fatal("user approved operator review", err)
	}
	final, err := e.Review(context.Background(), in.SessionID, review, "operator")
	if err != nil || final.Status != "completed" || !actionExecuted(final, "get_offices") {
		t.Fatal(final, err)
	}
	again, err := e.Review(context.Background(), in.SessionID, review, "operator")
	if err != nil || !equalJSON(final, again) || m.calls != 1 {
		t.Fatal("feedback retry was not idempotent", again, err)
	}
	review.RequestID = "stale"
	if _, err := e.Review(context.Background(), in.SessionID, review, "operator"); !errors.Is(err, ErrConflict) {
		t.Fatal("closed review accepted twice", err)
	}
}
func TestRejectedIntentRetrievesAndReroutes(t *testing.T) {
	firstDecision := decision("SC33", Values{"city": "Almaty"})
	firstDecision.Alternatives = []Candidate{{ScenarioID: "SC23", Confidence: .5, Reason: "might mean clinic"}}
	e, m := setup(t, firstDecision, decision("SC23", Values{"city": "Astana"}))
	in := input("one", "Адрес в Алматы")
	in.Slots = Values{"city": "Almaty"}
	in.ReviewMode = "operator"
	first := process(t, e, in)
	reject := feedback(first, "reject-one", "rejected")
	reject.Feedback = "Нужна клиника в Астане, не офис"
	second, err := e.Review(context.Background(), in.SessionID, reject, "operator")
	if err != nil || second.Status != "awaiting_intent_confirmation" || second.Review.Revision != 2 || second.Review.Decision.Scenarios[0].ScenarioID != "SC23" || !actionExecuted(second, "get_scenario") {
		t.Fatalf("did not retrieve and re-route: %+v %v", second, err)
	}
	if actionExecuted(second, "get_offices") || actionExecuted(second, "list_clinics") {
		t.Fatal("business tools executed before approval")
	}
	if second.Review.Decision.Slots["city"] != "Astana" {
		t.Fatal("original normalizer slots overrode review correction")
	}
	stale := feedback(first, "stale", "approved")
	if _, err := e.Review(context.Background(), in.SessionID, stale, "operator"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale proposal authorized new intent")
	}
	final, err := e.Review(context.Background(), in.SessionID, feedback(second, "approve-two", "approved"), "operator")
	if err != nil || !actionExecuted(final, "list_clinics") || actionExecuted(final, "get_offices") || m.calls != 2 {
		t.Fatal(final, err)
	}
}
func TestUserReviewSpeechReplay(t *testing.T) {
	e, m := setup(t, decision("SC33", Values{"city": "Almaty"}))
	in := input("one", "Офис")
	in.ReviewMode = "user"
	first := process(t, e, in)
	if first.Status != "awaiting_intent_confirmation" {
		t.Fatal(first)
	}
	yes := input("two", "Да")
	final := process(t, e, yes)
	if final.Status != "completed" || !actionExecuted(final, "get_offices") {
		t.Fatal(final)
	}
	again := process(t, e, yes)
	if !equalJSON(final, again) || m.calls != 1 {
		t.Fatal("speech review replayed business operation")
	}
	yes.Text = "Нет"
	if _, err := e.Process(context.Background(), yes); !errors.Is(err, ErrConflict) {
		t.Fatal("feedback request conflict not rejected")
	}
}
func TestIntentApprovalDoesNotAuthorizeMutation(t *testing.T) {
	e, _ := setup(t, decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "new@mail.example"}))
	in := input("one", "Изменить почту")
	in.ReviewMode = "user"
	first := process(t, e, in)
	if first.Status != "awaiting_intent_confirmation" {
		t.Fatal(first)
	}
	out, err := e.Review(context.Background(), in.SessionID, feedback(first, "yes", "approved"), "user")
	if err != nil || out.Status != "awaiting_confirmation" || actionExecuted(out, "update_contact") {
		t.Fatal("intent consent authorized irreversible action", out, err)
	}
}
func TestThreeProposalsLimitSurvivesReviews(t *testing.T) {
	e, m := setup(t, decision("SC33", Values{"city": "Almaty"}), decision("SC23", Values{"city": "Almaty"}), decision("SC31", Values{}))
	in := input("one", "Информация")
	in.ReviewMode = "operator"
	out := process(t, e, in)
	for _, id := range []string{"r1", "r2", "r3"} {
		var err error
		out, err = e.Review(context.Background(), in.SessionID, feedback(out, id, "rejected"), "operator")
		if err != nil {
			t.Fatal(err)
		}
	}
	if out.Status != "clarification" || m.calls != 3 {
		t.Fatal("proposal limit not enforced", out, m.calls)
	}
}

type contextCheckingModel struct {
	t     *testing.T
	calls int
}

func (m *contextCheckingModel) Route(ctx context.Context, in Input, s Session) (Decision, error) {
	m.calls++
	for _, turn := range s.Turns {
		if turn.Input.RequestID == in.RequestID {
			m.t.Error("current input duplicated in prior history")
		}
	}
	return decision("SC33", Values{"city": "Almaty"}), nil
}
func (m *contextCheckingModel) Respond(context.Context, string, Values) (string, error) {
	return "Адрес сохранён.", nil
}
func TestHistoryExcludesCurrentInput(t *testing.T) {
	c, _ := LoadCatalog()
	m := &contextCheckingModel{t: t}
	e := NewEngine(c, m, testRepo(t, c))
	process(t, e, input("one", "Офис"))
	process(t, e, input("two", "Ещё офис"))
	if m.calls != 2 {
		t.Fatal(m.calls)
	}
}

func TestReviewCanCorrectSlotsWithoutChangingScenario(t *testing.T) {
	e, m := setup(t, decision("SC33", Values{"city": "Almaty"}), decision("SC33", Values{"city": "Astana"}))
	in := input("one", "Адрес офиса")
	in.ReviewMode = "user"
	in.Slots = Values{"city": "Almaty"}
	first := process(t, e, in)
	reject := feedback(first, "correct-city", "rejected")
	reject.Feedback = "В Астане"
	corrected, err := e.Review(context.Background(), in.SessionID, reject, "user")
	if err != nil || corrected.Review == nil || corrected.Status != "awaiting_intent_confirmation" || corrected.Review.Revision != 2 || corrected.Review.Decision.Slots["city"] != "Astana" {
		t.Fatal("same-scenario correction was discarded", corrected, err)
	}
	final, err := e.Review(context.Background(), in.SessionID, feedback(corrected, "confirm-city", "approved"), "user")
	if err != nil || final.Status != "completed" || m.calls != 2 {
		t.Fatal(final, err)
	}
	for _, call := range final.Trace.Actions {
		if call.Name == "get_offices" && call.Inputs["city"] != "Astana" {
			t.Fatal("executed original rejected city", call)
		}
	}
}

func TestEarlierTurnReplayDoesNotResolvePendingReview(t *testing.T) {
	e, m := setup(t, decision("SC33", Values{"city": "Almaty"}), decision("SC23", Values{"city": "Almaty"}))
	oldInput := input("old", "Адрес офиса")
	old := process(t, e, oldInput)
	newInput := input("new", "Клиника")
	newInput.ReviewMode = "user"
	pending := process(t, e, newInput)
	replay := process(t, e, oldInput)
	if !equalJSON(old, replay) || m.calls != 2 {
		t.Fatal("old request became review feedback", replay)
	}
	current, err := e.Store.GetTurn(context.Background(), newInput.SessionID, newInput.RequestID)
	if err != nil || current.Phase != "awaiting_intent_confirmation" || !equalJSON(current.Output, pending) {
		t.Fatal("pending review changed on unrelated replay", current, err)
	}
}
