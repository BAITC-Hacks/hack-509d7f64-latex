package router

import "encoding/json"

// routingMessages rebuilds provider context from durable application state. It
// deliberately sends no recursive supervisor trace, no full session snapshot,
// and no pending copy of the current input from the turn table.
func routingMessages(in Input, state Session) ([]any, error) {
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
	messages := make([]any, 0, 2*len(completed)+2+len(state.RoutingContext))
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
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		// Metadata and review feedback are untrusted data, not system directives.
		messages = append(messages, Values{"role": "user", "content": string(encoded)})
		return nil
	}
	if err := appendData(Values{"kind": "workflow_context", "language": state.Language,
		"active": compactFrame(state.Active), "last_completed": compactFrame(state.LastCompleted),
		"suspended": frames(state.Stack), "queued": frames(state.Queue), "identity": state.Identity,
		"low_confidence_turns": state.LowConfidence}); err != nil {
		return nil, err
	}
	if err := appendData(Values{"kind": "current_input", "input": in}); err != nil {
		return nil, err
	}
	for _, event := range state.RoutingContext {
		if err := appendData(Values{"kind": "routing_event", "event": event}); err != nil {
			return nil, err
		}
	}
	return messages, nil
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
	if len(out.Turns) > 10 {
		out.Turns = out.Turns[len(out.Turns)-10:]
	}
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
