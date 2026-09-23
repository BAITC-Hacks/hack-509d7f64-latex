package router

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// mockTools executes actions on a fresh mock backend the way work.tool does,
// with a new operation key per call, and records them as frame results.
type mockTools struct {
	b *Backend
	n int
}

func (m *mockTools) run(name string, args Values) ActionCall {
	m.n++
	return ActionCall{Name: name, Mode: "execute", Inputs: clone(args), Result: clone(m.b.Execute(name, args, fmt.Sprintf("operation-%06d", m.n)))}
}

// completedFacts mirrors what execute() hands queueAnswer when a frame completes.
func completedFacts(c *Catalog, id string, slots Values, calls ...ActionCall) Values {
	return Values{"purpose": "answer from executed actions", "scenario": c.Scenarios[id], "slots": slots, "actions": calls, "utterance": "test", "pending_scenarios": []string{}}
}

type templateStep struct {
	name string
	args Values
}

func templateCase(t *testing.T, e *Engine, id string, slots Values, steps ...templateStep) Values {
	t.Helper()
	m := &mockTools{b: NewBackend(e.Catalog)}
	calls := []ActionCall{}
	for _, s := range steps {
		args := s.args
		if args == nil {
			args = slots
		}
		calls = append(calls, m.run(s.name, args))
	}
	return completedFacts(e.Catalog, id, slots, calls...)
}

func TestTemplateListsPartitionCatalog(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range templated {
		seen[id] = true
		for _, lang := range []string{"ru", "kk"} {
			if c.Scenarios[id].Responses[lang]["closing"] == "" {
				t.Fatalf("%s has no %s closing", id, lang)
			}
		}
	}
	for reason, ids := range untemplated {
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("%s is both templated and excluded (%s)", id, reason)
			}
			seen[id] = true
		}
	}
	for id := range c.Scenarios {
		if !seen[id] {
			t.Fatalf("%s is neither templated nor excluded", id)
		}
	}
	if len(seen) != len(c.Scenarios) {
		t.Fatalf("lists name %d scenarios, catalog has %d", len(seen), len(c.Scenarios))
	}
}

func TestTemplateFillsClosingFromResults(t *testing.T) {
	e, _ := setup(t)
	ogpo := Values{"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121"}, "iin": "850314300121"}
	buy := Values{"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121"}, "vehicle_plate": "123ABC02", "phone": "+77010000001", "product_type": "ogpo", "price": 30400.0}
	policy := Values{"policy_number": "SQ-OGPO-104501", "client_id": "C001", "phone": "+77010000001"}
	plate := Values{"policy_number": "SQ-OGPO-104501", "client_id": "C001", "vehicle_plate": "555KZZ02"}
	cancel := Values{"policy_number": "SQ-OGPO-104501", "client_id": "C001", "cancel_reason": "продал машину"}
	inspection := Values{"claim_number": "CL-500198", "client_id": "C001", "city": "Almaty", "preferred_date": "2026-10-05"}
	doctor := Values{"policy_number": "SQ-DMS-604220", "client_id": "C002", "doctor_specialty": "cardiologist", "city": "Astana", "preferred_date": "2026-10-07"}
	card := Values{"phone": "+77010000002", "client_id": "C002"}
	document := Values{"policy_number": "SQ-DMS-604220", "client_id": "C002", "document_type": "policy_duplicate", "email": "aigerim.b@mail.example"}
	callback := Values{"phone": "+77010000001", "callback_time": "2026-10-02T15:00"}
	for _, tc := range []struct {
		id, lang string
		slots    Values
		steps    []templateStep
		want     string
	}{
		{"SC01", "ru", ogpo, []templateStep{{"get_bm_class", nil}, {"calc_ogpo_price", nil}}, "Полис на год выйдет 30 400 тенге. Оформим?"},
		{"SC01", "kk", ogpo, []templateStep{{"get_bm_class", nil}, {"calc_ogpo_price", nil}}, "Бір жылдық полис 30 400 теңге болады. Рәсімдейміз бе?"},
		{"SC02", "ru", buy, []templateStep{{"calc_ogpo_price", nil}, {"create_policy", nil}, {"send_sms", nil}}, "Готово: полис SQ-OGPO-900001. Ссылку на оплату отправила в SMS, после оплаты полис сразу вступит в силу."},
		{"SC03", "kk", Values{"car_value": 12000000.0, "car_year": 2022.0, "franchise": 50000.0}, []templateStep{{"calc_casco_price", nil}, {"kb_lookup", Values{"topic": "products.casco"}}}, "Бір жылдық КАСКО Стандарт 540 000 теңге болады. Зақымды да, ұрлықты да қамтиды."},
		{"SC07", "ru", Values{"property_type": "house", "sum_insured": 10000000.0}, []templateStep{{"calc_property_price", nil}, {"kb_lookup", Values{"topic": "products.property"}}}, "На год выйдет 37 500 тенге. В покрытие входят пожар, затопление, кража и ответственность перед соседями."},
		// The new plate comes from the slot, not from get_policy.details (the old 777ABC02).
		{"SC05", "ru", plate, []templateStep{{"get_policy", nil}, {"update_policy", nil}}, "Готово, в полисе теперь машина 555KZZ02. Доплата 0 тенге."},
		{"SC25", "kk", policy, []templateStep{{"get_policy", nil}}, "SQ-OGPO-104501 полисі 14.03.2027 дейін жарамды."},
		{"SC25", "ru", policy, []templateStep{{"get_policy", nil}}, "Полис SQ-OGPO-104501 действует до 14.03.2027."},
		{"SC27", "ru", policy, []templateStep{{"get_policy", nil}, {"renew_policy", nil}, {"send_sms", nil}}, "Новый полис SQ-OGPO-900001 на год стоит 30 400 тенге, ссылка на оплату в SMS."},
		{"SC28", "ru", cancel, []templateStep{{"get_policy", nil}, {"cancel_policy", nil}}, "Договор расторгнут. К возврату 11 400 тенге, деньги придут на карту в течение 10 рабочих дней."},
		{"SC20", "ru", inspection, []templateStep{{"get_claim", nil}, {"book_inspection", nil}}, "Записала вас на 05.10.2026 10:00, адрес: Ryskulov Ave 200. Возьмите техпаспорт."},
		{"SC21", "kk", doctor, []templateStep{{"book_appointment", nil}}, "Сізді Saulet Medical емханасына 07.10.2026 10:00 уақытына жаздым. Жеке куәлігіңізді ала жүріңіз."},
		{"SC24", "kk", card, []templateStep{{"kb_lookup", Values{"topic": "products.dms.e_card"}}, {"send_sms", nil}}, "Картаны ***0002 нөміріне жібердім. Емханада экраннан көрсетсеңіз жеткілікті."},
		{"SC39", "ru", document, []templateStep{{"request_document", nil}}, "Документ отправлен на ***mple."},
		{"SC32", "kk", Values{"iin": "850314300121"}, []templateStep{{"get_bm_class", nil}, {"kb_lookup", Values{"topic": "bonus_malus"}}}, "Сіздің сыныбыңыз 7. Өз кінәңізбен апат болмаған әр жыл үшін сынып өседі, кінәлі ЖКО-дан кейін төмендейді."},
		{"SC36", "ru", callback, []templateStep{{"create_callback", nil}}, "Хорошо, перезвоним 02.10.2026 15:00."},
		{"SC35", "kk", Values{"complaint_text": "оператор дөрекі сөйледі"}, []templateStep{{"create_complaint", nil}}, "CMP-900001 шағымы тіркелді, ауысым жетекшісі бүгін хабарласады."},
		// The seed has no accident policy; the mock does not check the product on claims.
		{"SC16", "kk", Values{"policy_number": "SQ-OGPO-104501", "client_id": "C001", "product_type": "accident", "incident_date": "2026-09-27", "incident_description": "қолымды сындырдым", "phone": "+77010000001"}, []templateStep{{"get_policy", nil}, {"create_claim", nil}, {"send_sms", nil}}, "CL-900001 өтініші қабылданды. Травмпункттен анықтама керек болады, тізімді SMS-пен жібердім."},
	} {
		t.Run(tc.id+"_"+tc.lang, func(t *testing.T) {
			facts := templateCase(t, e, tc.id, tc.slots, tc.steps...)
			got, ok := e.template(&Session{Language: tc.lang}, facts, "completed")
			if !ok || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, ok, tc.want)
			}
			if strings.ContainsAny(got, "{}") || strings.Contains(got, "+7701") || strings.Contains(got, "@") {
				t.Fatalf("placeholder or unmasked contact left: %q", got)
			}
			// Facts restored from storage are plain JSON; the answer must not change.
			if again, ok := e.template(&Session{Language: tc.lang}, clone(facts), "completed"); !ok || again != got {
				t.Fatalf("restored facts gave %q, %v", again, ok)
			}
		})
	}
}

func TestTemplateDeclinesUnsafeClosings(t *testing.T) {
	e, _ := setup(t)
	ogpo := Values{"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121"}, "iin": "850314300121"}
	buy := Values{"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121"}, "vehicle_plate": "123ABC02", "phone": "+77010000001", "product_type": "ogpo", "price": 30400.0}
	claim := Values{"policy_number": "SQ-CASCO-204118", "client_id": "C001", "product_type": "casco", "incident_date": "2026-09-20", "incident_description": "угнали машину"}
	preview := ActionCall{Name: "create_policy", Mode: "preview", Inputs: buy, Result: Values{"confirmation_required": true}}
	for _, tc := range []struct {
		name  string
		facts Values
	}{
		{"KB answer SC40", templateCase(t, e, "SC40", Values{"topic": "исключения"}, templateStep{"kb_lookup", Values{"topic": "products.casco"}})},
		{"KB answer SC31", templateCase(t, e, "SC31", Values{}, templateStep{"kb_lookup", Values{"topic": "payments"}})},
		{"clinic list SC23", templateCase(t, e, "SC23", Values{"city": "Almaty", "phone": "+77010000001"}, templateStep{"list_clinics", nil}, templateStep{"send_sms", nil})},
		{"office hours SC33", templateCase(t, e, "SC33", Values{"city": "Almaty", "phone": "+77010000001"}, templateStep{"get_offices", nil}, templateStep{"send_sms", nil})},
		{"English claim status SC17", templateCase(t, e, "SC17", Values{"claim_number": "CL-500198", "client_id": "C001"}, templateStep{"get_claim", nil})},
		{"English coverage note SC22", templateCase(t, e, "SC22", Values{"policy_number": "SQ-DMS-604220", "client_id": "C002", "service_name": "МРТ"}, templateStep{"check_coverage", nil})},
		{"expired policy SC25", templateCase(t, e, "SC25", Values{"policy_number": "SQ-OGPO-102850", "client_id": "C003"}, templateStep{"get_policy", nil})},
		{"SMS skipped without phone SC02", templateCase(t, e, "SC02", buy, templateStep{"calc_ogpo_price", nil}, templateStep{"create_policy", nil})},
		{"errored booking SC20", templateCase(t, e, "SC20", Values{"claim_number": "CL-500198", "client_id": "C001", "city": "Almaty", "preferred_date": "2026-09-01"}, templateStep{"get_claim", nil}, templateStep{"book_inspection", nil})},
		{"errored renewal SC27", templateCase(t, e, "SC27", Values{"policy_number": "SQ-OGPO-999999", "phone": "+77010000001"}, templateStep{"get_policy", nil}, templateStep{"renew_policy", nil}, templateStep{"send_sms", nil})},
		{"transfer ran SC13", templateCase(t, e, "SC13", claim, templateStep{"get_policy", nil}, templateStep{"create_claim", nil}, templateStep{"transfer_to_operator", Values{"queue": "claims_team"}})},
		{"free-text callback time SC36", templateCase(t, e, "SC36", Values{"phone": "+77010000001", "callback_time": "завтра после обеда"}, templateStep{"create_callback", nil})},
		{"always handoff SC37", templateCase(t, e, "SC37", Values{}, templateStep{"transfer_to_operator", Values{"queue": "operator_general"}})},
		{"quote closing after purchase SC06", templateCase(t, e, "SC06", Values{"trip_country": "Turkey", "trip_start": "2026-10-10", "trip_end": "2026-10-17", "travelers_count": 2.0, "traveler_max_age": 40.0, "phone": "+77010000001", "product_type": "travel"}, templateStep{"calc_travel_price", nil})},
		{"unknown scenario", completedFacts(e.Catalog, "SC99", Values{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := e.template(&Session{Language: "ru"}, tc.facts, "completed"); ok {
				t.Fatalf("templated %q", got)
			}
		})
	}
	m := &mockTools{b: NewBackend(e.Catalog)}
	calc := m.run("calc_ogpo_price", buy)
	sms := m.run("send_sms", buy)
	if got, ok := e.template(&Session{Language: "ru"}, completedFacts(e.Catalog, "SC02", buy, calc, preview, sms), "completed"); ok {
		t.Fatalf("preview-only purchase templated %q", got)
	}
	done := templateCase(t, e, "SC01", ogpo, templateStep{"get_bm_class", nil}, templateStep{"calc_ogpo_price", nil})
	for _, status := range []string{"clarification", "awaiting_slot", "awaiting_confirmation", "handoff", ""} {
		if got, ok := e.template(&Session{Language: "ru"}, done, status); ok {
			t.Fatalf("status %q templated %q", status, got)
		}
	}
	if got, ok := e.template(&Session{Language: "en"}, done, "completed"); ok {
		t.Fatalf("unsupported language templated %q", got)
	}
}

func TestTemplateUsesLatestCallAfterRetry(t *testing.T) {
	e, _ := setup(t)
	m := &mockTools{b: NewBackend(e.Catalog)}
	bad := Values{"claim_number": "CL-500198", "client_id": "C001", "city": "Almaty", "preferred_date": "2026-09-01"}
	good := Values{"claim_number": "CL-500198", "client_id": "C001", "city": "Almaty", "preferred_date": "2026-10-06"}
	calls := []ActionCall{m.run("get_claim", good), m.run("book_inspection", bad), m.run("book_inspection", good)}
	if errorCode(calls[1].Result) == "" {
		t.Fatal("expected the first booking to fail")
	}
	got, ok := e.template(&Session{Language: "ru"}, completedFacts(e.Catalog, "SC20", good, calls...), "completed")
	if want := "Записала вас на 06.10.2026 10:00, адрес: Ryskulov Ave 200. Возьмите техпаспорт."; !ok || got != want {
		t.Fatalf("got %q, %v; want %q", got, ok, want)
	}
}

func TestTemplateOffersPendingTopic(t *testing.T) {
	e, _ := setup(t)
	bm := templateCase(t, e, "SC32", Values{"iin": "850314300121"}, templateStep{"get_bm_class", nil}, templateStep{"kb_lookup", Values{"topic": "bonus_malus"}})
	ru, ok := e.template(&Session{Language: "ru", Queue: []*Frame{newFrame("SC33")}}, bm, "completed")
	if !ok || !strings.HasPrefix(ru, "У вас класс 7.") || !strings.HasSuffix(ru, "по вашей вине. Вернёмся к вашему предыдущему вопросу?") {
		t.Fatalf("ru pending offer: %q, %v", ru, ok)
	}
	kk, ok := e.template(&Session{Language: "kk", Stack: []*Frame{newFrame("SC25")}}, bm, "completed")
	if !ok || !strings.HasSuffix(kk, "төмендейді. Алдыңғы сұрағыңызға оралайық па?") || strings.Contains(kk, "SC25") {
		t.Fatalf("kk pending offer: %q, %v", kk, ok)
	}
	if plain, _ := e.template(&Session{Language: "ru"}, bm, "completed"); strings.Contains(plain, "Вернёмся") {
		t.Fatalf("offer without pending topics: %q", plain)
	}
	// A closing that already asks something (SC01 in both languages, SC13 in
	// Kazakh) would leave two open questions for one "да".
	quote := templateCase(t, e, "SC01", Values{"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121"}}, templateStep{"get_bm_class", Values{"iin": "850314300121"}}, templateStep{"calc_ogpo_price", nil})
	claim := templateCase(t, e, "SC13", Values{"policy_number": "SQ-CASCO-204118", "client_id": "C001", "product_type": "casco", "incident_date": "2026-09-20", "incident_description": "царапина на двери"}, templateStep{"get_policy", nil}, templateStep{"create_claim", nil})
	for _, facts := range []Values{quote, claim} {
		for _, lang := range []string{"ru", "kk"} {
			pending := &Session{Language: lang, Queue: []*Frame{newFrame("SC33")}}
			if got, ok := e.template(pending, facts, "completed"); ok {
				t.Fatalf("offer after a closing question: %q", got)
			}
			if _, ok := e.template(&Session{Language: lang}, facts, "completed"); !ok {
				t.Fatalf("%s closing declined without pending topics", lang)
			}
		}
	}
}

func TestTemplateSpokenFormats(t *testing.T) {
	for n, want := range map[float64]string{0: "0", 999: "999", 1000: "1 000", 38000: "38 000", 1234567: "1 234 567", 1234.5: "1 234,5", -5000: "-5 000"} {
		if got := spokenAmount(n); got != want {
			t.Fatalf("spokenAmount(%v) = %q, want %q", n, got, want)
		}
	}
	for in, want := range map[string]string{"2026-10-05": "05.10.2026", "2026-10-05T10:00:00+05:00": "05.10.2026 10:00", "2026-10-02T15:00": "02.10.2026 15:00", "2026-10-02 09:30": "02.10.2026 09:30"} {
		if got, ok := spokenDate(in); !ok || got != want {
			t.Fatalf("spokenDate(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"завтра", "CL-500198", "10:00", "2026-13-01"} {
		if got, ok := spokenDate(in); ok {
			t.Fatalf("spokenDate(%q) = %q", in, got)
		}
	}
	for key, v := range map[string]any{"phone": "+77010000001", "iin": "850314300121", "email": "a@mail.example", "sent_to": "a@mail.example", "new_driver_iin": "920607400233"} {
		got, ok := spokenValue(key, v)
		if !ok || !strings.HasPrefix(got, "***") || len(got) != 7 {
			t.Fatalf("spokenValue(%s) = %q", key, got)
		}
	}
	for _, v := range []any{nil, "", true, []any{"x"}, map[string]any{"a": 1.0}} {
		if got, ok := spokenValue("x", v); ok {
			t.Fatalf("spokenValue(%v) = %q", v, got)
		}
	}
}

// The engine answers completed turns from the template without a Respond call,
// and still uses LLM wording where the closing is not templated.
func TestTemplateAnswersCompletedTurns(t *testing.T) {
	d := decision("SC32", Values{"iin": "850314300121"})
	d.Scenarios = append(d.Scenarios, Candidate{ScenarioID: "SC33", Confidence: .9, Reason: "second request in the same turn"})
	e, _ := setup(t, d, decision("SC33", Values{"city": "Almaty"}))
	o := process(t, e, input("one", "Какой у меня класс бонус-малус и где ваш офис?"))
	want := "У вас класс 7. Класс растёт за каждый год без аварий по вашей вине и снижается после ДТП по вашей вине. Вернёмся к вашему предыдущему вопросу?"
	if o.Status != "completed" || o.Trace.ResponseSource != "template" || o.Answer != want || !slices.Equal(o.PendingScenarios, []string{"SC33"}) {
		t.Fatalf("templated turn: %q %s %s %v", o.Answer, o.Status, o.Trace.ResponseSource, o.PendingScenarios)
	}
	o = process(t, e, input("two", "Да, где офис?"))
	if o.Status != "completed" || o.Trace.ResponseSource != "llm" || o.Answer != "Ответ на основе результатов." {
		t.Fatalf("untemplated turn: %q %s %s", o.Answer, o.Status, o.Trace.ResponseSource)
	}
}

// Every allowlisted scenario that the mock seed can complete renders its
// closing end to end, in both languages, with facts built by the executor.
func TestTemplateEveryAllowlistedScenarioEndToEnd(t *testing.T) {
	flows := map[string]Values{
		"SC01": {"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121"}},
		"SC02": {"region": "almaty", "vehicle_type": "car", "vehicle_plate": "123ABC02", "drivers_iin": []any{"850314300121"}, "phone": "+77010000001"},
		"SC03": {"car_value": 12000000, "car_year": 2022, "franchise": 50000},
		"SC05": {"phone": "+77010000001", "policy_number": "SQ-OGPO-104501", "vehicle_plate": "555KZZ02"},
		"SC07": {"property_type": "apartment", "sum_insured": 10000000},
		"SC08": {"sum_insured": 3000000},
		"SC11": {"injured": false, "location": "перекрёсток Абая и Сейфуллина", "phone": "+77010000001"},
		"SC12": {"culprit_vehicle_plate": "777ABC02", "incident_date": "2026-09-28", "incident_description": "въехали в заднюю дверь", "phone": "+77010000005"},
		"SC13": {"phone": "+77010000001", "policy_number": "SQ-CASCO-204118", "incident_date": "2026-09-20", "incident_description": "царапина на двери"},
		"SC14": {"phone": "+77010000004", "policy_number": "SQ-PROP-404077", "incident_date": "2026-09-25", "incident_description": "затопили соседи"},
		"SC19": {"phone": "+77010000004", "claim_number": "CL-500311", "complaint_text": "не согласен с запросом документов"},
		"SC20": {"phone": "+77010000001", "claim_number": "CL-500198", "city": "Almaty", "preferred_date": "2026-10-05"},
		"SC21": {"phone": "+77010000002", "policy_number": "SQ-DMS-604220", "doctor_specialty": "cardiologist", "city": "Astana", "preferred_date": "2026-10-07"},
		"SC24": {"phone": "+77010000002"},
		"SC25": {"phone": "+77010000001", "policy_number": "SQ-OGPO-104501"},
		"SC26": {"phone": "+77010000002"},
		"SC27": {"phone": "+77010000001", "policy_number": "SQ-OGPO-104501"},
		"SC28": {"phone": "+77010000001", "policy_number": "SQ-OGPO-104501", "cancel_reason": "продал машину"},
		"SC29": {"phone": "+77010000003", "contact_field": "email", "new_value": "new@mail.example"},
		"SC32": {"iin": "850314300121"},
		"SC35": {"complaint_text": "оператор был груб"},
		"SC36": {"phone": "+77010000001", "callback_time": "2026-10-02T15:00"},
		"SC38": {"fraud_details": "звонили от имени компании и просили код из SMS"},
		"SC39": {"phone": "+77010000002", "policy_number": "SQ-DMS-604220", "document_type": "policy_duplicate", "email": "aigerim.b@mail.example"},
	}
	// SC16 is left out: the mock seed has no accident policy to claim against.
	for _, id := range templated {
		if _, ok := flows[id]; !ok && id != "SC16" {
			t.Fatalf("%s has no end-to-end flow", id)
		}
	}
	for id, slots := range flows {
		for _, lang := range []string{"ru", "kk"} {
			t.Run(id+"_"+lang, func(t *testing.T) {
				first, yes := decision(id, clone(slots)), decision(id, Values{})
				first.Language, yes.Language = lang, lang
				e, _ := setup(t, first, yes)
				in := input("one", "запрос клиента")
				in.Language = lang
				o := process(t, e, in)
				if o.Status == "awaiting_confirmation" {
					in.RequestID, in.Text = "two", local(lang, "Да", "Иә")
					o = process(t, e, in)
				}
				if o.Status != "completed" || o.Trace.ResponseSource != "template" {
					if o.Trace.Uncertainty != nil {
						t.Logf("uncertainty %+v shortlist %v", *o.Trace.Uncertainty, o.Trace.Shortlist)
					}
					t.Fatalf("%s %s: %q (%s, %s, %s)", id, lang, o.Answer, o.Status, o.Trace.ResponseSource, o.Trace.Error)
				}
				// Every fixed fragment of this language's closing is in the answer.
				for _, part := range closingPlaceholder.Split(e.Catalog.Scenarios[id].Responses[lang]["closing"], -1) {
					if !strings.Contains(o.Answer, part) {
						t.Fatalf("%s %s: %q lacks %q", id, lang, o.Answer, part)
					}
				}
				if strings.ContainsAny(o.Answer, "{}") || strings.Contains(o.Answer, "+7701") || strings.Contains(o.Answer, "@") {
					t.Fatalf("%s %s: %q", id, lang, o.Answer)
				}
				t.Log(o.Answer)
			})
		}
	}
}

func BenchmarkTemplateCompleted(b *testing.B) {
	c, err := LoadCatalog()
	if err != nil {
		b.Fatal(err)
	}
	e := &Engine{Catalog: c}
	m := &mockTools{b: NewBackend(c)}
	slots := Values{"policy_number": "SQ-OGPO-104501", "client_id": "C001", "cancel_reason": "продал машину"}
	facts := completedFacts(c, "SC28", slots, m.run("get_policy", slots), m.run("cancel_policy", slots))
	s := &Session{Language: "ru"}
	for b.Loop() {
		if _, ok := e.template(s, facts, "completed"); !ok {
			b.Fatal("not templated")
		}
	}
}
