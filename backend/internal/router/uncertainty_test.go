package router

import (
	"maps"
	"slices"
	"testing"
)

func assessEngine(t *testing.T) *Engine {
	t.Helper()
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Catalog: c, Policy: DefaultPolicy()}
}
func routed(id string, confidence float64, alternatives ...Candidate) Decision {
	return Decision{Scenarios: []Candidate{{ScenarioID: id, Confidence: confidence}}, Alternatives: alternatives, Language: "ru"}
}
func ranking(pairs ...any) []ScoredScenario {
	out := []ScoredScenario{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, ScoredScenario{ScenarioID: pairs[i].(string), Score: pairs[i+1].(float64)})
	}
	return out
}

func TestAssess(t *testing.T) {
	cont := func(d Decision) Decision { d.IsContinuation = true; return d }
	multi := routed("SC38", .99)
	multi.Scenarios = append(multi.Scenarios, Candidate{ScenarioID: "SC33", Confidence: .95})
	both := routed("SC17", .95)
	both.Scenarios = append(both.Scenarios, Candidate{ScenarioID: "SC19", Confidence: .9})
	// A secondary below the queue bar is dropped by the engine, so it must not
	// settle a disagreement or hide a boundary neighbour.
	hedge := func(confidence float64) Decision {
		d := routed("SC17", .95)
		d.Scenarios = append(d.Scenarios, Candidate{ScenarioID: "SC19", Confidence: confidence})
		return d
	}
	active := &Session{Active: &Frame{ScenarioID: "SC01"}}
	cases := []struct {
		name      string
		d         Decision
		s         *Session
		shortlist []ScoredScenario
		path      string
		verdict   string
		keys      []string
		boundary  string
	}{
		{name: "confident agreement with clear favourite", d: routed("SC17", .95), shortlist: ranking("SC17", .8, "SC05", .3), path: "full",
			verdict: "execute", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "confident, disagrees, neighbour ranked first", d: routed("SC17", .95), shortlist: ranking("SC19", .7, "SC17", .6, "SC05", .2), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement", "boundary"}, boundary: "SC17→SC19: Client disagrees with the decision or the amount"},
		{name: "confident, outside top 3, neighbour clear favourite", d: routed("SC17", .95), shortlist: ranking("SC19", .9, "SC05", .3, "SC06", .2, "SC07", .1), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement", "boundary"}, boundary: "SC17→SC19: Client disagrees with the decision or the amount"},
		{name: "confident, disagrees, no boundary: paraphrase", d: routed("SC17", .95), shortlist: ranking("SC05", .7, "SC06", .3, "SC07", .2), path: "full",
			verdict: "execute", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "absent from single-entry shortlist", d: routed("SC33", .8), shortlist: ranking("SC05", .7), path: "full",
			verdict: "clarify", keys: []string{"model", "disagreement"}},
		{name: "absent from two-entry shortlist", d: routed("SC33", .8), shortlist: ranking("SC05", .7, "SC06", .1), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "absent from four-entry shortlist", d: routed("SC33", .8), shortlist: ranking("SC05", .7, "SC06", .1, "SC07", .1, "SC08", .1), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "confident, absent from single-entry shortlist: paraphrase", d: routed("SC33", .95), shortlist: ranking("SC05", .7), path: "full",
			verdict: "execute", keys: []string{"model", "disagreement"}},
		{name: "confident, disagrees with split shortlist", d: routed("SC17", .95), shortlist: ranking("SC05", .5, "SC06", .5, "SC07", .5), path: "full",
			verdict: "execute", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "confident agreement, neighbour second but far", d: routed("SC17", .95), shortlist: ranking("SC17", .8, "SC19", .4), path: "full",
			verdict: "execute", keys: []string{"model", "retrieval_margin", "disagreement", "boundary"}, boundary: "SC17→SC19: Client disagrees with the decision or the amount"},
		{name: "medium confidence despite agreement", d: routed("SC29", .6), shortlist: ranking("SC29", .9, "SC06", .1), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "medium confidence, empty shortlist", d: routed("SC29", .6), shortlist: nil, path: "full",
			verdict: "clarify", keys: []string{"model"}},
		{name: "low confidence despite agreement", d: routed("SC25", .3), shortlist: ranking("SC25", .9, "SC05", .1), path: "full",
			verdict: "handoff", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "low confidence, empty shortlist", d: routed("SC25", .3), shortlist: nil, path: "full",
			verdict: "handoff", keys: []string{"model"}},
		{name: "fast path, single scored candidate", d: routed("SC33", .9), shortlist: ranking("SC33", .7), path: "fast",
			verdict: "execute", keys: []string{"model", "disagreement"}},
		{name: "zero-score shortlist carries no opinion", d: routed("SC02", .95), s: active, shortlist: ranking("SC01", 0.0), path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "unsorted shortlist is ranked", d: routed("SC17", .95), shortlist: ranking("SC05", .1, "SC17", .9), path: "fast>full",
			verdict: "execute", keys: []string{"model", "retrieval_margin", "disagreement"}},

		{name: "continuation of active frame skips retrieval", d: cont(routed("SC01", .95)), s: active, shortlist: ranking("SC33", .8, "SC04", .6), path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "continuation of suspended frame", d: cont(routed("SC01", .95)), s: &Session{Active: &Frame{ScenarioID: "SC33"}, Stack: []*Frame{{ScenarioID: "SC01"}}}, shortlist: ranking("SC33", .8, "SC05", .1), path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "continuation of queued frame", d: cont(routed("SC33", .95)), s: &Session{Queue: []*Frame{{ScenarioID: "SC33"}}}, shortlist: ranking("SC05", .8, "SC06", .1), path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "quote to purchase follows last completed", d: cont(routed("SC02", .95)), s: &Session{LastCompleted: &Frame{ScenarioID: "SC01"}}, shortlist: ranking("SC06", .15, "SC26", .07, "SC02", .067), path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "continuation of last completed itself", d: cont(routed("SC05", .95)), s: &Session{LastCompleted: &Frame{ScenarioID: "SC05"}}, shortlist: ranking("SC33", .8, "SC06", .6), path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "last completed boundary neighbour is not a follow-up", d: cont(routed("SC19", .95)), s: &Session{LastCompleted: &Frame{ScenarioID: "SC17"}}, shortlist: ranking("SC17", .8, "SC19", .6), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement", "boundary"}, boundary: "SC19→SC17: Client only asks the status"},
		{name: "accident to victim claim is not a follow-up", d: cont(routed("SC12", .95)), s: &Session{LastCompleted: &Frame{ScenarioID: "SC11"}}, shortlist: ranking("SC11", .8, "SC12", .6), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement", "boundary"}, boundary: "SC12→SC11: Accident is happening right now"},
		{name: "continuation flag on a new scenario is a new topic", d: cont(routed("SC17", .95)), s: active, shortlist: ranking("SC19", .7, "SC17", .6), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement", "boundary"}, boundary: "SC17→SC19: Client disagrees with the decision or the amount"},
		{name: "continuation still checks model alternatives", d: cont(routed("SC01", .95, Candidate{ScenarioID: "SC02", Confidence: .6})), s: active, shortlist: ranking("SC05", .8), path: "full",
			verdict: "clarify", keys: []string{"model", "boundary"}, boundary: "SC01→SC02: Client already decided and asks to issue or buy the policy"},
		{name: "continuation still checks boundary neighbour in shortlist", d: cont(routed("SC01", .95)), s: active, shortlist: ranking("SC02", .8, "SC01", .2), path: "full",
			verdict: "clarify", keys: []string{"model", "boundary"}, boundary: "SC01→SC02: Client already decided and asks to issue or buy the policy"},
		{name: "continuation ignores neighbour ranked third", d: cont(routed("SC01", .95)), s: active, shortlist: ranking("SC01", .2, "SC33", .16, "SC02", .1), path: "full",
			verdict: "execute", keys: []string{"model"}},

		{name: "system intent skips retrieval", d: routed("SYS_GOODBYE", .95), shortlist: ranking("SC05", .9, "SC06", .1), path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "unsure system intent clarifies", d: routed("SYS_OUT_OF_SCOPE", .6), shortlist: ranking("SC05", .9), path: "full",
			verdict: "clarify", keys: []string{"model"}},

		{name: "boundary from alternative, no shortlist", d: routed("SC17", .95, Candidate{ScenarioID: "SC18", Confidence: .5}), shortlist: nil, path: "full",
			verdict: "clarify", keys: []string{"model", "boundary"}, boundary: "SC17→SC18: Client asks which documents are needed"},
		{name: "weak alternative is not live", d: routed("SC17", .95, Candidate{ScenarioID: "SC19", Confidence: .49}), shortlist: nil, path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "non-neighbour alternative is not a boundary", d: routed("SC17", .95, Candidate{ScenarioID: "SC05", Confidence: .9}), shortlist: nil, path: "full",
			verdict: "execute", keys: []string{"model"}},
		{name: "neighbour chosen too is not a conflict", d: both, shortlist: ranking("SC19", .7, "SC17", .6), path: "full",
			verdict: "execute", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "weak secondary neighbour stays live in shortlist", d: hedge(.2), shortlist: ranking("SC19", .7, "SC17", .6, "SC05", .2), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement", "boundary"}, boundary: "SC17→SC19: Client disagrees with the decision or the amount"},
		{name: "weak secondary does not settle disagreement", d: hedge(.2), shortlist: ranking("SC19", .9, "SC05", .3, "SC06", .2, "SC07", .1), path: "full",
			verdict: "clarify", keys: []string{"model", "retrieval_margin", "disagreement", "boundary"}, boundary: "SC17→SC19: Client disagrees with the decision or the amount"},
		{name: "hedged secondary neighbour acts as alternative", d: hedge(.6), shortlist: nil, path: "full",
			verdict: "clarify", keys: []string{"model", "boundary"}, boundary: "SC17→SC19: Client disagrees with the decision or the amount"},
		{name: "secondary at queue bar is chosen", d: hedge(.75), shortlist: ranking("SC19", .7, "SC17", .6), path: "full",
			verdict: "execute", keys: []string{"model", "retrieval_margin", "disagreement"}},
		{name: "multi-intent agreement on second intent", d: multi, shortlist: ranking("SC33", .8, "SC38", .5), path: "full",
			verdict: "execute", keys: []string{"model", "retrieval_margin", "disagreement"}},

		{name: "bypass", d: routed("SC01", .1), s: active, shortlist: ranking("SC05", .9), path: "bypass",
			verdict: "execute", keys: []string{"bypass"}},
		{name: "empty path treated as routed", d: routed("SC25", .3), path: "",
			verdict: "handoff", keys: []string{"model"}},
	}
	e := assessEngine(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			if s == nil {
				s = &Session{}
			}
			u := e.assess(c.d, s, c.shortlist, c.path)
			t.Logf("score %.3f %s %v", u.Score, u.Verdict, u.Components)
			if u.Verdict != c.verdict {
				t.Errorf("verdict %s, want %s (%+v)", u.Verdict, c.verdict, u)
			}
			if keys := slices.Sorted(maps.Keys(u.Components)); !slices.Equal(keys, slices.Sorted(slices.Values(c.keys))) {
				t.Errorf("components %v, want %v", keys, c.keys)
			}
			if u.Boundary != c.boundary {
				t.Errorf("boundary %q, want %q", u.Boundary, c.boundary)
			}
			for k, v := range u.Components {
				if v < 0 || v > 1 {
					t.Errorf("component %s = %v outside [0,1]", k, v)
				}
			}
			if u.Score < 0 || u.Score > 1 {
				t.Errorf("score %v outside [0,1]", u.Score)
			}
		})
	}
}

func TestAssessComponentValues(t *testing.T) {
	e := assessEngine(t)
	u := e.assess(routed("SC17", .95), &Session{}, ranking("SC19", .7, "SC17", .6, "SC05", .2), "full")
	want := map[string]float64{"model": .05, "retrieval_margin": .667, "disagreement": .5, "boundary": 1}
	if !maps.Equal(u.Components, want) {
		t.Fatalf("components %v, want %v", u.Components, want)
	}
	// (1·.05 + .08·.667 + .12·.5 + .3·1) / 1.5
	if u.Score != .309 {
		t.Fatalf("score %v", u.Score)
	}
	if u := e.assess(routed("SC17", .95), &Session{}, ranking("SC05", .9, "SC06", .1, "SC07", .05, "SC08", .01), "full"); u.Components["disagreement"] != 1 || u.Components["retrieval_margin"] != 0 {
		t.Fatalf("outside top 3 against a clear favourite: %v", u.Components)
	}
	if u := e.assess(routed("SC17", .95), &Session{}, ranking("SC05", .9, "SC06", .1, "SC17", .05), "full"); u.Components["disagreement"] != .5 {
		t.Fatalf("third place: %v", u.Components)
	}
	for _, shortlist := range [][]ScoredScenario{ranking("SC05", .7), ranking("SC05", .7, "SC06", .1), ranking("SC05", .7, "SC33", 0.0)} {
		if u := e.assess(routed("SC33", .8), &Session{}, shortlist, "full"); u.Components["disagreement"] != 1 {
			t.Fatalf("short shortlist without the primary %v: %v", shortlist, u.Components)
		}
	}
	// Single-entry: (1·.2 + .12·1) / 1.12
	if u := e.assess(routed("SC33", .8), &Session{}, ranking("SC05", .7), "full"); u.Score != .286 {
		t.Fatalf("single-entry score %v", u.Score)
	}
	hedged := routed("SC17", .95)
	hedged.Scenarios = append(hedged.Scenarios, Candidate{ScenarioID: "SC19", Confidence: .2})
	if u := e.assess(hedged, &Session{}, ranking("SC19", .7, "SC17", .6, "SC05", .2), "full"); u.Score != .309 {
		t.Fatalf("weak secondary neighbour scored %v, want the plain decision's .309", u.Score)
	}
	u = e.assess(routed("SC01", .2), &Session{}, nil, "bypass")
	if u.Score != 0 || u.Verdict != "execute" || u.Boundary != "" || !maps.Equal(u.Components, map[string]float64{"bypass": 0}) {
		t.Fatalf("bypass: %+v", u)
	}
}

func TestAssessThresholdsFromPolicy(t *testing.T) {
	e := assessEngine(t)
	cases := []struct {
		confidence float64
		verdict    string
	}{
		{.75, "execute"}, // score .25 == τ_exec
		{.749, "clarify"},
		{.45, "clarify"}, // score .55 == τ_handoff
		{.449, "handoff"},
	}
	for _, c := range cases {
		if u := e.assess(routed("SC33", c.confidence), &Session{}, nil, "full"); u.Verdict != c.verdict {
			t.Errorf("confidence %v: %+v, want %s", c.confidence, u, c.verdict)
		}
	}
	e.Policy.Execute, e.Policy.Handoff = .01, .02
	if u := e.assess(routed("SC33", .95), &Session{}, nil, "full"); u.Verdict != "handoff" {
		t.Fatalf("thresholds not read from policy: %+v", u)
	}
}

func TestAssessLeavesInputsUntouched(t *testing.T) {
	e := assessEngine(t)
	shortlist := ranking("SC05", .1, "SC17", .9)
	e.assess(routed("SC17", .95), &Session{}, shortlist, "full")
	if shortlist[0].ScenarioID != "SC05" {
		t.Fatal("assess reordered the trace shortlist")
	}
}
