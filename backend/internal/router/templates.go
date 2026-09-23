package router

// template renders a deterministic answer from catalog responses for the
// given facts and status. Stub: always defers to LLM wording.
func (e *Engine) template(s *Session, facts Values, status string) (string, bool) {
	return "", false
}
