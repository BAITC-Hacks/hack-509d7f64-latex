package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPIntegrationAndAuthentication(t *testing.T) {
	e, _ := setup(t, decision("SC33", Values{"city": "Almaty"}))
	h := Handler(e, "test-token")
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "/healthz", "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/v1/scenarios", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/v1/scenarios", "", "test-token"); w.Code != 200 || !strings.Contains(w.Body.String(), "SC40") {
		t.Fatal(w.Code)
	}
	body := `{"session_id":"call-1","request_id":"one","text":"Где офис?","language":"ru"}`
	w := call("POST", "/v1/turns", body, "test-token")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out Output
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Answer == "" {
		t.Fatal(err)
	}
	w = call("GET", "/v1/sessions/call-1", "", "test-token")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(out.Answer)) {
		t.Fatal("stored answer missing")
	}
	for _, bad := range []string{body + ` {}`, strings.Replace(body, `"language":"ru"`, `"language":"en"`, 1), strings.Replace(body, `"language":"ru"`, `"language":"ru","tool":"cancel_policy"`, 1)} {
		if w := call("POST", "/v1/turns", bad, "test-token"); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := call("GET", "/v1/sessions/absent", "", "test-token"); w.Code != http.StatusNotFound {
		t.Fatal(w.Code)
	}
}

func httpCall(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHTTPIntentReviewAndTurnStatus(t *testing.T) {
	e, _ := setup(t, decision("SC33", Values{"city": "Almaty"}))
	h := Handler(e, "user-token", "operator-token")
	w := httpCall(h, "POST", "/v1/turns", `{"session_id":"review-call","request_id":"one","text":"office address","language":"ru","review_mode":"operator","slots":{"city":"Almaty"}}`, "user-token")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var proposal Output
	if err := json.Unmarshal(w.Body.Bytes(), &proposal); err != nil || proposal.Status != "awaiting_intent_confirmation" || proposal.Review == nil {
		t.Fatal("missing proposal", err, w.Body.String())
	}
	w = httpCall(h, "GET", "/v1/sessions/review-call/turns/one", "", "operator-token")
	var current struct {
		Phase  string `json:"phase"`
		Output Output `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &current); err != nil || w.Code != http.StatusOK || current.Phase != "awaiting_intent_confirmation" || current.Output.Review == nil || current.Output.Review.ProposalID != proposal.Review.ProposalID {
		t.Fatal("invalid turn projection", err, w.Body.String())
	}
	in := ReviewInput{RequestID: "review-1", TurnRequestID: "one", ProposalID: proposal.Review.ProposalID, Revision: proposal.Review.Revision, Decision: "approved"}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if w = httpCall(h, "POST", "/v1/sessions/review-call/reviews", string(body), "user-token"); w.Code != http.StatusConflict {
		t.Fatal("user must not approve an operator proposal", w.Code, w.Body.String())
	}
	spoofed := strings.TrimSuffix(string(body), "}") + `,"actor":"operator"}`
	if w = httpCall(h, "POST", "/v1/sessions/review-call/reviews", spoofed, "user-token"); w.Code != http.StatusBadRequest {
		t.Fatal("body cannot set actor", w.Code, w.Body.String())
	}
	w = httpCall(h, "POST", "/v1/sessions/review-call/reviews", string(body), "operator-token")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"completed"`) {
		t.Fatal("operator approval failed", w.Code, w.Body.String())
	}
	completed := w.Body.String()
	w = httpCall(h, "POST", "/v1/sessions/review-call/reviews", string(body), "operator-token")
	if w.Code != http.StatusOK || w.Body.String() != completed {
		t.Fatal("review retry must return saved output", w.Code, w.Body.String())
	}
	if w = httpCall(h, "GET", "/v1/sessions/review-call/turns/absent", "", "user-token"); w.Code != http.StatusNotFound {
		t.Fatal(w.Code)
	}
}

func TestHTTPOperatorConfigurationAndBearerSyntax(t *testing.T) {
	e, _ := setup(t)
	h := Handler(e, "user-token")
	w := httpCall(h, "POST", "/v1/turns", `{"session_id":"review-call","request_id":"one","text":"office address","language":"ru","review_mode":"operator"}`, "user-token")
	if w.Code != http.StatusBadRequest {
		t.Fatal("operator review must not become permanently unreviewable", w.Code)
	}
	r := httptest.NewRequest("GET", "/v1/scenarios", nil)
	r.Header.Set("Authorization", "user-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("require Bearer scheme", w.Code)
	}
}

type failingHTTPRepo struct {
	Repository
	err error
}

func (r failingHTTPRepo) Ready(context.Context) error                          { return r.err }
func (r failingHTTPRepo) Get(context.Context, string) (Session, error)         { return Session{}, r.err }
func (r failingHTTPRepo) GetTurn(context.Context, string, string) (Run, error) { return Run{}, r.err }

func TestHTTPDatabaseFailureAndReadiness(t *testing.T) {
	e, _ := setup(t)
	if w := httpCall(Handler(e, "token"), "GET", "/readyz", "", ""); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	e.Store = failingHTTPRepo{Repository: e.Store, err: fmt.Errorf("%w: private connection details", ErrDatabase)}
	h := Handler(e, "token")
	for _, path := range []string{"/readyz", "/v1/sessions/call-1", "/v1/sessions/call-1/turns/one"} {
		w := httpCall(h, "GET", path, "", "token")
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private") {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	if w := httpCall(h, "GET", "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatal("database outage must not fail process health", w.Code)
	}
}
