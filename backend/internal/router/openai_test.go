package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

const testRouteResult = `{"scenarios":[{"scenario_id":"SC33","confidence":0.94,"reason":"office request"}],"alternatives":[],"language":"kk","slots":[{"name":"city","value_json":"\"Astana\""}],"is_continuation":false,"needs_handoff":false}`

func routeTestPath(value any, keys ...string) any {
	for _, key := range keys {
		switch object := value.(type) {
		case Values:
			value = object[key]
		case map[string]any:
			value = object[key]
		default:
			return nil
		}
	}
	return value
}

func routeResponse(w http.ResponseWriter) {
	writeJSON(w, 200, Values{"status": "completed", "output": []any{Values{"type": "message", "role": "assistant", "content": []any{Values{"type": "output_text", "text": testRouteResult}}}}})
}

func modelForServer(t *testing.T, server *httptest.Server) *OpenAI {
	t.Helper()
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	m := NewOpenAI("key", "test-model", c)
	m.BaseURL = server.URL
	m.HTTP = server.Client()
	return m
}

func TestOpenAIResponsesWireContract(t *testing.T) {
	c, _ := LoadCatalog()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("invalid OpenAI request")
		}
		var body Values
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["store"] != false || routeTestPath(body, "text", "format", "type") != "json_schema" || routeTestPath(body, "text", "format", "strict") != true {
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
	d, err := m.Route(context.Background(), input("one", "Астана офис"), Session{}, RouteOptions{})
	if err != nil || d.Slots["city"] != "Astana" || d.Language != "kk" {
		t.Fatal(d, err)
	}
}

func TestOpenAIToolRoundTrip(t *testing.T) {
	var calls atomic.Int32
	var first Values
	var events []Values
	providerOutput := []any{
		Values{"type": "reasoning", "id": "reasoning-one", "encrypted_content": "opaque", "summary": []any{}},
		Values{"type": "function_call", "id": "fc-one", "call_id": "call-scenario-one", "name": "get_scenario", "arguments": `{"scenario_id":"SC33"}`, "status": "completed"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body Values
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if calls.Add(1) == 1 {
			first = body
			tools := body["tools"].([]any)
			if len(tools) != 1 || routeTestPath(tools[0], "name") != "get_scenario" || routeTestPath(tools[0], "strict") != true || routeTestPath(tools[0], "parameters", "additionalProperties") != false {
				t.Error("routing should expose only a strict read-only scenario tool")
			}
			if body["parallel_tool_calls"] != false || !strings.Contains(str(body["include"]), "reasoning.encrypted_content") {
				t.Error("missing serial stateless tool loop configuration")
			}
			writeJSON(w, 200, Values{"status": "completed", "output": providerOutput})
			return
		}
		if !reflect.DeepEqual(body["instructions"], first["instructions"]) || !reflect.DeepEqual(body["tools"], first["tools"]) || !reflect.DeepEqual(body["text"], first["text"]) {
			t.Error("static prompt prefix changed between tool calls")
		}
		items := body["input"].([]any)
		base := first["input"].([]any)
		if len(items) != len(base)+3 || !reflect.DeepEqual(items[:len(base)], base) {
			t.Error("tool continuation changed initial context")
			return
		}
		if routeTestPath(items[len(base)], "encrypted_content") != "opaque" || routeTestPath(items[len(base)+1], "id") != "fc-one" {
			t.Error("provider reasoning and function output items must be retained verbatim")
		}
		toolOutput := items[len(base)+2]
		if routeTestPath(toolOutput, "type") != "function_call_output" || routeTestPath(toolOutput, "call_id") != "call-scenario-one" || !strings.Contains(str(routeTestPath(toolOutput, "output")), `"scenario_id":"SC33"`) {
			t.Error("tool output did not match the original call_id and scenario")
		}
		if output := str(routeTestPath(toolOutput, "output")); !strings.Contains(output, `"city":{`) || strings.Contains(output, `"responses"`) {
			t.Error("get_scenario should return the routing view with slot definitions")
		}
		if len(events) != 1 || events[0]["kind"] != "scenario_retrieved" {
			t.Error("scenario result must be checkpointed before the next model request")
		}
		routeResponse(w)
	}))
	defer server.Close()
	m := modelForServer(t, server)
	ctx := withRouteRecorder(context.Background(), func(kind string, payload Values) error {
		events = append(events, Values{"kind": kind, "payload": clone(payload)})
		return nil
	})
	decision, err := m.Route(ctx, input("one", "Астана офис"), Session{}, RouteOptions{})
	if err != nil || decision.Slots["city"] != "Astana" || calls.Load() != 2 {
		t.Fatal(decision, err, calls.Load())
	}
	if len(events) != 2 || routeTestPath(events[0], "payload", "tool_call", "call_id") != "call-scenario-one" || routeTestPath(events[1], "kind") != "model_proposal" {
		t.Fatal("incomplete durable route events", events)
	}
}

func TestRoutingContextChronologyAndCurrentInput(t *testing.T) {
	in := input("current", "CURRENT_UNIQUE_TEXT")
	state := Session{Language: "ru", Identity: Values{"client_id": "CL001"}, Active: &Frame{ScenarioID: "SC33", Slots: Values{"city": "Astana"}}, RoutingContext: []Values{{"kind": "review_feedback", "feedback": "REJECTED_FEEDBACK"}, {"kind": "retrieval", "scenario_id": "SC34"}}}
	for i := 0; i < 12; i++ {
		state.Turns = append(state.Turns, Turn{Input: input(fmt.Sprint(i), fmt.Sprintf("history-%d", i)), Output: &Output{Answer: fmt.Sprintf("answer-%d", i)}})
	}
	state.Turns = append(state.Turns, Turn{Input: input("incomplete", "UNFINISHED_INPUT")})
	// Pending review can have an output but is still the current request.
	state.Turns = append(state.Turns, Turn{Input: in, Output: &Output{Answer: "CURRENT_PENDING_ANSWER"}})
	messages, err := routingMessages(in, state, Values{"kind": "candidate_scenarios", "scenarios": []Values{{"scenario_id": "SC33"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 25 {
		t.Fatalf("want 20 history messages + workflow + candidates + input + 2 events, got %d", len(messages))
	}
	for i := 0; i < 10; i++ {
		if routeTestPath(messages[i*2], "role") != "user" || routeTestPath(messages[i*2], "content") != fmt.Sprintf("history-%d", i+2) || routeTestPath(messages[i*2+1], "role") != "assistant" || routeTestPath(messages[i*2+1], "content") != fmt.Sprintf("answer-%d", i+2) {
			t.Fatal("history order or role is incorrect", messages)
		}
	}
	if !strings.Contains(str(routeTestPath(messages[20], "content")), "workflow_context") || !strings.Contains(str(routeTestPath(messages[21], "content")), "candidate_scenarios") || routeTestPath(messages[21], "role") != "user" || !strings.Contains(str(routeTestPath(messages[22], "content")), "current_input") || !strings.Contains(str(routeTestPath(messages[23], "content")), "REJECTED_FEEDBACK") || !strings.Contains(str(routeTestPath(messages[24], "content")), "SC34") {
		t.Fatal("workflow/candidates/input/events not in chronological order")
	}
	encoded, _ := json.Marshal(messages)
	if strings.Count(string(encoded), "CURRENT_UNIQUE_TEXT") != 1 || strings.Contains(string(encoded), "UNFINISHED_INPUT") || strings.Contains(string(encoded), "CURRENT_PENDING_ANSWER") {
		t.Fatal("current turn was duplicated in model context")
	}
}

func TestOpenAIPromptStableAcrossInputs(t *testing.T) {
	var requests []Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body Values
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests = append(requests, body)
		routeResponse(w)
	}))
	defer server.Close()
	m := modelForServer(t, server)
	for i, text := range []string{"FIRST_UNIQUE_TEXT", "SECOND_UNIQUE_TEXT"} {
		opts := RouteOptions{}
		if i == 1 {
			opts.Candidates = []string{"SC33", "SC23"}
		}
		if _, err := m.Route(context.Background(), input(text, text), Session{}, opts); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"instructions", "tools", "text"} {
		if !reflect.DeepEqual(requests[0][key], requests[1][key]) {
			t.Fatalf("%s should not change with user input", key)
		}
	}
	if !strings.Contains(str(requests[0]["instructions"]), "PROMPT_VERSION:") || strings.Contains(str(requests[0]["instructions"]), "FIRST_UNIQUE_TEXT") {
		t.Fatal("system prompt missing version or contains variable input")
	}
}

// routeData decodes a request's data messages by kind, plus every message
// content joined, for substring checks.
func routeData(body Values) (map[string]Values, string) {
	data, contents := map[string]Values{}, []string{}
	items, _ := body["input"].([]any)
	for _, item := range items {
		content := str(routeTestPath(item, "content"))
		contents = append(contents, content)
		var message Values
		if json.Unmarshal([]byte(content), &message) == nil && message["kind"] != nil {
			data[str(message["kind"])] = message
		}
	}
	return data, strings.Join(contents, "\n")
}

func candidateIDs(data Values) []string {
	ids := []string{}
	for _, sc := range list(data["scenarios"]) {
		ids = append(ids, str(asMap(sc)["scenario_id"]))
	}
	return ids
}

func TestOpenAIFullRouteDetailsOnlyCandidates(t *testing.T) {
	var body Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		routeResponse(w)
	}))
	defer server.Close()
	m := modelForServer(t, server)
	m.FastModel = "fast-model"
	state := Session{Language: "ru", Turns: []Turn{{Input: input("old", "EARLIER_TURN"), Output: &Output{Answer: "EARLIER_ANSWER"}}}}
	if _, err := m.Route(context.Background(), input("one", "Астана офис"), state, RouteOptions{Candidates: []string{"SC33", "SYS_GOODBYE", "SC99", "SC17", "SC33"}}); err != nil {
		t.Fatal(err)
	}
	data, contents := routeData(body)
	candidates := data["candidate_scenarios"]
	if ids := candidateIDs(candidates); !reflect.DeepEqual(ids, []string{"SC17", "SC33"}) {
		t.Fatal("candidate details should cover exactly the requested scenarios, in catalog order", ids)
	}
	slots := asMap(candidates["slots"])
	for _, name := range []string{"city", "claim_number", "phone", "iin"} {
		if routeTestPath(slots, name, "type") == nil || routeTestPath(slots, name, "prompt") != nil {
			t.Fatal("candidate slot definitions incomplete or carry prompts", name, slots)
		}
	}
	if slots["trip_country"] != nil || candidates["system_intents"] != nil {
		t.Fatal("full route should define only candidate slots and index system intents statically")
	}
	items := body["input"].([]any)
	if data["workflow_context"] == nil || !strings.Contains(contents, "EARLIER_TURN") || !strings.Contains(str(routeTestPath(items[len(items)-2], "content")), `"kind":"candidate_scenarios"`) || !strings.Contains(str(routeTestPath(items[len(items)-1], "content")), `"kind":"current_input"`) {
		t.Fatal("candidates belong right before current_input, after history and workflow state")
	}
	if body["model"] != "test-model" || body["tools"] == nil || body["include"] == nil {
		t.Fatal("full route keeps the main model and the get_scenario loop")
	}
	c := m.Catalog
	if !strings.Contains(contents, c.Scenarios["SC33"].Examples["kk"][0]) || strings.Contains(contents, c.Scenarios["SC01"].Examples["ru"][0]) {
		t.Fatal("examples should be sent for candidates only")
	}
	instructions := str(body["instructions"])
	if !strings.Contains(instructions, "PROMPT_VERSION: voice-router.intent.v3") || strings.Contains(instructions, "SLOTS:") || strings.Contains(instructions, "not_this_if\":") {
		t.Fatal("static prompt should be the compact v3 index", instructions)
	}
	for _, sc := range c.Ordered {
		if !strings.Contains(instructions, "\n"+sc.ID+" [") {
			t.Fatal("index misses", sc.ID)
		}
		for _, examples := range sc.Examples {
			for _, example := range examples {
				if strings.Contains(instructions, example) {
					t.Fatal("static prompt must not carry examples", sc.ID, example)
				}
			}
		}
	}
	for id := range c.System {
		if !strings.Contains(instructions, "\n"+id+" [system] ") {
			t.Fatal("index misses", id)
		}
	}
}

func TestOpenAIEmptyCandidatesDetailAllScenarios(t *testing.T) {
	for _, candidates := range [][]string{nil, {"SYS_UNCLEAR", "SC99"}} {
		var body Values
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			routeResponse(w)
		}))
		m := modelForServer(t, server)
		if _, err := m.Route(context.Background(), input("one", "text"), Session{}, RouteOptions{Candidates: candidates}); err != nil {
			t.Fatal(err)
		}
		server.Close()
		data, _ := routeData(body)
		want := []string{}
		used := map[string]bool{}
		for _, sc := range m.Catalog.Ordered {
			want = append(want, sc.ID)
			for _, name := range append(slices.Clone(sc.Slots.Required), sc.Slots.Optional...) {
				used[name] = true
			}
		}
		slots := asMap(data["candidate_scenarios"]["slots"])
		if !reflect.DeepEqual(candidateIDs(data["candidate_scenarios"]), want) {
			t.Fatal("no usable candidates should detail every scenario", candidates)
		}
		for name := range used {
			if slots[name] == nil {
				t.Fatal("missing slot definition", name)
			}
		}
	}
}

func TestOpenAIFastRoute(t *testing.T) {
	var calls atomic.Int32
	var body Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&body)
		routeResponse(w)
	}))
	defer server.Close()
	m := modelForServer(t, server)
	m.FastModel = "fast-model"
	var events []string
	ctx := withRouteRecorder(context.Background(), func(kind string, _ Values) error {
		events = append(events, kind)
		return nil
	})
	state := Session{Language: "ru", Active: &Frame{ScenarioID: "SC17", Slots: Values{"claim_number": "ACTIVE_SLOT"}}, Turns: []Turn{{Input: input("old", "EARLIER_TURN"), Output: &Output{Answer: "EARLIER_ANSWER"}}}, RoutingContext: []Values{{"kind": "review_feedback", "feedback": "OLD_FEEDBACK"}}}
	d, err := m.Route(ctx, input("one", "Астана офис"), state, RouteOptions{Candidates: []string{"SC33", "SC23", "SC99"}, Fast: true})
	if err != nil || d.Scenarios[0].ScenarioID != "SC33" || d.Slots["city"] != "Astana" || calls.Load() != 1 || !reflect.DeepEqual(events, []string{"model_proposal"}) {
		t.Fatal(d, err, calls.Load(), events)
	}
	if body["model"] != "fast-model" || body["tools"] != nil || body["include"] != nil || body["parallel_tool_calls"] != nil || body["store"] != false || routeTestPath(body, "text", "format", "strict") != true {
		t.Fatal("fast route is one stateless strict request on the fast model without tools", body)
	}
	instructions := str(body["instructions"])
	if !strings.Contains(instructions, "PROMPT_VERSION: voice-router.fast.v1") || strings.Contains(instructions, "SCENARIO_INDEX") || instructions == m.routeInstructions {
		t.Fatal("fast route needs its own short instructions")
	}
	schema := routeTestPath(body, "text", "format", "schema", "properties")
	want := []any{"SC23", "SC33", "SYS_GOODBYE", "SYS_OUT_OF_SCOPE", "SYS_UNCLEAR"}
	for _, field := range []string{"scenarios", "alternatives"} {
		if got := routeTestPath(schema, field, "items", "properties", "scenario_id", "enum"); !reflect.DeepEqual(got, want) {
			t.Fatal("fast enum should be candidates plus system intents", field, got)
		}
	}
	if routeTestPath(schema, "slots", "items", "properties", "name", "enum") == nil || routeTestPath(schema, "scenarios", "minItems") != float64(1) {
		t.Fatal("slot names should be restricted to the catalog and a primary scenario required")
	}
	data, contents := routeData(body)
	if len(body["input"].([]any)) != 2 || !reflect.DeepEqual(candidateIDs(data["candidate_scenarios"]), []string{"SC23", "SC33"}) || len(list(data["candidate_scenarios"]["system_intents"])) != 3 || data["current_input"] == nil {
		t.Fatal("fast input is candidate details with system intents, then current_input", contents)
	}
	for _, leaked := range []string{"EARLIER_TURN", "EARLIER_ANSWER", "OLD_FEEDBACK", "ACTIVE_SLOT", "workflow_context"} {
		if strings.Contains(contents, leaked) {
			t.Fatal("fast route must not send dialog state", leaked)
		}
	}
	if _, err := m.Route(ctx, input("two", "text"), Session{}, RouteOptions{Candidates: []string{"SC33"}, Fast: true, Model: "fallback-model"}); err != nil || body["model"] != "fallback-model" {
		t.Fatal("explicit model overrides the fast model", err, body["model"])
	}
}

func TestOpenAIFastRouteRejectsToolCalls(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, Values{"status": "completed", "output": []any{Values{"type": "function_call", "call_id": "call-one", "name": "get_scenario", "arguments": `{"scenario_id":"SC33"}`}}})
	}))
	defer server.Close()
	_, err := modelForServer(t, server).Route(context.Background(), input("one", "text"), Session{}, RouteOptions{Candidates: []string{"SC33"}, Fast: true})
	if err == nil || !strings.Contains(err.Error(), "unexpected tool call") || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
}

func TestRoutingContextOmitsProviderInternals(t *testing.T) {
	event := Values{"kind": "scenario_retrieved", "data": Values{"result": Values{"scenario_id": "SC33"}, "provider_output": []any{Values{"encrypted_content": "OPAQUE_INTERNAL"}}, "tool_output": Values{"output": "DUPLICATE_RESULT"}}}
	messages, err := routingMessages(input("one", "text"), Session{RoutingContext: []Values{event}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(messages)
	if strings.Contains(string(encoded), "OPAQUE_INTERNAL") || strings.Contains(string(encoded), "DUPLICATE_RESULT") || !strings.Contains(string(encoded), "SC33") {
		t.Fatal("retrieval context should keep the result without provider internals")
	}
	if routeTestPath(event, "data", "provider_output") == nil {
		t.Fatal("building model context changed durable trace")
	}
}

func TestOpenAIRejectsInvalidRoutingTools(t *testing.T) {
	for _, test := range []struct {
		name, tool, callID, arguments, want string
	}{
		{"business_tool", "book_appointment", "call-one", `{}`, "unsupported routing tool"},
		{"unknown_id", "get_scenario", "call-one", `{"scenario_id":"SC999"}`, "unknown get_scenario"},
		{"extra_argument", "get_scenario", "call-one", `{"scenario_id":"SC33","override":true}`, "invalid get_scenario"},
		{"malformed", "get_scenario", "call-one", `{`, "invalid get_scenario"},
		{"trailing_json", "get_scenario", "call-one", `{"scenario_id":"SC33"}{}`, "invalid trailing"},
		{"missing_id", "get_scenario", "", `{"scenario_id":"SC33"}`, "missing call_id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeJSON(w, 200, Values{"status": "completed", "output": []any{Values{"type": "function_call", "call_id": test.callID, "name": test.tool, "arguments": test.arguments}}})
			}))
			defer server.Close()
			_, err := modelForServer(t, server).Route(context.Background(), input("one", "text"), Session{}, RouteOptions{})
			if err == nil || !strings.Contains(err.Error(), test.want) || calls.Load() != 1 {
				t.Fatal(err, calls.Load())
			}
		})
	}
}

func TestOpenAIToolLoopLimitAndDuplicateCall(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := fmt.Sprintf("call-%d", calls.Add(1))
				if duplicate {
					id = "call-duplicate"
				}
				writeJSON(w, 200, Values{"status": "completed", "output": []any{Values{"type": "function_call", "call_id": id, "name": "get_scenario", "arguments": `{"scenario_id":"SC33"}`}}})
			}))
			defer server.Close()
			_, err := modelForServer(t, server).Route(context.Background(), input("one", "text"), Session{}, RouteOptions{})
			wantCalls, wantError := int32(4), "limit reached"
			if duplicate {
				wantCalls, wantError = 2, "duplicate routing tool call_id"
			}
			if err == nil || !strings.Contains(err.Error(), wantError) || calls.Load() != wantCalls {
				t.Fatal(err, calls.Load())
			}
		})
	}
}

func TestOpenAICheckpointFailureStopsModelLoop(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, Values{"status": "completed", "output": []any{Values{"type": "function_call", "call_id": "call-one", "name": "get_scenario", "arguments": `{"scenario_id":"SC33"}`}}})
	}))
	defer server.Close()
	checkpointError := errors.New("database unavailable")
	ctx := withRouteRecorder(context.Background(), func(string, Values) error { return checkpointError })
	_, err := modelForServer(t, server).Route(ctx, input("one", "text"), Session{}, RouteOptions{})
	if !errors.Is(err, checkpointError) || calls.Load() != 1 {
		t.Fatal("model advanced past failed persistence", err, calls.Load())
	}
}

func TestOpenAIRespondHasNoToolAccess(t *testing.T) {
	for _, tool := range []bool{false, true} {
		t.Run(fmt.Sprint(tool), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body Values
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["tools"] != nil || body["store"] != false || !strings.Contains(str(body["instructions"]), "Use ONLY supplied facts") {
					t.Error("answer generation must be stateless and grounded without tools")
				}
				if tool {
					writeJSON(w, 200, Values{"status": "completed", "output": []any{Values{"type": "function_call", "call_id": "unexpected", "name": "get_scenario", "arguments": `{}`}}})
					return
				}
				writeJSON(w, 200, Values{"status": "completed", "output": []any{Values{"type": "message", "content": []any{Values{"type": "output_text", "text": `{"answer":"The office is open."}`}}}}})
			}))
			defer server.Close()
			answer, err := modelForServer(t, server).Respond(context.Background(), "ru", Values{"office_open": true})
			if tool {
				if err == nil || !strings.Contains(err.Error(), "unexpected tool call") {
					t.Fatal(answer, err)
				}
			} else if err != nil || answer != "The office is open." {
				t.Fatal(answer, err)
			}
		})
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
			_, err := m.Route(context.Background(), input("one", "text"), Session{}, RouteOptions{})
			if err == nil || strings.Contains(err.Error(), "secret") || int(calls.Load()) != test.wantCalls {
				t.Fatal(err, calls.Load())
			}
		})
	}
}
