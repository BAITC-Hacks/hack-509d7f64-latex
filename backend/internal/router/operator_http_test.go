package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return out
}

func TestHTTPOperatorTakeoverFlow(t *testing.T) {
	e, m := setupOperator(t, decision("SC37", Values{}), decision("SC33", Values{"city": "Almaty"}))
	h := Handler(e, "user-token", "operator-token")
	turnBody := func(id, text string) string {
		return `{"session_id":"call-1","request_id":"` + id + `","text":"` + text + `","language":"ru"}`
	}
	w := httpCall(h, "POST", "/v1/turns", turnBody("one", "Позовите оператора"), "user-token")
	if out := decodeBody[Output](t, w); w.Code != 200 || out.Status != "handoff" || out.Handoff == nil || out.Handoff.TicketID == "" {
		t.Fatal(w.Code, w.Body.String())
	}

	w = httpCall(h, "GET", "/v1/operator/handoffs?queue=operator_general", "", "operator-token")
	list := decodeBody[struct {
		Handoffs []HandoffView `json:"handoffs"`
	}](t, w)
	if w.Code != 200 || len(list.Handoffs) != 1 || list.Handoffs[0].Ticket.Reason != ReasonOperatorRequested || list.Handoffs[0].Ticket.Context.CurrentInput.Text != "Позовите оператора" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = httpCall(h, "GET", "/v1/operator/handoffs?queue=claims_team", "", "operator-token"); !strings.Contains(w.Body.String(), `"handoffs":[]`) {
		t.Fatal("queue filter", w.Body.String())
	}

	w = httpCall(h, "POST", "/v1/operator/handoffs/call-1/claim", `{"request_id":"op-1","operator":"Айгерим"}`, "operator-token")
	if out := decodeBody[Output](t, w); w.Code != 200 || out.Handoff.Status != "connected" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httpCall(h, "POST", "/v1/operator/handoffs/call-1/messages", `{"request_id":"op-2","text":"Здравствуйте, чем помочь?"}`, "operator-token")
	message := decodeBody[Output](t, w)
	if w.Code != 200 || message.Status != "operator_message" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = httpCall(h, "POST", "/v1/operator/handoffs/call-1/messages", `{"request_id":"op-2","text":"другое"}`, "operator-token"); w.Code != http.StatusConflict {
		t.Fatal("changed retry", w.Code)
	}

	w = httpCall(h, "GET", "/v1/sessions/call-1/messages?after=0", "", "user-token")
	feed := decodeBody[OperatorFeed](t, w)
	if w.Code != 200 || len(feed.Messages) != 2 || feed.Messages[1].Text != "Здравствуйте, чем помочь?" || feed.Handoff.Status != "connected" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = httpCall(h, "GET", "/v1/sessions/call-1/messages?after=-1", "", "user-token"); w.Code != http.StatusBadRequest {
		t.Fatal(w.Code)
	}

	w = httpCall(h, "POST", "/v1/turns", turnBody("two", "Мне нужен возврат"), "user-token")
	held := decodeBody[Output](t, w)
	if w.Code != 200 || held.Status != "with_operator" || held.Answer != "" || len(held.OperatorMessages) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httpCall(h, "GET", "/v1/operator/handoffs/call-1", "", "operator-token")
	if view := decodeBody[HandoffView](t, w); w.Code != 200 || len(view.ClientTurns) != 1 || view.ClientTurns[0].Text != "Мне нужен возврат" {
		t.Fatal(w.Code, w.Body.String())
	}

	w = httpCall(h, "POST", "/v1/operator/handoffs/call-1/close", `{"request_id":"op-3","resolution":"вопрос решён","return_to_bot":true}`, "operator-token")
	if out := decodeBody[Output](t, w); w.Code != 200 || out.Status != "operator_closed" || out.Handoff.Status != "closed" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = httpCall(h, "POST", "/v1/operator/handoffs/call-1/messages", `{"request_id":"op-4","text":"ещё"}`, "operator-token"); w.Code != http.StatusNotFound {
		t.Fatal("message after close", w.Code)
	}
	w = httpCall(h, "POST", "/v1/turns", turnBody("three", "Где офис в Алматы?"), "user-token")
	if out := decodeBody[Output](t, w); w.Code != 200 || out.Status != "completed" || m.routes != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestHTTPOperatorAuthorization(t *testing.T) {
	e, _ := setupOperator(t, decision("SC37", Values{}))
	if _, err := e.Process(context.Background(), input("one", "оператора")); err != nil {
		t.Fatal(err)
	}
	paths := []struct{ method, path, body string }{
		{"GET", "/v1/operator/handoffs", ""},
		{"GET", "/v1/operator/handoffs/call-1", ""},
		{"POST", "/v1/operator/handoffs/call-1/claim", `{"request_id":"op-1","operator":"Айгерим"}`},
		{"POST", "/v1/operator/handoffs/call-1/messages", `{"request_id":"op-2","text":"hi"}`},
		{"POST", "/v1/operator/handoffs/call-1/close", `{"request_id":"op-3","return_to_bot":true}`},
	}
	h := Handler(e, "user-token", "operator-token")
	open := Handler(e, "", "operator-token")
	disabled := Handler(e, "user-token")
	for _, p := range paths {
		if w := httpCall(h, p.method, p.path, p.body, ""); w.Code != http.StatusUnauthorized {
			t.Fatal(p.path, "missing token", w.Code)
		}
		if w := httpCall(h, p.method, p.path, p.body, "user-token"); w.Code != http.StatusForbidden {
			t.Fatal(p.path, "user token", w.Code)
		}
		if w := httpCall(open, p.method, p.path, p.body, ""); w.Code != http.StatusUnauthorized {
			t.Fatal(p.path, "unauthenticated local user", w.Code)
		}
		if w := httpCall(disabled, p.method, p.path, p.body, "user-token"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "OPERATOR_API_TOKEN") {
			t.Fatal(p.path, "operator API without OPERATOR_API_TOKEN", w.Code)
		}
	}
	if w := httpCall(h, "POST", "/v1/operator/handoffs/call-1/claim", `{"request_id":"op-1","operator":"Айгерим","actor":"x"}`, "operator-token"); w.Code != http.StatusBadRequest {
		t.Fatal("unknown field", w.Code)
	}
	if w := httpCall(h, "POST", "/v1/turns", `{"session_id":"call-1","request_id":"x","text":"hi","language":"ru","operator":{"kind":"message"}}`, "user-token"); w.Code != http.StatusBadRequest {
		t.Fatal("layer 1 cannot forge operator runs", w.Code)
	}
	if w := httpCall(h, "GET", "/v1/operator/handoffs/call-1", "", "operator-token"); w.Code != http.StatusOK {
		t.Fatal("operator token", w.Code)
	}
	e.Store = struct{ Repository }{e.Store}
	if w := httpCall(h, "GET", "/v1/operator/handoffs", "", "operator-token"); w.Code != http.StatusNotImplemented {
		t.Fatal("store without HandoffLister", w.Code, w.Body.String())
	}
}

type webhookRecorder struct {
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
	status  int
}

func (r *webhookRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(body))
	r.headers = append(r.headers, req.Header.Clone())
	status := r.status
	r.mu.Unlock()
	w.WriteHeader(status)
}

func TestHandoffWebhookSendsMaskedTicket(t *testing.T) {
	rec := &webhookRecorder{status: http.StatusNoContent}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	e, _ := setupOperator(t, decision("SC30", Values{"phone": "+77010000003", "payment_date": "2026-09-30"}))
	e.Operators.WebhookURL = srv.URL + "/hook"
	out := turn(t, e, "one", "Списали деньги 30 сентября, мой номер +77010000003")
	if out.Status != "handoff" || out.Handoff.Notification != "pending" {
		t.Fatalf("%+v", out.Handoff)
	}
	e.WaitNotifications()
	rec.mu.Lock()
	if len(rec.bodies) != 1 {
		t.Fatal("webhook not called once", len(rec.bodies))
	}
	body, header := rec.bodies[0], rec.headers[0]
	rec.mu.Unlock()
	if strings.Contains(body, "+77010000003") || strings.Contains(body, "850314") || !strings.Contains(body, "***0003") {
		t.Fatal("webhook payload leaks phone/IIN", body)
	}
	var payload struct {
		Event     string          `json:"event"`
		SessionID string          `json:"session_id"`
		Ticket    OperatorHandoff `json:"ticket"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Event != "handoff.created" || payload.SessionID != "call-1" || payload.Ticket.TicketID != out.Handoff.TicketID || payload.Ticket.Reason != ReasonScenarioHandoff || payload.Ticket.Context.CurrentInput.Text == "" {
		t.Fatalf("payload %+v", payload)
	}
	if header.Get("Authorization") != "" || header.Get("Idempotency-Key") != out.Handoff.TicketID || header.Get("Content-Type") != "application/json" {
		t.Fatal("unexpected webhook headers", header)
	}
	if next := turn(t, e, "two", "Алло"); next.Status != "with_operator" || next.Handoff.Notification != "sent" {
		t.Fatalf("delivery not recorded: %+v", next.Handoff)
	}
	if s := session(t, e, "call-1"); s.Operator.Notification == nil || s.Operator.Notification.Status != "sent" {
		t.Fatal("delivery not persisted")
	}
}

func TestFailingWebhookNeverBreaksTheTurn(t *testing.T) {
	rec := &webhookRecorder{status: http.StatusInternalServerError}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	e, _ := setupOperator(t, decision("SC37", Values{}))
	e.Operators.WebhookURL = srv.URL
	out := turn(t, e, "one", "Оператора")
	if out.Status != "handoff" || out.Handoff == nil || out.Handoff.Status != "waiting" {
		t.Fatalf("%+v", out)
	}
	e.WaitNotifications()
	next := turn(t, e, "two", "Алло")
	if next.Status != "with_operator" || next.Handoff.Notification != "failed" {
		t.Fatalf("failure not recorded: %+v", next.Handoff)
	}
	s := session(t, e, "call-1")
	if s.Operator.Notification == nil || s.Operator.Notification.Status != "failed" || !strings.Contains(s.Operator.Notification.Error, "500") {
		t.Fatalf("failure not persisted: %+v", s.Operator.Notification)
	}

	// Unreachable endpoint: same outcome, and the URL is not echoed.
	srv.Close()
	e2, _ := setupOperator(t, decision("SC37", Values{}))
	e2.Operators.WebhookURL = srv.URL + "/secret-path"
	if out := turn(t, e2, "one", "Оператора"); out.Status != "handoff" {
		t.Fatal(out.Status)
	}
	e2.WaitNotifications()
	turn(t, e2, "two", "Алло")
	if n := session(t, e2, "call-1").Operator.Notification; n == nil || n.Status != "failed" || strings.Contains(n.Error, "secret-path") {
		t.Fatalf("%+v", n)
	}
}

func TestNoWebhookWhenUnset(t *testing.T) {
	e, _ := setupOperator(t, decision("SC37", Values{}))
	out := turn(t, e, "one", "Оператора")
	e.WaitNotifications()
	if out.Handoff.Notification != "" || session(t, e, "call-1").Operator.Notification != nil {
		t.Fatal("notification recorded without OPERATOR_WEBHOOK_URL")
	}
}
