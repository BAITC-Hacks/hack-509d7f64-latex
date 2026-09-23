package router

import (
	"math"
	"slices"
	"sort"
)

// Uncertainty weights. The score is the weighted mean of the components that
// apply to the turn, so only ratios matter. They are set against the default
// τ_exec = .25 and τ_handoff = .55:
//   - retrieval together weighs .2 of the model. Retrieval is lexical and misses
//     paraphrases, so a .95 choice that a split shortlist ranks outside its top
//     3 still executes ((.05+.08+.12)/1.2 ≈ .21), while a .6 choice clarifies
//     (.4/1.2 ≈ .33) and a .3 choice counts toward handoff (.7/1.2 ≈ .58) even
//     with retrieval fully agreeing.
//   - a boundary conflict is a flag, not a graded signal: it joins the mean only
//     when it fires, because "no conflict found" is no evidence the choice is
//     right and would dilute the model's vote. It makes a .95 choice clarify on
//     its own (.35/1.3 ≈ .27) or when retrieval disagrees (≥ .27), but not when
//     retrieval clearly agrees ((.05+.3)/1.5 ≈ .23).
const (
	weightModel        = 1.0
	weightMargin       = .08
	weightDisagreement = .12
	weightBoundary     = .3
	// marginScale is the retrieval lead at which the shortlist is decisive.
	marginScale = .3
	// boundaryConfidence is the alternative confidence at which the model itself
	// treats a boundary neighbour as live.
	boundaryConfidence = .5
	// queueConfidence mirrors the engine's bar for queueing a secondary intent
	// (workflow_helpers.go): a secondary below it is dropped, so it counts as
	// an alternative, not as a choice that settles a disagreement or boundary.
	queueConfidence = .75
)

// followUps are the scenarios a completed one hands the dialog to on the next
// turn: only the quote's closing ("Оформим?") continues in another scenario.
// not_this_if lists confusable neighbours, not follow-ups.
var followUps = map[string][]string{"SC01": {"SC02"}}

// assess turns a validated routing decision into an execute / clarify /
// handoff verdict from observable signals: the model's confidence, the
// retrieval shortlist and the dataset's not_this_if boundaries. A continuation
// of a scenario in play skips retrieval_margin and disagreement but keeps the
// boundary check, shortlist included: a neighbour ranked top-2 by the reply
// means the client may have switched topics.
func (e *Engine) assess(d Decision, s *Session, shortlist []ScoredScenario, path string) Uncertainty {
	if path == "bypass" {
		return Uncertainty{Components: map[string]float64{"bypass": 0}, Verdict: "execute"}
	}
	primary := d.Scenarios[0]
	u := Uncertainty{Components: map[string]float64{}}
	sum, weight := 0.0, 0.0
	add := func(name string, w, v float64) {
		v = round3(clamp01(v))
		u.Components[name] = v
		sum += w * v
		weight += w
	}
	add("model", weightModel, 1-primary.Confidence)
	chosen, hedged := []string{}, []Candidate{}
	for i, c := range d.Scenarios {
		if i == 0 || c.Confidence >= queueConfidence {
			chosen = append(chosen, c.ScenarioID)
		} else {
			hedged = append(hedged, c)
		}
	}
	// An all-zero shortlist (only the forced in-play IDs) carries no opinion.
	ranked := slices.Clone(shortlist)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	if len(ranked) == 0 || ranked[0].Score <= 0 {
		ranked = nil
	}
	// Retrieval ranks only a new catalog topic. A continuation such as a bare
	// "В Алматы, легковая" does not retrieve the scenario it continues, and
	// system intents have no examples.
	_, catalogued := e.Catalog.Scenarios[primary.ScenarioID]
	if catalogued && !(d.IsContinuation && inPlay(s, primary.ScenarioID)) && ranked != nil {
		if len(ranked) >= 2 {
			add("retrieval_margin", weightMargin, 1-clamp01((ranked[0].Score-ranked[1].Score)/marginScale))
		}
		add("disagreement", weightDisagreement, disagreement(chosen, ranked))
	}
	if rule, ok := e.boundary(d, chosen, hedged, ranked); ok {
		add("boundary", weightBoundary, 1)
		u.Boundary = rule
	}
	u.Score = round3(sum / weight)
	switch {
	case u.Score <= e.Policy.Execute:
		u.Verdict = "execute"
	case u.Score <= e.Policy.Handoff:
		u.Verdict = "clarify"
	default:
		u.Verdict = "handoff"
	}
	return u
}

// inPlay reports whether a scenario already belongs to the dialog: an active,
// suspended or queued frame, the last completed one or its follow-up.
func inPlay(s *Session, id string) bool {
	if s == nil {
		return false
	}
	if slices.Contains(workflowIDs(s), id) {
		return true
	}
	last := s.LastCompleted
	return last != nil && (last.ScenarioID == id || slices.Contains(followUps[last.ScenarioID], id))
}

// disagreement is 0 when retrieval ranks a chosen scenario first, .5 when in
// its top 3, 1 otherwise, including when a short shortlist omits it. Any
// chosen scenario counts: a multi-intent turn sorts the urgent intent first,
// and retrieval over the whole utterance may rank the other one higher.
func disagreement(chosen []string, ranked []ScoredScenario) float64 {
	for i, c := range ranked[:min(3, len(ranked))] {
		if c.Score <= 0 {
			break
		}
		if slices.Contains(chosen, c.ScenarioID) {
			if i == 0 {
				return 0
			}
			return .5
		}
	}
	return 1
}

// boundary returns the first not_this_if rule of the primary whose use_instead
// scenario is live: retrieval ranks it in its top 2, or the model offers it
// with confidence ≥ .5 as an alternative or as a secondary intent too weak to
// queue. A neighbour the decision also chose is not a conflict.
func (e *Engine) boundary(d Decision, chosen []string, hedged []Candidate, ranked []ScoredScenario) (string, bool) {
	live := map[string]bool{}
	for _, c := range ranked[:min(2, len(ranked))] {
		if c.Score > 0 {
			live[c.ScenarioID] = true
		}
	}
	for _, c := range append(slices.Clone(d.Alternatives), hedged...) {
		if c.Confidence >= boundaryConfidence {
			live[c.ScenarioID] = true
		}
	}
	for _, id := range chosen {
		delete(live, id)
	}
	primary := d.Scenarios[0].ScenarioID
	for _, rule := range e.Catalog.Scenarios[primary].Boundaries {
		if id := str(rule["use_instead"]); live[id] {
			return primary + "→" + id + ": " + str(rule["condition"]), true
		}
	}
	return "", false
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// round3 keeps the trace readable; the verdict compares the rounded score so
// the panel shows exactly the number that crossed (or missed) a threshold.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
