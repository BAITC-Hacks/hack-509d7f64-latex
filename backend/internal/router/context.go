package router

import "encoding/json"

// dataMessage frames metadata, scenario details and review feedback as
// untrusted user-role data, not system directives.
func dataMessage(data any) (Values, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return Values{"role": "user", "content": string(encoded)}, nil
}

// fastRoutingMessages is the fast route's whole context: candidate details and
// the utterance, without history or workflow state.
func fastRoutingMessages(in Input, candidates Values) ([]any, error) {
	messages := []any{}
	for _, data := range []any{candidates, Values{"kind": "current_input", "input": in}} {
		message, err := dataMessage(data)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// routingMessages rebuilds provider context from durable application state. It
// deliberately sends no recursive supervisor trace, no full session snapshot,
// and no pending copy of the current input from the turn table. Candidates,
// which change every turn, go after history so that consecutive turns of a
// session share the longest prompt prefix; nil omits them.
func routingMessages(in Input, state Session, candidates Values) ([]any, error) {
	completed := make([]Turn, 0, len(state.Turns))
	for _, turn := range state.Turns {
		if turn.Output == nil || (in.RequestID != "" && turn.Input.RequestID == in.RequestID) {
			continue
		}
		completed = append(completed, turn)
	}
	if len(completed) > 10 {
		completed = completed[len(completed)-10:]
	}
	messages := make([]any, 0, 2*len(completed)+3+len(state.RoutingContext))
	for _, turn := range completed {
		messages = append(messages, Values{"role": "user", "content": turn.Input.Text})
		if turn.Output.Answer != "" {
			messages = append(messages, Values{"role": "assistant", "content": turn.Output.Answer})
		}
	}
	frames := func(items []*Frame) []any {
		out := make([]any, 0, len(items))
		for _, frame := range items {
			out = append(out, compactFrame(frame))
		}
		return out
	}
	appendData := func(data any) error {
		message, err := dataMessage(data)
		if err != nil {
			return err
		}
		messages = append(messages, message)
		return nil
	}
	if err := appendData(Values{"kind": "workflow_context", "language": state.Language,
		"active": compactFrame(state.Active), "last_completed": compactFrame(state.LastCompleted),
		"suspended": frames(state.Stack), "queued": frames(state.Queue), "identity": state.Identity,
		"low_confidence_turns": state.LowConfidence}); err != nil {
		return nil, err
	}
	if candidates != nil {
		if err := appendData(candidates); err != nil {
			return nil, err
		}
	}
	if err := appendData(Values{"kind": "current_input", "input": in}); err != nil {
		return nil, err
	}
	for _, event := range state.RoutingContext {
		if err := appendData(Values{"kind": "routing_event", "event": compactRoutingEvent(event)}); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func compactRoutingEvent(event Values) Values {
	out := clone(event)
	// Native provider items are retained for the in-flight tool round trip and
	// durable trace. A rebuilt context only needs the human-readable retrieval
	// result, not opaque reasoning or another JSON-string copy of that result.
	for _, data := range []Values{out, asMap(out["data"])} {
		delete(data, "provider_output")
		delete(data, "tool_output")
	}
	return out
}

// Do not recursively resend supervisor traces and handoff payloads to the LLM.
func history(turns []Turn) []Values {
	if len(turns) > 10 {
		turns = turns[len(turns)-10:]
	}
	out := []Values{}
	for _, t := range turns {
		v := Values{"text": t.Input.Text, "language": t.Input.Language}
		if t.Output != nil {
			v["answer"] = t.Output.Answer
			v["status"] = t.Output.Status
			v["scenarios"] = t.Output.Trace.Decision.Scenarios
		}
		out = append(out, v)
	}
	return out
}
func compactFrame(f *Frame) any {
	if f == nil {
		return nil
	}
	return Values{"scenario_id": f.ScenarioID, "slots": clone(f.Slots), "next_action": f.NextAction, "pending": clone(f.Pending)}
}
func modelState(s Session) Session {
	out := clone(s)
	// routingMessages applies the ten-completed-turn bound after removing the
	// active request; truncating here could evict an extra completed turn.
	for i := range out.Turns {
		if o := out.Turns[i].Output; o != nil {
			o.Trace.Actions = nil
		}
	}
	frames := append(append([]*Frame{}, out.Stack...), out.Queue...)
	if out.Active != nil {
		frames = append(frames, out.Active)
	}
	if out.LastCompleted != nil {
		frames = append(frames, out.LastCompleted)
	}
	for _, f := range frames {
		for i := range f.Results {
			delete(f.Results[i].Inputs, "context")
			delete(f.Results[i].Result, "context")
		}
	}
	return out
}
