package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeModel struct {
	mu         sync.Mutex
	decisions  []Decision
	calls      int
	routeErr   error
	respondErr error
}

func (m *fakeModel) Route(ctx context.Context, in Input, s Session, _ RouteOptions) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.routeErr != nil {
		return Decision{}, m.routeErr
	}
	if len(m.decisions) == 0 {
		return Decision{}, errors.New("no scripted decision")
	}
	d := m.decisions[0]
	m.decisions = m.decisions[1:]
	return clone(d), nil
}
func (m *fakeModel) Respond(context.Context, string, Values) (string, error) {
	return "Ответ на основе результатов.", m.respondErr
}
func decision(id string, slots Values) Decision {
	return Decision{Scenarios: []Candidate{{ScenarioID: id, Confidence: .95, Reason: "test routing decision"}}, Language: "ru", Slots: slots, IsContinuation: true}
}
func setup(t *testing.T, ds ...Decision) (*Engine, *fakeModel) {
	t.Helper()
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeModel{decisions: ds}
	return NewEngine(c, m, testRepo(t, c)), m
}
func input(id, text string) Input {
	return Input{SessionID: "call-1", RequestID: id, Text: text, Language: "ru"}
}
func process(t *testing.T, e *Engine, in Input) Output {
	t.Helper()
	o, err := e.Process(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if o.Answer == "" {
		t.Fatal("missing final answer")
	}
	r, err := e.Store.GetTurn(context.Background(), in.SessionID, o.RequestID)
	if err != nil || r.Output.Answer == "" {
		t.Fatal("answer not committed before return")
	}
	return o
}
func actionExecuted(o Output, name string) bool {
	for _, a := range o.Trace.Actions {
		if a.Name == name && a.Mode == "execute" && errorCode(a.Result) == "" {
			return true
		}
	}
	return false
}

func TestStoreAnswerAndDuplicateRequest(t *testing.T) {
	e, m := setup(t, decision("SC33", Values{"city": "Almaty"}))
	in := input("one", "Адрес в Алматы")
	a := process(t, e, in)
	b := process(t, e, in)
	if a.Status != "completed" || !equalJSON(a, b) || m.calls != 1 {
		t.Fatalf("duplicate was rerun: %#v", b)
	}
	in.Text = "different"
	if _, err := e.Process(context.Background(), in); !errors.Is(err, ErrConflict) {
		t.Fatal("expected duplicate conflict")
	}
	s, _ := e.Store.Get(context.Background(), in.SessionID)
	s.Turns[0].Output.Answer = "tampered"
	s2, _ := e.Store.Get(context.Background(), in.SessionID)
	if s2.Turns[0].Output.Answer == "tampered" {
		t.Fatal("snapshot aliases live state")
	}
}
func TestConfirmationAndRetryDoNotRepeatMutation(t *testing.T) {
	slots := Values{"phone": "+77010000003", "contact_field": "email", "new_value": "new@mail.example"}
	e, m := setup(t, decision("SC29", slots), decision("SC29", Values{}))
	first := process(t, e, input("one", "Измените почту"))
	if first.Status != "awaiting_confirmation" || actionExecuted(first, "update_contact") {
		t.Fatal("mutation ran before confirmation")
	}
	second := process(t, e, input("two", "Да, верно."))
	if second.Status != "completed" || !actionExecuted(second, "update_contact") {
		t.Fatalf("confirmation did not execute: %#v", second)
	}
	_ = process(t, e, input("two", "Да, верно."))
	if m.calls != 2 {
		t.Fatal("retry called model")
	}
	c := e.Store.(*memoryTestRepo).backend.Execute("find_client", Values{"phone": "+77010000003"}, "")
	if c["email"] != "new@mail.example" {
		t.Fatal("mutation not stored")
	}
}
func TestCorrectionInvalidatesConfirmation(t *testing.T) {
	e, _ := setup(t, decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "one@mail.example"}), decision("SC29", Values{"new_value": "two@mail.example"}), decision("SC29", Values{}))
	process(t, e, input("one", "Изменить почту"))
	o := process(t, e, input("two", "Да"))
	if o.Status != "awaiting_confirmation" || actionExecuted(o, "update_contact") {
		t.Fatal("changed parameters executed with stale confirmation")
	}
	o = process(t, e, input("three", "Подтверждаю"))
	if !actionExecuted(o, "update_contact") {
		t.Fatal("fresh confirmation failed")
	}
}
func TestInterveningTurnInvalidatesConfirmation(t *testing.T) {
	e, _ := setup(t, decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "one@mail.example"}), decision("SYS_OUT_OF_SCOPE", Values{}), decision("SC29", Values{}))
	process(t, e, input("one", "Поменять почту"))
	process(t, e, input("two", "Какая погода?"))
	o := process(t, e, input("three", "Да"))
	if actionExecuted(o, "update_contact") || o.Status != "awaiting_confirmation" {
		t.Fatal("stale yes executed mutation")
	}
}
func TestTopicSwitchAndReturnPreserveSlots(t *testing.T) {
	e, _ := setup(t, decision("SC01", Values{"region": "almaty"}), decision("SC33", Values{"city": "Astana"}), decision("SC01", Values{"vehicle_type": "car", "drivers_iin": []any{"850314300121"}}))
	if o := process(t, e, input("one", "Посчитайте ОГПО")); o.Status != "awaiting_slot" {
		t.Fatal(o.Status)
	}
	o := process(t, e, input("two", "Где офис в Астане?"))
	if o.Status != "completed" || len(o.PendingScenarios) != 1 || o.PendingScenarios[0] != "SC01" {
		t.Fatal("interrupted topic lost")
	}
	o = process(t, e, input("three", "Вернёмся к ОГПО, легковая"))
	if !actionExecuted(o, "calc_ogpo_price") {
		t.Fatalf("topic did not resume: %+v", o)
	}
	for _, a := range o.Trace.Actions {
		if a.Name == "calc_ogpo_price" && number(a.Result["price"]) != 30400 {
			t.Fatal(a.Result)
		}
	}
}
func TestQuoteToPurchaseCarriesParameters(t *testing.T) {
	e, _ := setup(t, decision("SC01", Values{"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121"}}), decision("SC02", Values{"phone": "+77010000001", "vehicle_plate": "777ABC02"}), decision("SC02", Values{}))
	process(t, e, input("one", "Цена ОГПО"))
	o := process(t, e, input("two", "Давайте оформим"))
	if o.Status != "awaiting_confirmation" {
		t.Fatalf("quote context lost: %+v", o)
	}
	o = process(t, e, input("three", "Да"))
	if !actionExecuted(o, "create_policy") || !actionExecuted(o, "send_sms") {
		t.Fatalf("purchase not completed: %+v", o)
	}
}
func TestUrgentFirstAndMultiIntentQueue(t *testing.T) {
	d := decision("SC33", Values{"city": "Almaty", "fraud_details": "Asked for SMS code"})
	d.Scenarios = append(d.Scenarios, Candidate{ScenarioID: "SC38", Confidence: .99, Reason: "fraud"})
	e, _ := setup(t, d, d)
	o := process(t, e, input("one", "Офис, и мне звонили мошенники"))
	if o.Trace.Decision.Scenarios[0].ScenarioID != "SC38" || !actionExecuted(o, "report_fraud") {
		t.Fatal("urgent scenario did not execute first")
	}
	if len(o.PendingScenarios) != 1 || o.PendingScenarios[0] != "SC33" {
		t.Fatal("secondary intent dropped")
	}
}
func TestLowConfidenceTwiceHandsOff(t *testing.T) {
	d := decision("SC25", Values{})
	d.Scenarios[0].Confidence = .3
	e, _ := setup(t, d, d, d, d)
	if o := process(t, e, input("one", "ну там")); o.Status != "clarification" {
		t.Fatal(o.Status)
	}
	o := process(t, e, input("two", "это вот"))
	if o.Status != "handoff" || !actionExecuted(o, "transfer_to_operator") {
		t.Fatal("missing low-confidence handoff")
	}
}
func TestMediumConfidenceNeverExecutesTools(t *testing.T) {
	d := decision("SC29", Values{})
	d.Scenarios[0].Confidence = .6
	e, _ := setup(t, d, d)
	o := process(t, e, input("one", "Изменить что-то"))
	if o.Status != "clarification" || actionExecuted(o, "update_contact") {
		t.Fatal(o)
	}
}
func TestModelFailureStillStoresAnswer(t *testing.T) {
	e, m := setup(t)
	m.routeErr = errors.New("provider unavailable")
	o := process(t, e, input("one", "привет"))
	if o.Status != "handoff" || o.Trace.Error == "" {
		t.Fatal(o)
	}
}
func TestInvalidModelScenarioCannotExecute(t *testing.T) {
	e, _ := setup(t, decision("SC999", Values{}))
	o := process(t, e, input("one", "anything"))
	if o.Status != "handoff" {
		t.Fatal(o)
	}
}
func TestNoConfirmationOnFirstTurn(t *testing.T) {
	e, _ := setup(t, decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "new@mail.example"}))
	o := process(t, e, input("one", "Да"))
	if o.Status != "awaiting_confirmation" || actionExecuted(o, "update_contact") {
		t.Fatal(o)
	}
}
func TestNegativeConfirmationCancels(t *testing.T) {
	e, _ := setup(t, decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "new@mail.example"}), decision("SC29", Values{}))
	process(t, e, input("one", "Поменяйте почту"))
	o := process(t, e, input("two", "Нет"))
	if o.Status != "cancelled" || actionExecuted(o, "update_contact") {
		t.Fatal(o)
	}
}
func TestIdentificationFailureReasksThenHandoff(t *testing.T) {
	d := decision("SC25", Values{"phone": "+77010000099"})
	e, _ := setup(t, d, d, d, d)
	if o := process(t, e, input("one", "мой полис")); o.Status != "awaiting_slot" {
		t.Fatal(o)
	}
	if o := process(t, e, input("two", "номер тот же")); o.Status != "handoff" {
		t.Fatal(o)
	}
}
func TestKnownPhoneInfersSinglePolicy(t *testing.T) {
	e, _ := setup(t, decision("SC25", Values{"phone": "+77010000011"}))
	o := process(t, e, input("one", "Действует страховка?"))
	if o.Status != "completed" || !actionExecuted(o, "get_policy") {
		t.Fatal(o)
	}
}
func TestKazakhMixedAndSlotValidation(t *testing.T) {
	d := decision("SC33", Values{})
	d.Language = "kk"
	e, _ := setup(t, d, d)
	in := input("one", "Адрес қайда?")
	in.Language = "mixed"
	o := process(t, e, in)
	if o.Language != "kk" || !strings.Contains(o.Answer, "қала") {
		t.Fatal(o.Answer)
	}
	in.RequestID = "two"
	in.Slots = Values{"phone": "bad"}
	if _, err := e.Process(context.Background(), in); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("bad phone accepted")
	}
}
func TestExecutionBudgetProducesStoredHandoff(t *testing.T) {
	e, _ := setup(t, decision("SC33", Values{"city": "Almaty"}))
	e.MaxSteps = 0
	o := process(t, e, input("one", "Офис"))
	if o.Status != "handoff" {
		t.Fatal(o)
	}
}

type concurrentModel struct{ calls atomic.Int32 }

func (m *concurrentModel) Route(context.Context, Input, Session, RouteOptions) (Decision, error) {
	m.calls.Add(1)
	time.Sleep(10 * time.Millisecond)
	return decision("SC33", Values{"city": "Almaty"}), nil
}
func (m *concurrentModel) Respond(context.Context, string, Values) (string, error) {
	return "Адрес офиса сохранён.", nil
}
func TestConcurrentDuplicateRunsOnce(t *testing.T) {
	c, _ := LoadCatalog()
	m := &concurrentModel{}
	e := NewEngine(c, m, testRepo(t, c))
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Process(context.Background(), input("same", "office"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if m.calls.Load() != 1 {
		t.Fatal("concurrent duplicate processed more than once")
	}
}
func TestSeparateSessionsAreIsolated(t *testing.T) {
	c, _ := LoadCatalog()
	m := &concurrentModel{}
	e := NewEngine(c, m, testRepo(t, c))
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := input("one", "office")
			in.SessionID = fmt.Sprintf("session-%d", i)
			if _, err := e.Process(context.Background(), in); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if m.calls.Load() != 10 {
		t.Fatal(m.calls.Load())
	}
}
