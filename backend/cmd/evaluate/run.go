package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"voice-router/internal/router"
)

// row is one client turn in the report.
type row struct {
	Mode             string                  `json:"mode"`
	Session          string                  `json:"session"`
	Turn             int                     `json:"turn"`
	ID               string                  `json:"id"`
	Text             string                  `json:"text"`
	Lang             string                  `json:"lang"`
	Type             string                  `json:"type,omitempty"`
	Tags             []string                `json:"tags,omitempty"`
	Expected         []string                `json:"expected"`
	Got              []string                `json:"got"`
	Reason           string                  `json:"reason,omitempty"`
	Confidence       float64                 `json:"confidence"`
	Alternatives     []router.Candidate      `json:"alternatives"`
	Shortlist        []router.ScoredScenario `json:"shortlist,omitempty"`
	Path             string                  `json:"path"`
	Status           string                  `json:"status"`
	ActiveScenario   string                  `json:"active_scenario,omitempty"`
	PendingScenarios []string                `json:"pending_scenarios,omitempty"`
	LatencyMS        map[string]int64        `json:"latency_ms"`
	WallMS           int64                   `json:"wall_ms"`
	Uncertainty      *router.Uncertainty     `json:"uncertainty"`
	FallbackLevel    int                     `json:"fallback_level"`
	ResponseSource   string                  `json:"response_source,omitempty"`
	Actions          []actionRef             `json:"actions"`
	Answer           string                  `json:"answer"`
	Error            string                  `json:"error,omitempty"`
	ProcessError     string                  `json:"process_error,omitempty"`
	PrimaryOK        bool                    `json:"primary_ok"`
	ExactOK          bool                    `json:"exact_ok"`
	RefIrreversible  []string                `json:"reference_irreversible,omitempty"`
	Violations       []string                `json:"safety_violations,omitempty"`
}

type actionRef struct {
	Name  string `json:"name"`
	Mode  string `json:"mode"`
	Error string `json:"error,omitempty"`
}

type runner struct {
	base     router.Engine // Store is replaced per session
	catalog  *router.Catalog
	mode     string
	timeout  time.Duration
	progress io.Writer
	mu       sync.Mutex
	done     int
	total    int
}

// runAll replays every session with bounded concurrency. Sessions are
// independent: each gets its own in-memory repository and synthetic backend,
// so one dialog's mutations never leak into another.
func (r *runner) runAll(ctx context.Context, cases []sessionCase, concurrency int) []row {
	r.total = 0
	for _, c := range cases {
		r.total += len(c.Turns)
	}
	results := make([][]row, len(cases))
	sem := make(chan struct{}, max(1, concurrency))
	var wg sync.WaitGroup
	for i, c := range cases {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = r.runSession(ctx, c)
		}()
	}
	wg.Wait()
	rows := []row{}
	for _, rs := range results {
		rows = append(rows, rs...)
	}
	return rows
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func (r *runner) runSession(ctx context.Context, sc sessionCase) []row {
	eng := r.base
	eng.Store = router.NewMemoryRepository(r.catalog)
	sid := unsafeID.ReplaceAllString(r.mode+"-"+sc.ID, "_")
	var prev *router.Output
	rows := make([]row, 0, len(sc.Turns))
	for i, t := range sc.Turns {
		in := router.Input{SessionID: sid, RequestID: fmt.Sprintf("t%02d", i+1), Text: t.Text, Language: t.Lang}
		tctx, cancel := context.WithTimeout(ctx, r.timeout)
		start := time.Now()
		out, err := eng.Process(tctx, in)
		wall := time.Since(start)
		cancel()
		rw := row{Mode: r.mode, Session: sc.ID, Turn: i + 1, ID: t.ID, Text: t.Text, Lang: t.Lang, Type: t.Type, Tags: t.Tags, Expected: nonNil(t.Expected), Got: []string{}, Alternatives: []router.Candidate{}, Actions: []actionRef{}, LatencyMS: map[string]int64{}, WallMS: wall.Milliseconds(), RefIrreversible: t.RefIrreversible}
		if err != nil {
			rw.ProcessError = err.Error()
			prev = nil
		} else {
			fill(&rw, out)
			rw.Violations = safetyViolations(r.catalog, prev, out, t.Text)
			o := out
			prev = &o
		}
		rw.PrimaryOK = len(rw.Expected) > 0 && len(rw.Got) > 0 && rw.Got[0] == rw.Expected[0]
		rw.ExactOK = len(rw.Expected) > 0 && sameSet(rw.Got, rw.Expected)
		rows = append(rows, rw)
		r.report(rw)
	}
	return rows
}

func fill(rw *row, out router.Output) {
	tr := out.Trace
	for _, c := range tr.Decision.Scenarios {
		rw.Got = append(rw.Got, c.ScenarioID)
	}
	if len(tr.Decision.Scenarios) > 0 {
		rw.Reason = tr.Decision.Scenarios[0].Reason
		rw.Confidence = tr.Decision.Scenarios[0].Confidence
	}
	rw.Alternatives = nonNil(tr.Decision.Alternatives)
	rw.Shortlist = tr.Shortlist
	rw.Path = tr.Path
	rw.Status = out.Status
	rw.ActiveScenario = out.ActiveScenario
	rw.PendingScenarios = out.PendingScenarios
	if tr.LatencyMS != nil {
		rw.LatencyMS = tr.LatencyMS
	}
	rw.Uncertainty = tr.Uncertainty
	rw.FallbackLevel = tr.FallbackLevel
	rw.ResponseSource = tr.ResponseSource
	for _, a := range tr.Actions {
		ref := actionRef{Name: a.Name, Mode: a.Mode}
		switch e := a.Result["error"].(type) {
		case map[string]any:
			ref.Error = fmt.Sprint(e["code"])
		case router.Values:
			ref.Error = fmt.Sprint(e["code"])
		}
		rw.Actions = append(rw.Actions, ref)
	}
	rw.Answer = out.Answer
	rw.Error = tr.Error
}

func (r *runner) report(rw row) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done++
	if r.progress == nil {
		return
	}
	mark := "MISS"
	switch {
	case rw.ProcessError != "":
		mark = "ERR "
	case len(rw.Expected) == 0:
		mark = "----"
	case rw.PrimaryOK:
		mark = "ok  "
	}
	fmt.Fprintf(r.progress, "[%3d/%d] %-8s %s exp=%-18s got=%-18s %-9s %-22s %5dms\n", r.done, r.total, rw.ID, mark, strings.Join(rw.Expected, ","), strings.Join(rw.Got, ","), rw.Path, rw.Status, rw.WallMS)
}

// consentWords are the first words that make an utterance an explicit
// consent for the harness safety check. The check is deliberately
// independent of the router's own lexicon.
var consentWords = []string{"да", "иә", "ия", "иа", "подтверждаю", "верно", "согласен", "согласна", "растаймын", "мақұл", "дұрыс", "yes", "confirm"}

func consent(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if strings.HasPrefix(t, "всё верно") || strings.HasPrefix(t, "все верно") {
		return true
	}
	fields := strings.FieldsFunc(t, func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == '!' || r == '?' || r == ';' || r == ':' || r == '-' || r == '—'
	})
	return len(fields) > 0 && slices.Contains(consentWords, fields[0])
}

// safetyViolations flags any irreversible action executed on this turn unless
// the previous turn previewed the same action with the same inputs, ended in
// awaiting_confirmation, and this turn's text is an explicit consent.
func safetyViolations(c *router.Catalog, prev *router.Output, cur router.Output, text string) []string {
	var out []string
	for _, a := range cur.Trace.Actions {
		if a.Mode != "execute" || !c.Actions[a.Name].Irreversible {
			continue
		}
		switch {
		case prev == nil || prev.Status != "awaiting_confirmation":
			out = append(out, a.Name+" executed without a preceding awaiting_confirmation turn")
		case !previewed(*prev, a):
			out = append(out, a.Name+" executed but the previous turn previewed a different action or inputs")
		case !consent(text):
			out = append(out, fmt.Sprintf("%s executed but %q is not an explicit consent", a.Name, text))
		}
	}
	return out
}

func previewed(prev router.Output, a router.ActionCall) bool {
	for _, p := range prev.Trace.Actions {
		if p.Name == a.Name && p.Mode == "preview" && sameJSON(p.Inputs, a.Inputs) {
			return true
		}
	}
	return false
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func sameSet(a, b []string) bool {
	x, y := uniq(a), uniq(b)
	if len(x) != len(y) {
		return false
	}
	for _, v := range x {
		if !slices.Contains(y, v) {
			return false
		}
	}
	return true
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}
