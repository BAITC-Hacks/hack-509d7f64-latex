package router

import (
	"strings"
	"testing"
)

func TestPreRouterSlotParsers(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	r := slotReader{c}
	accept := []struct {
		slot, text string
		want       any
	}{
		{"phone", "+7 701 234 56 78", "+77012345678"},
		{"phone", "8 701 234-56-78", "+77012345678"},
		{"phone", "87012345678", "+77012345678"},
		{"phone", "+7 (701) 234-56-78.", "+77012345678"},
		{"phone", "77012345678", "+77012345678"},
		{"iin", "910512300456", "910512300456"},
		{"iin", "910512 300456", "910512300456"},
		{"iin", " 9105 1230 0456. ", "910512300456"},
		{"new_driver_iin", "850314300121", "850314300121"},
		{"drivers_iin", "850314300121", []any{"850314300121"}},
		{"drivers_iin", "850314300121, 910512300456", []any{"850314300121", "910512300456"}},
		{"drivers_iin", "850314300121 и 910512300456", []any{"850314300121", "910512300456"}},
		{"drivers_iin", "850314300121 және 910512300456", []any{"850314300121", "910512300456"}},
		{"drivers_iin", "850314300121 910512300456", []any{"850314300121", "910512300456"}},
		{"drivers_iin", "850314 300121 910512 300456", []any{"850314300121", "910512300456"}},
		{"policy_number", "SQ-OGPO-123456", "SQ-OGPO-123456"},
		{"policy_number", "sq ogpo 123456", "SQ-OGPO-123456"},
		{"policy_number", "Sq-Casco-123 456", "SQ-CASCO-123456"},
		{"policy_number", "SQ NS 000001", "SQ-NS-000001"},
		{"claim_number", "CL-000123", "CL-000123"},
		{"claim_number", "cl 000123", "CL-000123"},
		{"vehicle_plate", "777ABC02", "777ABC02"},
		{"vehicle_plate", "777 abc 02", "777ABC02"},
		{"vehicle_plate", "777 АВС 02", "777ABC02"},
		{"culprit_vehicle_plate", "123 kz 01", "123KZ01"},
		{"email", "Ivan.Petrov@mail.kz", "ivan.petrov@mail.kz"},
		{"email", "ivan@mail.kz.", "ivan@mail.kz"},
		{"car_value", "8000000", 8000000.0},
		{"car_value", "8 000 000", 8000000.0},
		{"car_year", "2018", 2018.0},
		{"travelers_count", "2", 2.0},
		{"vehicle_type", "легковая", "car"},
		{"vehicle_type", "Легковой.", "car"},
		{"vehicle_type", "жеңіл", "car"},
		{"vehicle_type", "грузовик", "truck"},
		{"vehicle_type", "жүк көлігі", "truck"},
		{"vehicle_type", "мотоцикл", "motorcycle"},
		{"vehicle_type", "car", "car"},
		{"region", "Алматы", "almaty"},
		{"region", "в Астане", "astana"},
		{"region", "Шымкент", "other"},
		{"region", "другой город", "other"},
		{"city", "Алматы", "Almaty"},
		{"city", "в Караганде", "Karaganda"},
		{"city", "Өскемен", "Oskemen"},
		{"city", "Усть-Каменогорск", "Oskemen"},
		{"city", "Almaty", "Almaty"},
		{"city", "Астанада", "Astana"},
		{"city", "Шымкент қаласы", "Shymkent"},
		{"city", "г. Павлодар", "Pavlodar"},
		{"property_type", "квартира", "apartment"},
		{"property_type", "пәтер", "apartment"},
		{"property_type", "частный дом", "house"},
		{"property_type", "үй", "house"},
		{"product_type", "каско", "casco"},
		{"product_type", "ОГПО", "ogpo"},
		{"product_type", "по каско", "casco"},
		{"product_type", "ДМС", "dms"},
		{"document_type", "справка для посольства", "embassy_certificate"},
		{"document_type", "policy_duplicate", "policy_duplicate"},
		{"contact_field", "почту", "email"},
		{"contact_field", "телефон", "phone"},
		{"contact_field", "мекенжай", "address"},
		{"franchise", "50000", 50000.0},
		{"franchise", "50 000", 50000.0},
		{"franchise", "50 тысяч", 50000.0},
		{"franchise", "100 тысяч тенге", 100000.0},
		{"franchise", "без франшизы", 0.0},
		{"franchise", "0", 0.0},
		{"sum_insured", "5 млн", 5000000.0},
		{"sum_insured", "3 000 000", 3000000.0},
		{"sum_insured", "1 миллион", 1000000.0},
		{"trip_start", "15.10.2026", "2026-10-15"},
		{"trip_start", "15.10", "2026-10-15"},
		{"trip_start", "5.1.2027", "2027-01-05"},
		{"trip_start", "2026-10-15", "2026-10-15"},
		{"trip_start", "15 октября", "2026-10-15"},
		{"trip_end", "20 қазан 2026", "2026-10-20"},
		{"preferred_date", "Сегодня", "2026-10-01"},
		{"preferred_date", "бүгін", "2026-10-01"},
		{"preferred_date", "завтра", "2026-10-02"},
		{"preferred_date", "ертең", "2026-10-02"},
		{"preferred_date", "послезавтра", "2026-10-03"},
		{"preferred_date", "бүрсігүні", "2026-10-03"},
		{"incident_date", "вчера", "2026-09-30"},
		{"payment_date", "кеше", "2026-09-30"},
		{"injured", "есть пострадавшие", true},
		{"injured", "все целы", false},
		{"injured", "барлығы аман", false},
	}
	for _, tc := range accept {
		got, ok := r.parse(tc.slot, preNormalize(tc.text))
		if !ok || !equalJSON(got, tc.want) {
			t.Errorf("%s %q: got %#v, %v; want %#v", tc.slot, tc.text, got, ok, tc.want)
			continue
		}
		// The value must have the JSON-decoded shape the engine validates.
		if err := c.ValidateSlots(Values{tc.slot: got}); err != nil {
			t.Errorf("%s %q: %v", tc.slot, tc.text, err)
		}
	}
	reject := []struct{ slot, text string }{
		{"phone", "7012345678"},
		{"phone", "+8 701 234 56 78"},
		{"phone", "мой телефон 87012345678"},
		{"phone", "87012345678, а ещё вопрос"},
		{"phone", "8 701 234 56 78 9"},
		{"phone", "да, но телефон другой"},
		{"iin", "91051230045"},
		{"iin", "мой ИИН 910512300456"},
		{"iin", "910512300456, а ещё…"},
		{"iin", "910512300456 и 850314300121"},
		{"drivers_iin", "850314300121 и"},
		{"drivers_iin", "и 850314300121"},
		{"drivers_iin", "850314300121 и ещё один"},
		{"drivers_iin", "85031430012"},
		{"drivers_iin", "850314300121, , 910512300456"},
		{"drivers_iin", "8503143001219"},
		{"policy_number", "SQ-ABC-123456"},
		{"policy_number", "SQ-OGPO-12345"},
		{"policy_number", "полис SQ-OGPO-123456"},
		{"claim_number", "CL-12345"},
		{"claim_number", "заявление CL-000123"},
		{"vehicle_plate", "777AB"},
		{"vehicle_plate", "номер 777ABC02"},
		{"vehicle_plate", "да"},
		{"email", "ivan собака mail.kz"},
		{"email", "ivan@mail"},
		{"email", "почта ivan@mail.kz"},
		{"car_value", "8 млн"},
		{"car_year", "2018 года"},
		{"car_value", "восемь"},
		{"car_value", "8,5"},
		{"car_value", "80 00"},
		{"car_value", "-5"},
		{"vehicle_type", "да"},
		{"vehicle_type", "легковая, но старая"},
		{"region", "алматинская область"},
		{"city", "Москва"},
		{"city", "в Алматы на Абая"},
		{"product_type", "каско и огпо"},
		{"franchise", "70000"},
		{"franchise", "70 тысяч"},
		{"franchise", "нет"},
		{"sum_insured", "2 млн"},
		{"trip_start", "31.02.2026"},
		{"trip_start", "32.10"},
		{"trip_start", "15.13"},
		{"trip_start", "завтра утром"},
		{"trip_start", "с 15.10"},
		{"trip_start", "15 чего-то"},
		// "Все целы? Есть пострадавшие?": a bare yes/no answers either question.
		{"injured", "да"},
		{"injured", "нет"},
		{"trip_country", "Турция"},
		{"location", "на Абая"},
		{"incident_description", "врезался в столб"},
		{"new_value", "ivan@mail.kz"},
		{"callback_time", "завтра"},
		{"doctor_specialty", "терапевт"},
		{"topic", "франшиза"},
		{"cancel_reason", "продал машину"},
		{"client_id", "C001"},
		{"phone", ""},
	}
	for _, tc := range reject {
		if got, ok := r.parse(tc.slot, preNormalize(tc.text)); ok {
			t.Errorf("%s %q: accepted %#v", tc.slot, tc.text, got)
		}
	}
}

func TestPreRouterBooleanLexicon(t *testing.T) {
	single := Slot{Type: "boolean", Prompt: map[string]string{"ru": "Вы водитель?", "kk": "Сіз жүргізушісіз бе?"}}
	for text, want := range map[string]bool{"да": true, "иә": true, "нет": false, "жоқ": false} {
		if got, ok := preBool("driver", single, preNormalize(text)); !ok || got != want {
			t.Errorf("%q: got %v, %v", text, got, ok)
		}
	}
	if _, ok := preBool("driver", single, "да наверное"); ok {
		t.Error("hedged answer accepted")
	}
}

func TestPreRouterEnumSynonymsAreCatalogValues(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for slot, values := range preEnumSynonyms {
		seen := map[string]string{}
		for canonical, words := range values {
			if err := c.ValidateSlots(Values{slot: slotReader{c}.enum(slot, c.Slots[slot], strings.ToLower(canonical))}); err != nil {
				t.Errorf("%s: %s is not a catalog value", slot, canonical)
			}
			for _, w := range words {
				if w != preNormalize(w) {
					t.Errorf("%s: %q is not in matching form", slot, w)
				}
				if other, ok := seen[w]; ok && other != canonical {
					t.Errorf("%s: %q maps to %s and %s", slot, w, other, canonical)
				}
				seen[w] = canonical
			}
		}
	}
}

// awaitingSession is the state the engine leaves after asking for slots.
func awaitingSession(id, status string, awaiting ...string) *Session {
	f := newFrame(id)
	f.Awaiting = awaiting
	return &Session{ID: "call-1", Language: "ru", Identity: Values{}, Active: f, Turns: []Turn{{Input: input("prev", "..."), Output: &Output{Status: status, ActiveScenario: id}}}}
}

func TestPreRouterWitnesses(t *testing.T) {
	e, _ := setup(t)
	s := awaitingSession("SC33", "awaiting_slot", "city")
	d, ok := e.preRoute(s, input("next", "Астана"))
	if !ok || len(d.Scenarios) != 1 || d.Scenarios[0].ScenarioID != "SC33" || d.Scenarios[0].Confidence != 1 || !d.IsContinuation || d.NeedsHandoff || len(d.Alternatives) != 0 || d.Slots["city"] != "Astana" || len(d.Slots) != 1 || d.Language != "ru" {
		t.Fatalf("%v %#v", ok, d)
	}
	if !strings.Contains(d.Scenarios[0].Reason, "awaited slot city") || e.validateDecision(d) != nil {
		t.Fatal(d.Scenarios[0].Reason)
	}
	confirm := awaitingSession("SC29", "awaiting_confirmation")
	confirm.Active.Pending = &Pending{Action: "update_contact", Inputs: Values{}}
	for _, text := range []string{"Да", "да, верно.", "Иә", "Нет", "жоқ"} {
		d, ok := e.preRoute(confirm, input("next", text))
		if !ok || len(d.Slots) != 0 || d.Scenarios[0].ScenarioID != "SC29" || !strings.Contains(d.Scenarios[0].Reason, "update_contact") {
			t.Errorf("%q: %v %#v", text, ok, d)
		}
	}
	negatives := map[string]func() (*Session, string){
		"no turns": func() (*Session, string) {
			s := awaitingSession("SC33", "awaiting_slot", "city")
			s.Turns = nil
			return s, "Астана"
		},
		"last output missing": func() (*Session, string) {
			s := awaitingSession("SC33", "awaiting_slot", "city")
			s.Turns[0].Output = nil
			return s, "Астана"
		},
		"status completed": func() (*Session, string) { return awaitingSession("SC33", "completed", "city"), "Астана" },
		"status clarification": func() (*Session, string) {
			return awaitingSession("SC33", "clarification", "city"), "Астана"
		},
		"different active scenario": func() (*Session, string) {
			s := awaitingSession("SC33", "awaiting_slot", "city")
			s.Turns[0].Output.ActiveScenario = "SC23"
			return s, "Астана"
		},
		"no active frame": func() (*Session, string) {
			s := awaitingSession("SC33", "awaiting_slot", "city")
			s.Active = nil
			return s, "Астана"
		},
		"empty awaiting": func() (*Session, string) { return awaitingSession("SC33", "awaiting_slot"), "Астана" },
		"leftover words": func() (*Session, string) {
			return awaitingSession("SC33", "awaiting_slot", "city"), "Астана, а где ещё офисы?"
		},
		"a question": func() (*Session, string) {
			return awaitingSession("SC33", "awaiting_slot", "city"), "а в Астане есть?"
		},
		"yes to a slot ask": func() (*Session, string) { return awaitingSession("SC33", "awaiting_slot", "city"), "да" },
		"nil pending": func() (*Session, string) {
			return awaitingSession("SC29", "awaiting_confirmation"), "Да"
		},
		"slot value to a preview": func() (*Session, string) {
			s := awaitingSession("SC29", "awaiting_confirmation", "phone")
			s.Active.Pending = &Pending{Action: "update_contact"}
			return s, "87010000003"
		},
		"yes with a correction": func() (*Session, string) {
			s := awaitingSession("SC29", "awaiting_confirmation")
			s.Active.Pending = &Pending{Action: "update_contact"}
			return s, "да, но телефон другой"
		},
		"two awaited slots accept the text": func() (*Session, string) {
			return awaitingSession("SC04", "awaiting_slot", "iin", "new_driver_iin"), "850314300121"
		},
		"free-text slot": func() (*Session, string) {
			return awaitingSession("SC29", "awaiting_slot", "new_value"), "ivan@mail.kz"
		},
		"iin with a preamble": func() (*Session, string) {
			return awaitingSession("SC25", "awaiting_slot", "phone", "iin"), "мой ИИН 910512300456, а ещё…"
		},
	}
	for name, build := range negatives {
		s, text := build()
		if d, ok := e.preRoute(s, input("next", text)); ok {
			t.Errorf("%s: bypassed with %#v", name, d)
		}
	}
	// phone|iin: the shape decides which alternative was answered.
	for text, slot := range map[string]string{"8 701 000 00 11": "phone", "860219300112": "iin"} {
		d, ok := e.preRoute(awaitingSession("SC25", "awaiting_slot", "phone", "iin"), input("next", text))
		if !ok || !has(d.Slots, slot) || len(d.Slots) != 1 {
			t.Errorf("%q: %v %#v", text, ok, d)
		}
	}
	// Reply language: explicit, then detected, then the session's.
	s = awaitingSession("SC33", "awaiting_slot", "city")
	s.Language = "kk"
	for _, tc := range []struct{ reply, lang, want string }{{"kk", "ru", "kk"}, {"", "ru", "ru"}, {"", "mixed", "kk"}} {
		in := input("next", "Астана")
		in.ReplyLanguage, in.Language = tc.reply, tc.lang
		if d, ok := e.preRoute(s, in); !ok || d.Language != tc.want {
			t.Errorf("%+v: %v %q", tc, ok, d.Language)
		}
	}
	s.Language = ""
	in := input("next", "Астана")
	in.Language = "mixed"
	if d, ok := e.preRoute(s, in); !ok || d.Language != "ru" {
		t.Errorf("fallback language: %v %q", ok, d.Language)
	}
}

func TestPreRouterFillsIdentificationWithoutModel(t *testing.T) {
	e, m := setup(t, decision("SC25", Values{}))
	o := process(t, e, input("one", "Проверьте, действует ли мой полис"))
	if o.Status != "awaiting_slot" || o.Trace.Path == "bypass" || m.calls != 1 {
		t.Fatalf("%+v", o)
	}
	o = process(t, e, input("two", "8 701 000 00 11"))
	if m.calls != 1 || o.Trace.Path != "bypass" || o.Status != "completed" || !actionExecuted(o, "get_policy") {
		t.Fatalf("calls=%d %+v", m.calls, o)
	}
	for _, a := range o.Trace.Actions {
		if a.Name == "find_client" && a.Inputs["phone"] != "+77010000011" {
			t.Fatal(a.Inputs)
		}
	}
	if _, ok := o.Trace.LatencyMS["prerouter"]; !ok || !strings.HasPrefix(o.Trace.Decision.Scenarios[0].Reason, "deterministic continuation") {
		t.Fatalf("%+v", o.Trace)
	}
	// A replay of the bypassed turn returns the stored answer.
	if again := process(t, e, input("two", "8 701 000 00 11")); !equalJSON(again, o) || m.calls != 1 {
		t.Fatal("replay differs")
	}
}

func TestPreRouterWalksRequiredSlots(t *testing.T) {
	e, m := setup(t, decision("SC01", Values{}))
	if o := process(t, e, input("one", "Сколько стоит ОГПО?")); o.Status != "awaiting_slot" {
		t.Fatal(o)
	}
	var o Output
	for i, text := range []string{"Алматы", "легковая", "850314300121"} {
		o = process(t, e, input(string(rune('a'+i)), text))
		if o.Trace.Path != "bypass" {
			t.Fatalf("%q: %+v", text, o)
		}
	}
	if m.calls != 1 || o.Status != "completed" || !actionExecuted(o, "calc_ogpo_price") {
		t.Fatalf("calls=%d %+v", m.calls, o)
	}
	for _, a := range o.Trace.Actions {
		if a.Name == "calc_ogpo_price" && number(a.Result["price"]) != 30400 {
			t.Fatal(a.Result)
		}
	}
}

func TestPreRouterConfirmsPreviewWithoutModel(t *testing.T) {
	slots := Values{"phone": "+77010000003", "contact_field": "email", "new_value": "new@mail.example"}
	e, m := setup(t, decision("SC29", slots))
	if o := process(t, e, input("one", "Поменяйте почту")); o.Status != "awaiting_confirmation" {
		t.Fatal(o)
	}
	o := process(t, e, input("two", "Да"))
	if m.calls != 1 || o.Trace.Path != "bypass" || o.Status != "completed" || !actionExecuted(o, "update_contact") {
		t.Fatalf("calls=%d %+v", m.calls, o)
	}

	e, m = setup(t, decision("SC29", slots))
	process(t, e, input("one", "Поменяйте почту"))
	o = process(t, e, input("two", "Жоқ"))
	if m.calls != 1 || o.Trace.Path != "bypass" || o.Status != "cancelled" || actionExecuted(o, "update_contact") {
		t.Fatalf("calls=%d %+v", m.calls, o)
	}
}

func TestPreRouterLeavesQualifiedYesToModel(t *testing.T) {
	e, m := setup(t, decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "new@mail.example"}), decision("SC29", Values{}))
	process(t, e, input("one", "Поменяйте почту"))
	o := process(t, e, input("two", "да, но телефон другой"))
	if m.calls != 2 || o.Trace.Path == "bypass" || actionExecuted(o, "update_contact") {
		t.Fatalf("calls=%d %+v", m.calls, o)
	}
}
