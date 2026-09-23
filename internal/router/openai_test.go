package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpenAIResponsesWireContract(t *testing.T) {
	c, _ := LoadCatalog()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("invalid OpenAI request")
		}
		var body Values
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["store"] != false || path(body, "text", "format", "type") != "json_schema" || path(body, "text", "format", "strict") != true {
			t.Error("missing strict structured outputs")
		}
		if !strings.Contains(str(body["instructions"]), "SC40") || strings.Contains(str(body["instructions"]), "U001") {
			t.Error("catalog missing or evaluation labels leaked")
		}
		result := `{"scenarios":[{"scenario_id":"SC33","confidence":0.94,"reason":"office request"}],"alternatives":[],"language":"kk","slots":[{"name":"city","value_json":"\"Astana\""}],"is_continuation":false,"needs_handoff":false}`
		writeJSON(w, 200, Values{"status": "completed", "output": []any{Values{"type": "reasoning"}, Values{"content": []any{Values{"type": "output_text", "text": result}}}}})
	}))
	defer server.Close()
	m := NewOpenAI("test-secret", "test-model", c)
	m.BaseURL = server.URL
	m.HTTP = server.Client()
	d, err := m.Route(context.Background(), input("one", "Астана офис"), Session{})
	if err != nil || d.Slots["city"] != "Astana" || d.Language != "kk" {
		t.Fatal(d, err)
	}
}
func TestOpenAIRetryAndFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantCalls int
	}{
		{"auth", 401, `{"error":{"message":"secret should not leak"}}`, 1},
		{"rate_limit", 429, `{}`, 2},
		{"incomplete", 200, `{"status":"incomplete","output":[]}`, 1},
		{"refusal", 200, `{"status":"completed","output":[{"content":[{"type":"refusal"}]}]}`, 1},
		{"empty", 200, `{"status":"completed","output":[]}`, 1},
		{"malformed", 200, `not-json`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			c, _ := LoadCatalog()
			m := NewOpenAI("key", "model", c)
			m.BaseURL = server.URL
			m.HTTP = server.Client()
			_, err := m.Route(context.Background(), input("one", "text"), Session{})
			if err == nil || strings.Contains(err.Error(), "secret") || int(calls.Load()) != test.wantCalls {
				t.Fatal(err, calls.Load())
			}
		})
	}
}
