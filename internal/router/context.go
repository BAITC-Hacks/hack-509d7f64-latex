package router

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
	for _, f := range frames {
		for i := range f.Results {
			delete(f.Results[i].Inputs, "context")
			delete(f.Results[i].Result, "context")
		}
	}
	return out
}
