package router

import (
	"context"
	"encoding/json"
	"time"
)

type Values map[string]any

// Input is the boundary with layer 1. Language has already been detected there.
type Input struct {
	SessionID     string `json:"session_id"`
	RequestID     string `json:"request_id"`
	Text          string `json:"text"`
	Language      string `json:"language"`
	ReplyLanguage string `json:"reply_language,omitempty"`
	Slots         Values `json:"slots,omitempty"`
	ReviewMode    string `json:"review_mode,omitempty"`
}
type Candidate struct {
	ScenarioID string  `json:"scenario_id"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}
type Decision struct {
	Scenarios      []Candidate `json:"scenarios"`
	Alternatives   []Candidate `json:"alternatives"`
	Language       string      `json:"language"`
	Slots          Values      `json:"slots"`
	IsContinuation bool        `json:"is_continuation"`
	NeedsHandoff   bool        `json:"needs_handoff"`
}
type ActionCall struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Inputs Values `json:"inputs"`
	Result Values `json:"result"`
}
type Trace struct {
	Turn       int              `json:"turn"`
	Transcript string           `json:"transcript"`
	Language   string           `json:"language"`
	Decision   Decision         `json:"decision"`
	Actions    []ActionCall     `json:"actions"`
	LatencyMS  map[string]int64 `json:"latency_ms"`
	Error      string           `json:"error,omitempty"`
	Proposals  []Decision       `json:"proposals,omitempty"`
	// Path is how the scenario was chosen: bypass (deterministic continuation,
	// no model call), fast (small model over a few candidates), full (main
	// model over the shortlist and dialog state) or fast>full (escalated).
	Path           string           `json:"path,omitempty"`
	Shortlist      []ScoredScenario `json:"shortlist,omitempty"`
	Uncertainty    *Uncertainty     `json:"uncertainty,omitempty"`
	FallbackLevel  int              `json:"fallback_level"`
	ResponseSource string           `json:"response_source,omitempty"`
}

// ScoredScenario is one retrieval candidate. Retrieval only narrows the
// candidates shown to the model; it never assigns a scenario.
type ScoredScenario struct {
	ScenarioID string  `json:"scenario_id"`
	Score      float64 `json:"score"`
}

// Uncertainty is computed in Go from observable signals; the model's own
// confidence is one component, not the verdict.
type Uncertainty struct {
	Score      float64            `json:"score"`
	Components map[string]float64 `json:"components"`
	Verdict    string             `json:"verdict"`
	Boundary   string             `json:"boundary,omitempty"`
}

// RouteOptions narrows one routing call. Candidates are shown to the model in
// full detail; Fast selects the small model and a candidates-only prompt;
// Model overrides the model ID (fallback ladder).
type RouteOptions struct {
	Candidates []string
	Fast       bool
	Model      string
}
type Output struct {
	SessionID        string        `json:"session_id"`
	RequestID        string        `json:"request_id"`
	Answer           string        `json:"answer"`
	Language         string        `json:"language"`
	Status           string        `json:"status"`
	ActiveScenario   string        `json:"active_scenario,omitempty"`
	PendingScenarios []string      `json:"pending_scenarios"`
	Trace            Trace         `json:"trace"`
	Review           *IntentReview `json:"review,omitempty"`
}
type Pending struct {
	Action string `json:"action"`
	Inputs Values `json:"inputs"`
}
type Frame struct {
	ScenarioID string         `json:"scenario_id"`
	Slots      Values         `json:"slots"`
	NextAction int            `json:"next_action"`
	Results    []ActionCall   `json:"results"`
	Pending    *Pending       `json:"pending,omitempty"`
	Failures   map[string]int `json:"failures"`
	// Awaiting lists the slot names (alternatives) the last question asked for.
	Awaiting []string `json:"awaiting,omitempty"`
}
type Turn struct {
	Input  Input   `json:"input"`
	Output *Output `json:"output,omitempty"`
}
type Session struct {
	ID             string                    `json:"session_id"`
	Language       string                    `json:"language"`
	Identity       Values                    `json:"identity"`
	Active         *Frame                    `json:"active,omitempty"`
	LastCompleted  *Frame                    `json:"last_completed,omitempty"`
	Stack          []*Frame                  `json:"stack"`
	Queue          []*Frame                  `json:"queue"`
	Turns          []Turn                    `json:"turns"`
	LowConfidence  int                       `json:"low_confidence"`
	UpdatedAt      time.Time                 `json:"updated_at"`
	Version        int64                     `json:"version"`
	TurnCount      int                       `json:"turn_count"`
	PendingTurnID  string                    `json:"pending_turn_id,omitempty"`
	RoutingContext []Values                  `json:"routing_context,omitempty"`
	ReviewReplies  map[string]UserReviewLink `json:"review_replies,omitempty"`
}
type UserReviewLink struct {
	TurnRequestID string `json:"turn_request_id"`
	InputHash     string `json:"input_hash"`
}
type Model interface {
	Route(context.Context, Input, Session, RouteOptions) (Decision, error)
	Respond(context.Context, string, Values) (string, error)
}

func clone[T any](v T) T { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }
func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func merge(dst, src Values) {
	for k, v := range src {
		dst[k] = v
	}
}
func has(v Values, k string) bool {
	x, ok := v[k]
	return ok && x != nil && str(x) != "" && str(x) != "[]"
}
func local(lang, ru, kk string) string {
	if lang == "kk" {
		return kk
	}
	return ru
}
