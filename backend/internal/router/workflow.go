package router

import (
	"fmt"
	"strings"
	"time"
)

func (w *work) prepare() error {
	if w.r.FrameReady {
		w.r.Phase = "executing"
		return w.save("workflow_resumed", nil)
	}
	d := *w.r.Decision
	w.e.selectFrames(w.s, d)
	f := w.s.Active
	sc := w.e.Catalog.Scenarios[f.ScenarioID]
	changed := false
	for k, v := range d.Slots {
		if old, ok := f.Slots[k]; ok && !equalJSON(old, v) {
			changed = true
		}
	}
	if changed {
		f.Pending = nil
		mutated := false
		for _, call := range f.Results {
			if w.e.Catalog.Actions[call.Name].Irreversible && errorCode(call.Result) == "" {
				mutated = true
			}
		}
		if !mutated {
			f.NextAction = 0
			f.Results = []ActionCall{}
		}
	}
	for _, key := range []string{"phone", "iin"} {
		if v, ok := d.Slots[key]; ok && has(w.s.Identity, key) && w.s.Identity[key] != v {
			w.s.Identity = Values{}
			w.s.LastCompleted = nil
			f.Slots = Values{}
			f.NextAction = 0
			f.Pending = nil
			f.Results = nil
			w.s.Stack = nil
			w.s.Queue = nil
			break
		}
	}
	w.e.fillSlots(f, d.Slots)
	f.Awaiting = nil
	for k, v := range w.s.Identity {
		if !has(f.Slots, k) {
			f.Slots[k] = v
		}
	}
	if f.Pending != nil {
		args := w.e.arguments(w.s, f, sc, f.Pending.Action)
		if !equalJSON(args, f.Pending.Inputs) {
			f.Pending = nil
		} else if explicitYes(w.r.Input.Text) && d.IsContinuation {
			w.r.ActionApproved = true
		} else if explicitNo(w.r.Input.Text) && d.IsContinuation {
			f.Pending = nil
			w.s.Active = nil
			return w.finish(local(w.s.Language, "Действие отменено. Чем ещё могу помочь?", "Әрекет тоқтатылды. Тағы қалай көмектесе аламын?"), "cancelled")
		}
	}
	w.r.FrameReady = true
	w.r.Phase = "executing"
	if sc.Priority == "urgent" && sc.Handoff != nil && ((sc.ID == "SC11" && (f.Slots["injured"] == true || d.NeedsHandoff)) || (sc.ID == "SC15" && d.NeedsHandoff) || (sc.ID == "SC38" && d.NeedsHandoff)) {
		detail := sc.ID + ": needs_handoff (" + sc.Handoff.When + ")"
		if sc.ID == "SC11" && f.Slots["injured"] == true {
			detail = sc.ID + ": injured"
		}
		return w.handoff(ReasonUrgentScenario, detail, sc.ID, "workflow_prepared", Values{"scenario_id": sc.ID})
	}
	return w.save("workflow_prepared", Values{"scenario_id": sc.ID})
}

// tool saves its operation identity before execution. The repository performs
// the mutation, receipt and this callback's workflow advancement atomically.
func (w *work) tool(name string, args Values, apply func(Values)) error {
	w.r.Steps++
	if err := w.save("tool_started", Values{"name": name, "operation": w.r.ToolSerial}); err != nil {
		return err
	}
	ctx, cancel := w.dbContext()
	defer cancel()
	start := time.Now()
	op := fmt.Sprintf("operation-%06d", w.r.ToolSerial)
	return w.l.Tool(ctx, w.s, w.r, op, name, args, func(result Values) {
		call := ActionCall{Name: name, Mode: "execute", Inputs: clone(args), Result: clone(result)}
		w.r.Output.Trace.Actions = append(w.r.Output.Trace.Actions, call)
		w.r.LastTool = &call
		w.r.ToolSerial++
		w.r.Output.Trace.LatencyMS["tools"] += time.Since(start).Milliseconds()
		w.account()
		apply(result)
	})
}
func (w *work) execute() error {
	f := w.s.Active
	if f == nil {
		return fmt.Errorf("%w: missing active workflow", ErrConflict)
	}
	sc := w.e.Catalog.Scenarios[f.ScenarioID]
	if sc.Identify && !has(w.s.Identity, "client_id") {
		if !has(f.Slots, "phone") && !has(f.Slots, "iin") {
			return w.ask(f, w.e.question(w.s, f, "phone"), "phone", "iin")
		}
		return w.tool("find_client", Values{"phone": f.Slots["phone"], "iin": f.Slots["iin"]}, func(result Values) {
			if errorCode(result) != "" {
				f.Failures["identify"]++
				delete(f.Slots, "phone")
				delete(f.Slots, "iin")
				w.r.Phase = "tool_error"
				return
			}
			w.s.Identity = clone(result)
			merge(f.Slots, result)
			w.r.Phase = "executing"
		})
	}
	if sc.Identify && has(w.s.Identity, "client_id") && !has(f.Slots, "policy_number") && f.Slots["_policies_loaded"] != true {
		return w.tool("get_policies", Values{"client_id": w.s.Identity["client_id"]}, func(result Values) {
			f.Slots["_policies_loaded"] = true
			product := map[string]string{"SC13": "casco", "SC14": "property", "SC15": "travel", "SC16": "accident", "SC21": "dms", "SC22": "dms", "SC24": "dms", "SC04": "ogpo", "SC05": "ogpo"}[sc.ID]
			matches := []Values{}
			for _, row := range list(result["policies"]) {
				p := asMap(row)
				if product == "" || p["product"] == product {
					matches = append(matches, p)
				}
			}
			if len(matches) == 1 {
				f.Slots["policy_number"] = matches[0]["policy_number"]
			}
			w.r.Phase = "executing"
		})
	}
	for _, slot := range sc.Slots.Required {
		if !has(f.Slots, slot) {
			question := w.e.question(w.s, f, slot)
			if sc.Priority == "urgent" {
				f.Awaiting = []string{slot}
				return w.queueAnswer(Values{"purpose": "give immediate safety guidance, then ask only this question", "question": question, "scenario": sc, "knowledge": w.e.Catalog.KB}, question, "awaiting_slot", false)
			}
			return w.ask(f, question, slot)
		}
	}
	if f.NextAction >= len(sc.Actions) {
		return w.queueAnswer(Values{"purpose": "answer from executed actions", "scenario": sc, "slots": f.Slots, "actions": f.Results, "utterance": w.r.Input.Text, "pending_scenarios": pendingIDs(w.s)}, local(w.s.Language, "Запрос обработан; результаты сохранены.", "Сұрау өңделді; нәтижелер сақталды."), "completed", true)
	}
	name := sc.Actions[f.NextAction]
	if name == "transfer_to_operator" {
		// The scenario's own handoff rule goes through the common handoff so
		// it opens the same operator ticket as every other trigger.
		if ok, detail := w.e.shouldHandoff(sc, *w.r.Decision, f); ok {
			reason := ReasonScenarioHandoff
			if sc.ID == "SC37" {
				reason = ReasonOperatorRequested
			}
			return w.handoff(reason, sc.ID+": "+detail, sc.ID, "scenario_handoff", Values{"scenario_id": sc.ID})
		}
	}
	if (name == "find_client" && has(w.s.Identity, "client_id")) || (name == "send_sms" && !has(f.Slots, "phone")) || name == "transfer_to_operator" {
		f.NextAction++
		return w.save("action_skipped", Values{"name": name})
	}
	args := w.e.arguments(w.s, f, sc, name)
	action := w.e.Catalog.Actions[name]
	for _, group := range action.Inputs {
		present := false
		for _, slot := range strings.Split(group, "|") {
			present = present || has(args, slot)
		}
		if !present {
			return w.ask(f, w.e.question(w.s, f, strings.Split(group, "|")[0]), strings.Split(group, "|")...)
		}
	}
	if action.Irreversible && !w.r.ActionApproved {
		f.Pending = &Pending{Action: name, Inputs: clone(args)}
		w.r.Output.Trace.Actions = append(w.r.Output.Trace.Actions, ActionCall{Name: name, Mode: "preview", Inputs: clone(args), Result: Values{"confirmation_required": true}})
		return w.finish(w.e.confirmation(w.s, sc, name, args), "awaiting_confirmation")
	}
	return w.tool(name, args, func(result Values) {
		f.Results = append(f.Results, *clone(w.r.LastTool))
		if errorCode(result) != "" {
			f.Failures[name]++
			f.Pending = nil
			w.r.ActionApproved = false
			w.r.Phase = "tool_error"
			return
		}
		if name == "find_client" {
			w.s.Identity = clone(result)
			merge(f.Slots, result)
		}
		for _, key := range []string{"policy_number", "claim_number", "price", "bm_class", "client_id"} {
			if has(result, key) && (!has(f.Slots, key) || key == "price" || key == "bm_class" || name == "create_policy" || name == "renew_policy" || name == "create_claim") {
				f.Slots[key] = result[key]
			}
		}
		if name == "get_policy" {
			if !has(f.Slots, "product_type") {
				f.Slots["product_type"] = result["product"]
			}
			if has(result, "premium") {
				f.Slots["price"] = result["premium"]
			}
		}
		if name == "get_policies" {
			rows := list(result["policies"])
			if len(rows) == 1 {
				f.Slots["policy_number"] = asMap(rows[0])["policy_number"]
			}
		}
		if name == "update_contact" {
			w.s.Identity[str(args["contact_field"])] = args["new_value"]
		}
		f.NextAction++
		f.Pending = nil
		w.r.ActionApproved = false
		w.r.Phase = "executing"
	})
}
func (w *work) toolError() error {
	call := w.r.LastTool
	f := w.s.Active
	code := errorCode(call.Result)
	if call.Name == "find_client" && !has(w.s.Identity, "client_id") {
		if f.Failures["identify"] >= 2 {
			return w.handoff(ReasonIdentificationFailed, fmt.Sprintf("find_client failed %d times: %s", f.Failures["identify"], code), "", "identification_failed", nil)
		}
		return w.ask(f, local(w.s.Language, "Клиент не найден. Назовите ИИН или проверьте номер телефона.", "Клиент табылмады. ЖСН-ді айтыңыз немесе телефон нөмірін тексеріңіз."), "phone", "iin")
	}
	if code == "service_unavailable" && f.Failures[call.Name] == 1 {
		w.r.ActionApproved = w.e.Catalog.Actions[call.Name].Irreversible
		w.r.Phase = "executing"
		return w.save("tool_retry", nil)
	}
	if f.Failures[call.Name] >= 2 || code == "service_unavailable" {
		detail := fmt.Sprintf("%s failed %d times: %s", call.Name, f.Failures[call.Name], code)
		return w.handoff(ReasonToolFailed, detail, "", "tool_failed", Values{"name": call.Name, "code": code})
	}
	if code == "not_found" {
		for _, key := range []string{"policy_number", "claim_number", "vehicle_plate"} {
			delete(f.Slots, key)
		}
	}
	f.Awaiting = nil
	return w.queueAnswer(Values{"purpose": "explain failure and ask for corrected data or offer operator", "action": call}, local(w.s.Language, "Не удалось выполнить действие. Уточните данные или попросите оператора.", "Әрекет орындалмады. Деректерді нақтылаңыз немесе операторды сұраңыз."), "awaiting_slot", false)
}

// ask finishes the turn with a slot question and remembers which slots it
// asked for, so a bare answer next turn can continue without a model call.
func (w *work) ask(f *Frame, question string, slots ...string) error {
	if f != nil {
		f.Awaiting = slots
	}
	return w.finish(question, "awaiting_slot")
}

// queueAnswer answers from a catalog template when one covers the facts, and
// otherwise schedules LLM wording with a deterministic fallback.
func (w *work) queueAnswer(facts Values, fallback, status string, complete bool) error {
	if text, ok := w.e.template(w.s, facts, status); ok {
		w.r.Output.Trace.ResponseSource = "template"
		if complete {
			w.s.LastCompleted = clone(w.s.Active)
			w.s.Active = nil
		}
		return w.finish(text, status)
	}
	w.r.AnswerFacts = facts
	w.r.Fallback = fallback
	w.r.FinalStatus = status
	w.r.CompleteFrame = complete
	w.r.Phase = "generating_answer"
	return w.save("answer_requested", Values{"status": status})
}
func (w *work) generate() error {
	if err := w.beforeModel("response_started"); err != nil {
		return err
	}
	start := time.Now()
	answer, err := w.e.Model.Respond(w.ctx, w.s.Language, w.r.AnswerFacts)
	w.r.Output.Trace.LatencyMS["response"] += time.Since(start).Milliseconds()
	w.afterModel()
	w.r.Output.Trace.ResponseSource = "llm"
	if err != nil || strings.TrimSpace(answer) == "" {
		w.r.Output.Trace.Error = "response generation failed"
		w.r.Output.Trace.ResponseSource = "fallback"
		answer = w.r.Fallback
	}
	if w.r.CompleteFrame {
		w.s.LastCompleted = clone(w.s.Active)
		w.s.Active = nil
	}
	return w.finish(answer, w.r.FinalStatus)
}

// transfer (the single handoff executor) lives in operator.go.
