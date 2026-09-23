package router

import (
	"encoding/json"
	"slices"
	"strings"
)

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func explicitYes(s string) bool {
	s = strings.NewReplacer(",", "", ".", "", "!", "", "?", "").Replace(strings.ToLower(strings.TrimSpace(s)))
	s = strings.Join(strings.Fields(s), " ")
	return slices.Contains([]string{"да", "да подтверждаю", "да верно", "да всё верно", "верно", "всё верно", "да правильно", "правильно", "да оформляйте", "да добавляйте", "да записывайте", "подтверждаю", "согласен", "согласна", "иә", "ия", "иә тіркеңіз", "иә растаймын", "иә дұрыс", "растаймын", "мақұл", "yes", "confirm"}, s)
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

// shouldHandoff reports whether the scenario's handoff rule applies, and why.
func (e *Engine) shouldHandoff(sc Scenario, d Decision, f *Frame) (bool, string) {
	if sc.Handoff == nil {
		return false, ""
	}
	if strings.HasPrefix(sc.Handoff.When, "always") {
		return true, sc.Handoff.When
	}
	if d.NeedsHandoff {
		return true, "needs_handoff (" + sc.Handoff.When + ")"
	}
	if sc.ID == "SC30" {
		for _, r := range f.Results {
			if r.Result["payment_status"] == "charged_policy_not_issued" {
				return true, "payment charged_policy_not_issued"
			}
		}
	}
	return false, ""
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
