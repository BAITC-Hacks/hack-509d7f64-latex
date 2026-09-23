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
}
type Output struct {
	SessionID        string   `json:"session_id"`
	RequestID        string   `json:"request_id"`
	Answer           string   `json:"answer"`
	Language         string   `json:"language"`
	Status           string   `json:"status"`
	ActiveScenario   string   `json:"active_scenario,omitempty"`
	PendingScenarios []string `json:"pending_scenarios"`
	Trace            Trace    `json:"trace"`
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
}
type Turn struct {
	Input  Input   `json:"input"`
	Output *Output `json:"output,omitempty"`
}
type Session struct {
	ID            string    `json:"session_id"`
	Language      string    `json:"language"`
	Identity      Values    `json:"identity"`
	Active        *Frame    `json:"active,omitempty"`
	LastCompleted *Frame    `json:"last_completed,omitempty"`
	Stack         []*Frame  `json:"stack"`
	Queue         []*Frame  `json:"queue"`
	Turns         []Turn    `json:"turns"`
	LowConfidence int       `json:"low_confidence"`
	UpdatedAt     time.Time `json:"updated_at"`
}
type Model interface {
	Route(context.Context, Input, Session) (Decision, error)
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
