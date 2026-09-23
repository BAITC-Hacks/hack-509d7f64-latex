package router

import (
	"bytes"
	"encoding/json"
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
