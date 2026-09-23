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
		w.r.Phase = "handoff"
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
			return w.finish(w.e.question(w.s, f, "phone"), "awaiting_slot")
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
				return w.queueAnswer(Values{"purpose": "give immediate safety guidance, then ask only this question", "question": question, "scenario": sc, "knowledge": w.e.Catalog.KB}, question, "awaiting_slot", false)
			}
			return w.finish(question, "awaiting_slot")
		}
	}
	if f.NextAction >= len(sc.Actions) {
		return w.queueAnswer(Values{"purpose": "answer from executed actions", "scenario": sc, "slots": f.Slots, "actions": f.Results, "utterance": w.r.Input.Text, "pending_scenarios": pendingIDs(w.s)}, local(w.s.Language, "Запрос обработан; результаты сохранены.", "Сұрау өңделді; нәтижелер сақталды."), "completed", true)
	}
	name := sc.Actions[f.NextAction]
	if (name == "find_client" && has(w.s.Identity, "client_id")) || (name == "send_sms" && !has(f.Slots, "phone")) || (name == "transfer_to_operator" && !w.e.shouldHandoff(sc, *w.r.Decision, f)) {
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
			return w.finish(w.e.question(w.s, f, strings.Split(group, "|")[0]), "awaiting_slot")
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
		if name == "transfer_to_operator" {
			w.r.FinalStatus = "handoff"
			w.r.Fallback = local(w.s.Language, "Передаю разговор профильному оператору с контекстом.", "Әңгімені мәліметтерімен бірге тиісті операторға беремін.")
			w.r.AnswerFacts = Values{"purpose": "confirm handoff", "action": w.r.LastTool}
			w.r.Phase = "generating_answer"
		}
	})
}
func (w *work) toolError() error {
	call := w.r.LastTool
	f := w.s.Active
	code := errorCode(call.Result)
	if call.Name == "find_client" && !has(w.s.Identity, "client_id") {
		if f.Failures["identify"] >= 2 {
			w.r.Phase = "handoff"
			return w.save("identification_failed", nil)
		}
		return w.finish(local(w.s.Language, "Клиент не найден. Назовите ИИН или проверьте номер телефона.", "Клиент табылмады. ЖСН-ді айтыңыз немесе телефон нөмірін тексеріңіз."), "awaiting_slot")
	}
	if code == "service_unavailable" && f.Failures[call.Name] == 1 {
		w.r.ActionApproved = w.e.Catalog.Actions[call.Name].Irreversible
		w.r.Phase = "executing"
		return w.save("tool_retry", nil)
	}
	if f.Failures[call.Name] >= 2 || code == "service_unavailable" {
		w.r.Phase = "handoff"
		return w.save("tool_failed", nil)
	}
	if code == "not_found" {
		for _, key := range []string{"policy_number", "claim_number", "vehicle_plate"} {
			delete(f.Slots, key)
		}
	}
	return w.queueAnswer(Values{"purpose": "explain failure and ask for corrected data or offer operator", "action": call}, local(w.s.Language, "Не удалось выполнить действие. Уточните данные или попросите оператора.", "Әрекет орындалмады. Деректерді нақтылаңыз немесе операторды сұраңыз."), "awaiting_slot", false)
}
func (w *work) queueAnswer(facts Values, fallback, status string, complete bool) error {
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
	if err != nil || strings.TrimSpace(answer) == "" {
		w.r.Output.Trace.Error = "response generation failed"
		answer = w.r.Fallback
	}
	if w.r.CompleteFrame {
		w.s.LastCompleted = clone(w.s.Active)
		w.s.Active = nil
	}
	return w.finish(answer, w.r.FinalStatus)
}
func (w *work) transfer() error {
	queue := "operator_general"
	if w.s.Active != nil {
		sc := w.e.Catalog.Scenarios[w.s.Active.ScenarioID]
		if sc.Priority == "urgent" && sc.Handoff != nil {
			queue = sc.Handoff.Queue
		}
	}
	args := Values{"queue": queue, "context": Values{"session_id": w.s.ID, "language": w.s.Language, "active": compactFrame(w.s.Active), "pending_scenarios": pendingIDs(w.s), "history": history(w.s.Turns), "current_input": w.r.Input, "proposals": w.r.Output.Trace.Proposals}}
	err := w.tool("transfer_to_operator", args, func(result Values) {
		if w.s.Active != nil {
			w.s.Active.Pending = nil
		}
		w.s.PendingTurnID = ""
		w.r.Output.Status = "handoff"
		w.r.Output.Answer = local(w.s.Language, "Передаю запрос оператору вместе с контекстом.", "Сұрауды мәліметтерімен бірге операторға беремін.")
		w.r.Output.Language = w.s.Language
		w.r.Output.PendingScenarios = pendingIDs(w.s)
		w.r.Output.Review = clone(w.r.Review)
		w.r.Phase = "finished"
		if errorCode(result) != "" {
			w.r.Output.Answer = local(w.s.Language, "Не удалось подключить оператора. Пожалуйста, повторите запрос позже.", "Операторға қосылу мүмкін болмады. Кейін қайталап көріңіз.")
			w.r.Output.Trace.Error = "handoff failed"
		}
		for i := range w.r.Reviews {
			if w.r.Reviews[i].Output == nil {
				w.r.Reviews[i].Output = clone(&w.r.Output)
			}
		}
	})
	return err
}
