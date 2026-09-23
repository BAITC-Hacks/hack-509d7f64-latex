package router

// preRoute recognises, without a model call, a continuation of the scenario
// the model chose on an earlier turn. Stub: always defers to the model.
func (e *Engine) preRoute(s *Session, in Input) (Decision, bool) {
	return Decision{}, false
}
