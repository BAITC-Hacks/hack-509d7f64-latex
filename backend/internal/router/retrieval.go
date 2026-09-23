package router

// Retriever ranks catalog scenarios by similarity to an utterance. Stub:
// returns only the forced IDs, so the model reads the full catalog.
type Retriever struct {
	catalog *Catalog
}

func NewRetriever(c *Catalog) *Retriever { return &Retriever{catalog: c} }

// Shortlist returns up to k scenarios by descending score, plus every forced
// ID (scenarios already in play) even when it falls outside the top k.
func (r *Retriever) Shortlist(text string, k int, forced ...string) []ScoredScenario {
	out := []ScoredScenario{}
	seen := map[string]bool{}
	for _, id := range forced {
		if _, ok := r.catalog.Scenarios[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, ScoredScenario{ScenarioID: id})
		}
	}
	return out
}
