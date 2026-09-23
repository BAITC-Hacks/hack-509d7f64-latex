package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var ErrInvalidInput = errors.New("invalid input")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

type Engine struct {
	Catalog  *Catalog
	Store    *Store
	Backend  *Backend
	Model    Model
	MaxSteps int
	Timeout  time.Duration
}

func NewEngine(c *Catalog, m Model) *Engine {
	return &Engine{Catalog: c, Store: NewStore(), Backend: NewBackend(c), Model: m, MaxSteps: 24, Timeout: 60 * time.Second}
}
func (e *Engine) ValidateInput(in Input) error {
	if !idPattern.MatchString(in.SessionID) || !idPattern.MatchString(in.RequestID) {
		return fmt.Errorf("%w: session_id/request_id must be 1-100 letters, digits, underscores or hyphens", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Text) == "" || len(in.Text) > 8000 {
		return fmt.Errorf("%w: text must contain 1-8000 bytes", ErrInvalidInput)
	}
	if !slices.Contains([]string{"ru", "kk", "mixed"}, in.Language) {
		return fmt.Errorf("%w: language must be ru, kk or mixed", ErrInvalidInput)
	}
	if in.ReplyLanguage != "" && in.ReplyLanguage != "ru" && in.ReplyLanguage != "kk" {
		return fmt.Errorf("%w: invalid reply_language", ErrInvalidInput)
	}
	if err := e.Catalog.ValidateSlots(in.Slots); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return nil
}
func (e *Engine) validateDecision(d Decision) error {
	if len(d.Scenarios) == 0 || len(d.Scenarios) > 8 {
		return fmt.Errorf("invalid scenario count")
	}
	if d.Language != "ru" && d.Language != "kk" {
		return fmt.Errorf("invalid model language")
	}
	seen := map[string]bool{}
	for _, c := range d.Scenarios {
		if !e.Catalog.ValidID(c.ScenarioID) || seen[c.ScenarioID] {
			return fmt.Errorf("unknown or duplicate scenario")
		}
		seen[c.ScenarioID] = true
	}
	for _, c := range append(append([]Candidate{}, d.Scenarios...), d.Alternatives...) {
		if !e.Catalog.ValidID(c.ScenarioID) || math.IsNaN(c.Confidence) || c.Confidence < 0 || c.Confidence > 1 {
			return fmt.Errorf("invalid candidate")
		}
	}
	return e.Catalog.ValidateSlots(d.Slots)
}
func (e *Engine) Process(ctx context.Context, in Input) (Output, error) {
	if err := e.ValidateInput(in); err != nil {
		return Output{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	entry, unlock, err := e.Store.lock(ctx, in.SessionID)
	if err != nil {
		return Output{}, err
	}
	defer unlock()
	s := &entry.state
	for _, turn := range s.Turns {
		if turn.Input.RequestID == in.RequestID {
			a, _ := json.Marshal(turn.Input)
			b, _ := json.Marshal(in)
			if string(a) != string(b) {
				return Output{}, ErrConflict
			}
			if turn.Output != nil {
				return clone(*turn.Output), nil
			}
		}
	}
	if len(s.Turns) >= 100 {
		return Output{}, fmt.Errorf("%w: session reached 100 turns; start a new session", ErrInvalidInput)
	}
	if s.Active != nil && s.Active.Pending != nil && len(s.Turns) > 0 {
		last := s.Turns[len(s.Turns)-1].Output
		if last == nil || last.Status != "awaiting_confirmation" || last.ActiveScenario != s.Active.ScenarioID {
			s.Active.Pending = nil
		}
	}
	start := time.Now()
	s.Turns = append(s.Turns, Turn{Input: clone(in)})
	turnIndex := len(s.Turns) - 1
	out := Output{SessionID: in.SessionID, RequestID: in.RequestID, Language: s.Language, PendingScenarios: []string{}, Trace: Trace{Turn: len(s.Turns), Transcript: in.Text, Language: in.Language, Actions: []ActionCall{}, LatencyMS: map[string]int64{}}}
	// Every accepted input obtains a stored answer, including model failures and limits.
	finish := func(answer, status string) (Output, error) {
		if strings.TrimSpace(answer) == "" {
			answer = local(s.Language, "Не удалось завершить запрос. Соединю с оператором.", "Сұрауды аяқтау мүмкін болмады. Операторға қосамын.")
		}
		out.Answer = answer
		out.Status = status
		out.Language = s.Language
		if s.Active != nil {
			out.ActiveScenario = s.Active.ScenarioID
		}
		for _, f := range s.Queue {
			out.PendingScenarios = append(out.PendingScenarios, f.ScenarioID)
		}
		for i := len(s.Stack) - 1; i >= 0; i-- {
			out.PendingScenarios = append(out.PendingScenarios, s.Stack[i].ScenarioID)
		}
		out.Trace.LatencyMS["total"] = time.Since(start).Milliseconds()
		s.UpdatedAt = time.Now()
		copy := clone(out)
		s.Turns[turnIndex].Output = &copy
		return clone(out), nil
	}
	if in.ReplyLanguage != "" {
		s.Language = in.ReplyLanguage
	} else if in.Language != "mixed" {
		s.Language = in.Language
	}
	routeStart := time.Now()
	d, err := e.Model.Route(ctx, in, modelState(*s))
	out.Trace.LatencyMS["router"] = time.Since(routeStart).Milliseconds()
	if err == nil {
		err = e.validateDecision(d)
	}
	if err != nil {
		out.Trace.Error = err.Error()
		e.handoff(s, &out, "operator_general", "router_failed")
		return finish(local(s.Language, "Не удалось уверенно обработать запрос. Передаю контекст оператору.", "Сұрауды сенімді өңдей алмадым. Мәліметтерді операторға беремін."), "handoff")
	}
	if in.ReplyLanguage != "" {
		d.Language = in.ReplyLanguage
	} else if in.Language != "mixed" {
		d.Language = in.Language
	}
	s.Language = d.Language
	if d.Slots == nil {
		d.Slots = Values{}
	}
	merge(d.Slots, in.Slots)
	// Keep urgency stable; other intents preserve spoken order. Never promote weak
	// secondary intents into executable actions.
	sort.SliceStable(d.Scenarios, func(i, j int) bool {
		return e.Catalog.Scenarios[d.Scenarios[i].ScenarioID].Priority == "urgent" && e.Catalog.Scenarios[d.Scenarios[j].ScenarioID].Priority != "urgent"
	})
	out.Trace.Decision = clone(d)
	for _, c := range d.Scenarios {
		if c.ScenarioID == "SC37" && c.Confidence >= .75 {
			e.handoff(s, &out, "operator_general", "user_requested")
			return finish(local(s.Language, "Передаю разговор оператору вместе с контекстом.", "Әңгімені мәліметтерімен бірге операторға беремін."), "handoff")
		}
	}
	primary := d.Scenarios[0]
	if primary.Confidence < .75 || primary.ScenarioID == "SYS_UNCLEAR" {
		if primary.Confidence < .45 {
			s.LowConfidence++
		} else {
			s.LowConfidence = 0
		}
		if s.LowConfidence >= 2 {
			e.handoff(s, &out, "operator_general", "low_confidence_twice")
			return finish(local(s.Language, "Передаю запрос оператору, чтобы помочь точнее.", "Нақты көмек беру үшін сұрауды операторға беремін."), "handoff")
		}
		options := append(append([]Candidate{}, d.Scenarios...), d.Alternatives...)
		names := []string{}
		for _, c := range options {
			if sc, ok := e.Catalog.Scenarios[c.ScenarioID]; ok && !slices.Contains(names, sc.Name) {
				names = append(names, sc.Name)
				if len(names) == 2 {
					break
				}
			}
		}
		answer := e.respond(ctx, s, &out, Values{"purpose": "clarify", "options": names, "utterance": in.Text}, local(s.Language, "Уточните, пожалуйста, что нужно сделать со страховкой?", "Сақтандыру бойынша не істеу керегін нақтылаңызшы?"))
		return finish(answer, "clarification")
	}
	s.LowConfidence = 0
	if response, ok := e.Catalog.System[primary.ScenarioID]; ok {
		if primary.ScenarioID == "SYS_GOODBYE" {
			s.Active = nil
			s.Queue = nil
			s.Stack = nil
		}
		return finish(response[s.Language], "completed")
	}
	e.selectFrames(s, d)
	f := s.Active
	sc := e.Catalog.Scenarios[f.ScenarioID]
	// Corrections invalidate both the preview and any preceding quote. Never
	// rerun an already executed irreversible operation when collecting more data.
	changed := false
	for k, v := range d.Slots {
		if old, ok := f.Slots[k]; ok && !equalJSON(old, v) {
			changed = true
		}
	}
	if changed {
		f.Pending = nil
		mutated := false
		for _, r := range f.Results {
			if e.Catalog.Actions[r.Name].Irreversible && errorCode(r.Result) == "" {
				mutated = true
			}
		}
		if !mutated {
			f.NextAction = 0
			f.Results = []ActionCall{}
		}
	}
	// Identity changes cannot inherit an earlier customer's context.
	for _, k := range []string{"phone", "iin"} {
		if v, ok := d.Slots[k]; ok && has(s.Identity, k) && s.Identity[k] != v {
			s.Identity = Values{}
			s.LastCompleted = nil
			f.Slots = clone(d.Slots)
			f.NextAction = 0
			f.Pending = nil
			f.Results = nil
			s.Stack = nil
			s.Queue = nil
			break
		}
	}
	e.fillSlots(f, d.Slots)
	for k, v := range s.Identity {
		if !has(f.Slots, k) {
			f.Slots[k] = v
		}
	}
	approved := false
	if f.Pending != nil {
		current := e.arguments(s, f, sc, f.Pending.Action)
		if !equalJSON(current, f.Pending.Inputs) {
			f.Pending = nil
		} else if explicitYes(in.Text) && d.IsContinuation {
			approved = true
		} else if explicitNo(in.Text) && d.IsContinuation {
			f.Pending = nil
			s.Active = nil
			return finish(local(s.Language, "Действие отменено. Чем ещё могу помочь?", "Әрекет тоқтатылды. Тағы қалай көмектесе аламын?"), "cancelled")
		}
	}
	if sc.Priority == "urgent" && ((sc.ID == "SC11" && (f.Slots["injured"] == true || d.NeedsHandoff)) || (sc.ID == "SC15" && d.NeedsHandoff) || (sc.ID == "SC38" && d.NeedsHandoff)) {
		e.handoff(s, &out, sc.Handoff.Queue, "urgent")
		answer := e.respond(ctx, s, &out, Values{"purpose": "urgent assistance and handoff", "scenario": sc, "knowledge": e.Catalog.KB, "utterance": in.Text, "actions": out.Trace.Actions}, local(s.Language, "Передаю срочный запрос профильному оператору.", "Шұғыл сұрауды тиісті операторға беремін."))
		return finish(answer, "handoff")
	}
	for step := 0; step < e.MaxSteps; step++ {
		if ctx.Err() != nil {
			out.Trace.Error = "turn deadline exceeded"
			e.handoff(s, &out, "operator_general", "deadline")
			return finish(local(s.Language, "Обработка заняла слишком много времени. Передаю оператору.", "Өңдеу тым ұзаққа созылды. Операторға беремін."), "handoff")
		}
		if sc.Identify && !has(s.Identity, "client_id") {
			if !has(f.Slots, "phone") && !has(f.Slots, "iin") {
				return finish(e.question(s, f, "phone"), "awaiting_slot")
			}
			args := Values{"phone": f.Slots["phone"], "iin": f.Slots["iin"]}
			result := e.Backend.Execute("find_client", args, in.SessionID+":"+in.RequestID+":identify")
			call := ActionCall{Name: "find_client", Mode: "execute", Inputs: args, Result: result}
			out.Trace.Actions = append(out.Trace.Actions, call)
			if errorCode(result) != "" {
				f.Failures["identify"]++
				delete(f.Slots, "phone")
				delete(f.Slots, "iin")
				if f.Failures["identify"] >= 2 {
					e.handoff(s, &out, "operator_general", "identification_failed")
					return finish(local(s.Language, "Не удалось найти клиента. Передаю оператору.", "Клиент табылмады. Операторға беремін."), "handoff")
				}
				return finish(local(s.Language, "Клиент не найден. Назовите ИИН или проверьте номер телефона.", "Клиент табылмады. ЖСН-ді айтыңыз немесе телефон нөмірін тексеріңіз."), "awaiting_slot")
			}
			s.Identity = clone(result)
			merge(f.Slots, result)
			e.inferSinglePolicy(s, f, sc, &out)
			continue
		}
		for _, slot := range sc.Slots.Required {
			if !has(f.Slots, slot) {
				question := e.question(s, f, slot)
				if sc.Priority == "urgent" {
					question = e.respond(ctx, s, &out, Values{"purpose": "give immediate safety guidance, then ask only the supplied question", "question": question, "scenario": sc, "knowledge": e.Catalog.KB}, question)
				}
				return finish(question, "awaiting_slot")
			}
		}
		if f.NextAction >= len(sc.Actions) {
			facts := Values{"purpose": "answer using executed actions", "scenario": sc, "slots": f.Slots, "actions": f.Results, "utterance": in.Text, "pending_scenarios": pendingIDs(s)}
			answer := e.respond(ctx, s, &out, facts, local(s.Language, "Запрос обработан; результаты сохранены.", "Сұрау өңделді; нәтижелер сақталды."))
			s.LastCompleted = clone(f)
			s.Active = nil
			return finish(answer, "completed")
		}
		name := sc.Actions[f.NextAction]
		if name == "find_client" && has(s.Identity, "client_id") {
			f.NextAction++
			continue
		}
		if name == "send_sms" && !has(f.Slots, "phone") {
			f.NextAction++
			continue
		}
		if name == "transfer_to_operator" && !e.shouldHandoff(sc, d, f) {
			f.NextAction++
			continue
		}
		args := e.arguments(s, f, sc, name)
		action := e.Catalog.Actions[name]
		missing := ""
		for _, group := range action.Inputs {
			present := false
			for _, slot := range strings.Split(group, "|") {
				present = present || has(args, slot)
			}
			if !present {
				missing = strings.Split(group, "|")[0]
				break
			}
		}
		if missing != "" {
			return finish(e.question(s, f, missing), "awaiting_slot")
		}
		if action.Irreversible && !approved {
			f.Pending = &Pending{Action: name, Inputs: clone(args)}
			call := ActionCall{Name: name, Mode: "preview", Inputs: clone(args), Result: Values{"confirmation_required": true}}
			out.Trace.Actions = append(out.Trace.Actions, call)
			// Deterministic preview guarantees that the actual action and parameters
			// are shown even if response generation fails. Personal values are masked.
			answer := e.confirmation(s, sc, name, args)
			return finish(answer, "awaiting_confirmation")
		}
		key := fmt.Sprintf("%s:%s:%s:%d", in.SessionID, in.RequestID, f.ScenarioID, f.NextAction)
		toolStart := time.Now()
		result := e.Backend.Execute(name, args, key)
		out.Trace.LatencyMS["tools"] += time.Since(toolStart).Milliseconds()
		call := ActionCall{Name: name, Mode: "execute", Inputs: clone(args), Result: result}
		out.Trace.Actions = append(out.Trace.Actions, call)
		f.Results = append(f.Results, call)
		code := errorCode(result)
		if code != "" {
			f.Failures[name]++
			f.Pending = nil
			approved = false
			if code == "service_unavailable" && f.Failures[name] == 1 {
				continue
			}
			if f.Failures[name] >= 2 || code == "service_unavailable" {
				e.handoff(s, &out, "operator_general", "tool_failed")
				return finish(local(s.Language, "Не удалось выполнить действие. Передаю результаты оператору.", "Әрекет орындалмады. Нәтижелерді операторға беремін."), "handoff")
			}
			// Drop failed identifiers so the next turn can correct them, not retry
			// forever using stale data.
			if code == "not_found" {
				for _, k := range []string{"policy_number", "claim_number", "vehicle_plate"} {
					delete(f.Slots, k)
				}
			}
			answer := e.respond(ctx, s, &out, Values{"purpose": "explain tool failure and ask for a correction or offer operator", "action": call}, local(s.Language, "Не удалось выполнить действие. Уточните данные или попросите оператора.", "Әрекет орындалмады. Деректерді нақтылаңыз немесе операторды сұраңыз."))
			return finish(answer, "awaiting_slot")
		}
		if name == "find_client" {
			s.Identity = clone(result)
			merge(f.Slots, result)
		}
		// Do not overwrite user-proposed changes with values read from the policy.
		for _, k := range []string{"policy_number", "claim_number", "price", "bm_class", "client_id"} {
			if has(result, k) {
				if !has(f.Slots, k) || k == "price" || k == "bm_class" || name == "create_policy" || name == "renew_policy" || name == "create_claim" {
					f.Slots[k] = result[k]
				}
			}
		}
		if name == "get_policy" && !has(f.Slots, "product_type") {
			f.Slots["product_type"] = result["product"]
		}
		if name == "get_policy" && has(result, "premium") {
			f.Slots["price"] = result["premium"]
		}
		if name == "get_policies" {
			rows := list(result["policies"])
			if len(rows) == 1 {
				f.Slots["policy_number"] = asMap(rows[0])["policy_number"]
			}
		}
		if name == "update_contact" {
			s.Identity[str(args["contact_field"])] = args["new_value"]
		}
		f.NextAction++
		f.Pending = nil
		approved = false
		if name == "transfer_to_operator" {
			answer := local(s.Language, "Передаю разговор профильному оператору с контекстом.", "Әңгімені мәліметтерімен бірге тиісті операторға беремін.")
			return finish(answer, "handoff")
		}
	}
	e.handoff(s, &out, "operator_general", "step_limit")
	return finish(local(s.Language, "Для завершения запроса подключаю оператора.", "Сұрауды аяқтау үшін операторды қосамын."), "handoff")
}
func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func explicitYes(s string) bool {
	s = strings.NewReplacer(",", "", ".", "", "!", "", "?", "").Replace(strings.ToLower(strings.TrimSpace(s)))
	s = strings.Join(strings.Fields(s), " ")
	return slices.Contains([]string{"да", "да подтверждаю", "да верно", "да всё верно", "да оформляйте", "да добавляйте", "да записывайте", "подтверждаю", "согласен", "согласна", "иә", "ия", "иә тіркеңіз", "иә растаймын", "иә дұрыс", "растаймын", "мақұл", "yes", "confirm"}, s)
}
func explicitNo(s string) bool {
	return slices.Contains([]string{"нет", "отмена", "отменить", "жоқ", "бас тартамын", "no", "cancel"}, strings.Trim(strings.ToLower(strings.TrimSpace(s)), ".!?,"))
}
func newFrame(id string) *Frame {
	return &Frame{ScenarioID: id, Slots: Values{}, Results: []ActionCall{}, Failures: map[string]int{}}
}
func pendingIDs(s *Session) []string {
	ids := []string{}
	for _, f := range s.Queue {
		ids = append(ids, f.ScenarioID)
	}
	for _, f := range s.Stack {
		ids = append(ids, f.ScenarioID)
	}
	return ids
}
func (e *Engine) fillSlots(f *Frame, values Values) {
	sc := e.Catalog.Scenarios[f.ScenarioID]
	allowed := append(append([]string{}, sc.Slots.Required...), sc.Slots.Optional...)
	allowed = append(allowed, "phone", "iin")
	for _, name := range sc.Actions {
		for _, group := range e.Catalog.Actions[name].Inputs {
			allowed = append(allowed, strings.Split(group, "|")...)
		}
	}
	for k, v := range values {
		if slices.Contains(allowed, k) {
			f.Slots[k] = v
		}
	}
}
func (e *Engine) selectFrames(s *Session, d Decision) {
	id := d.Scenarios[0].ScenarioID
	if s.Active == nil || s.Active.ScenarioID != id {
		var selected *Frame
		for i, f := range s.Stack {
			if f.ScenarioID == id {
				selected = f
				s.Stack = append(s.Stack[:i], s.Stack[i+1:]...)
				break
			}
		}
		if selected == nil {
			for i, f := range s.Queue {
				if f.ScenarioID == id {
					selected = f
					s.Queue = append(s.Queue[:i], s.Queue[i+1:]...)
					break
				}
			}
		}
		if s.Active != nil {
			s.Active.Pending = nil
			s.Stack = append(s.Stack, s.Active)
		}
		if selected == nil {
			selected = newFrame(id)
			if previous := s.LastCompleted; previous != nil && previous.ScenarioID == "SC01" && id == "SC02" {
				for _, key := range []string{"region", "vehicle_type", "drivers_iin", "vehicle_plate"} {
					if has(previous.Slots, key) {
						selected.Slots[key] = clone(previous.Slots[key])
					}
				}
			}
		}
		s.Active = selected
	}
	for _, c := range d.Scenarios[1:] {
		if _, ok := e.Catalog.Scenarios[c.ScenarioID]; !ok || c.Confidence < .75 {
			continue
		}
		var existing *Frame
		for _, f := range append(append([]*Frame{}, s.Queue...), s.Stack...) {
			if f.ScenarioID == c.ScenarioID {
				existing = f
				break
			}
		}
		if existing == nil {
			existing = newFrame(c.ScenarioID)
			s.Queue = append(s.Queue, existing)
		}
		e.fillSlots(existing, d.Slots)
	}
}
func (e *Engine) question(s *Session, f *Frame, key string) string {
	if p := e.Catalog.Slots[key].Prompt[s.Language]; p != "" {
		return p
	}
	return local(s.Language, "Уточните данные для продолжения или попросите оператора.", "Жалғастыру үшін деректерді нақтылаңыз немесе операторды сұраңыз.")
}
func (e *Engine) respond(ctx context.Context, s *Session, out *Output, facts Values, fallback string) string {
	start := time.Now()
	answer, err := e.Model.Respond(ctx, s.Language, facts)
	out.Trace.LatencyMS["response"] += time.Since(start).Milliseconds()
	if err != nil || strings.TrimSpace(answer) == "" {
		out.Trace.Error = "response generation failed"
		return fallback
	}
	return answer
}
func (e *Engine) handoff(s *Session, out *Output, queue, reason string) {
	args := Values{"queue": queue, "context": Values{"session_id": s.ID, "language": s.Language, "active": compactFrame(s.Active), "pending_scenarios": pendingIDs(s), "history": history(s.Turns), "reason": reason}}
	result := e.Backend.Execute("transfer_to_operator", args, out.SessionID+":"+out.RequestID+":handoff")
	out.Trace.Actions = append(out.Trace.Actions, ActionCall{Name: "transfer_to_operator", Mode: "execute", Inputs: Values{"queue": queue, "reason": reason}, Result: result})
	if s.Active != nil {
		s.Active.Pending = nil
	}
}
func (e *Engine) shouldHandoff(sc Scenario, d Decision, f *Frame) bool {
	if sc.Handoff == nil {
		return false
	}
	if strings.HasPrefix(sc.Handoff.When, "always") {
		return true
	}
	if d.NeedsHandoff {
		return true
	}
	if sc.ID == "SC30" {
		for _, r := range f.Results {
			if r.Result["payment_status"] == "charged_policy_not_issued" {
				return true
			}
		}
	}
	return false
}
func (e *Engine) inferSinglePolicy(s *Session, f *Frame, sc Scenario, out *Output) {
	if has(f.Slots, "policy_number") {
		return
	}
	r := e.Backend.Execute("get_policies", Values{"client_id": s.Identity["client_id"]}, "")
	product := map[string]string{"SC13": "casco", "SC14": "property", "SC15": "travel", "SC16": "accident", "SC21": "dms", "SC22": "dms", "SC24": "dms", "SC04": "ogpo", "SC05": "ogpo"}[sc.ID]
	matches := []Values{}
	for _, v := range list(r["policies"]) {
		p := asMap(v)
		if product == "" || p["product"] == product {
			matches = append(matches, p)
		}
	}
	if len(matches) == 1 {
		f.Slots["policy_number"] = matches[0]["policy_number"]
	}
	out.Trace.Actions = append(out.Trace.Actions, ActionCall{Name: "get_policies", Mode: "execute", Inputs: Values{"client_id": s.Identity["client_id"]}, Result: r})
}
func (e *Engine) arguments(s *Session, f *Frame, sc Scenario, name string) Values {
	a := clone(f.Slots)
	if has(s.Identity, "client_id") {
		a["client_id"] = s.Identity["client_id"]
	}
	if name == "get_bm_class" {
		if has(a, "new_driver_iin") {
			a["iin"] = a["new_driver_iin"]
		} else if xs := list(a["drivers_iin"]); len(xs) > 0 && !has(a, "iin") {
			a["iin"] = xs[0]
		}
	}
	if name == "calc_ogpo_price" && !has(a, "region") && len(str(a["vehicle_plate"])) >= 2 {
		plate := str(a["vehicle_plate"])
		region := path(e.Catalog.KB, "products", "ogpo", "pricing", "region_by_plate_code", plate[len(plate)-2:])
		if region == nil {
			region = "other"
		}
		a["region"] = region
	}
	if product := map[string]string{"SC02": "ogpo", "SC06": "travel", "SC12": "ogpo", "SC13": "casco", "SC14": "property", "SC16": "accident"}[sc.ID]; product != "" {
		a["product_type"] = product
	}
	if sc.ID == "SC12" {
		a["victim_lookup"] = true
		if name == "get_policy" {
			a["vehicle_plate"] = a["culprit_vehicle_plate"]
			delete(a, "policy_number")
		}
	}
	if name == "kb_lookup" {
		topic := map[string]string{"SC03": "products.casco", "SC07": "products.property", "SC08": "products.accident", "SC09": "products.dms", "SC11": "claims.road_accident_now", "SC18": "claims", "SC24": "products.dms.e_card", "SC31": "payments", "SC32": "bonus_malus", "SC34": "app_help", "SC38": "fraud_policy"}[sc.ID]
		if topic == "" {
			topic = "all"
		}
		a["topic"] = topic
	}
	if name == "transfer_to_operator" {
		a["queue"] = sc.Handoff.Queue
		a["context"] = Values{"session_id": s.ID, "active": compactFrame(f), "pending_scenarios": pendingIDs(s), "language": s.Language, "history": history(s.Turns)}
	}
	return a
}
func (e *Engine) confirmation(s *Session, sc Scenario, name string, args Values) string {
	// Render only the parameters of the proposed operation, not unrelated
	// identity/profile data. Keep wording deterministic at the approval boundary.
	keys := map[string][]string{
		"create_policy": {"product_type", "vehicle_plate", "drivers_iin", "trip_country", "trip_start", "trip_end", "travelers_count", "price", "phone"},
		"renew_policy":  {"policy_number", "price"}, "update_policy": {"policy_number", "new_driver_iin", "vehicle_plate"},
		"cancel_policy": {"policy_number", "cancel_reason"}, "create_claim": {"policy_number", "incident_date", "incident_description", "phone"},
		"create_dispute": {"claim_number", "complaint_text"}, "book_inspection": {"claim_number", "city", "preferred_date"},
		"book_appointment": {"policy_number", "doctor_specialty", "city", "preferred_date"}, "update_contact": {"contact_field", "new_value"},
	}[name]
	labels := map[string][2]string{
		"product_type": {"страхование", "сақтандыру"}, "vehicle_plate": {"госномер", "көлік нөмірі"}, "drivers_iin": {"ИИН водителей", "жүргізушілердің ЖСН"},
		"trip_country": {"страна", "ел"}, "trip_start": {"начало поездки", "сапардың басталуы"}, "trip_end": {"окончание поездки", "сапардың аяқталуы"},
		"travelers_count": {"число туристов", "саяхатшылар саны"}, "price": {"сумма в тенге", "теңге сомасы"}, "phone": {"телефон", "телефон"},
		"policy_number": {"полис", "полис"}, "new_driver_iin": {"ИИН нового водителя", "жаңа жүргізушінің ЖСН"}, "cancel_reason": {"причина", "себебі"},
		"incident_date": {"дата события", "оқиға күні"}, "incident_description": {"событие", "оқиға"}, "claim_number": {"заявление", "өтініш"},
		"complaint_text": {"обращение", "шағым"}, "city": {"город", "қала"}, "preferred_date": {"дата", "күні"}, "doctor_specialty": {"врач", "дәрігер"},
		"contact_field": {"изменяемые данные", "өзгертілетін деректер"}, "new_value": {"новое значение", "жаңа мән"},
	}
	parts := []string{}
	for _, k := range keys {
		if !has(args, k) {
			continue
		}
		v := str(args[k])
		if slices.Contains([]string{"phone", "iin", "new_driver_iin", "drivers_iin", "email"}, k) {
			if len(v) > 4 {
				v = "***" + v[len(v)-4:]
			}
		}
		if k == "new_value" && args["contact_field"] != "address" {
			if len(v) > 4 {
				v = "***" + v[len(v)-4:]
			}
		}
		if k == "drivers_iin" {
			values := []string{}
			for _, iin := range list(args[k]) {
				value := str(iin)
				if len(value) > 4 {
					value = "***" + value[len(value)-4:]
				}
				values = append(values, value)
			}
			v = strings.Join(values, " / ")
		}
		if k == "contact_field" {
			field := map[string][2]string{"phone": {"телефон", "телефон"}, "email": {"электронная почта", "электрондық пошта"}, "address": {"адрес", "мекенжай"}}[v]
			v = local(s.Language, field[0], field[1])
		}
		label := labels[k]
		parts = append(parts, local(s.Language, label[0], label[1])+": "+v)
	}
	label := map[string][2]string{"create_policy": {"оформление полиса", "полис рәсімдеу"}, "renew_policy": {"продление полиса", "полисті ұзарту"}, "update_policy": {"изменение полиса", "полисті өзгерту"}, "cancel_policy": {"расторжение полиса", "полисті тоқтату"}, "create_claim": {"регистрация заявления", "өтінішті тіркеу"}, "create_dispute": {"регистрация несогласия", "келіспеушілікті тіркеу"}, "book_inspection": {"запись на осмотр", "тексеруге жазылу"}, "book_appointment": {"запись к врачу", "дәрігерге жазылу"}, "update_contact": {"изменение контактов", "байланыс деректерін өзгерту"}}[name]
	action := label[0]
	if s.Language == "kk" {
		action = label[1]
	}
	return local(s.Language, "Подтвердите "+action+": "+strings.Join(parts, ", ")+". Выполнить?", "Растаңыз: "+action+"; "+strings.Join(parts, ", ")+". Орындаймыз ба?")
}
