package router

// assess turns a routing decision into an execute / clarify / handoff verdict.
// Stub: the model's confidence alone, matching the previous 0.75 / 0.45 cuts.
func (e *Engine) assess(d Decision, s *Session, shortlist []ScoredScenario, path string) Uncertainty {
	c := d.Scenarios[0].Confidence
	u := Uncertainty{Score: 1 - c, Components: map[string]float64{"model": 1 - c}, Verdict: "execute"}
	if path == "bypass" {
		u.Score, u.Components["model"] = 0, 0
	}
	if u.Score > e.Policy.Execute {
		u.Verdict = "clarify"
	}
	if u.Score > e.Policy.Handoff {
		u.Verdict = "handoff"
	}
	return u
}
