package router

import (
	"strings"
	"testing"
)

func TestIntentQuestionUsesProposedFactsInsteadOfExamples(t *testing.T) {
	e, _ := setup(t)
	for _, lang := range []string{"ru", "kk"} {
		question := e.intentQuestion(lang, decision("SC33", Values{"city": "Astana"}))
		if !strings.Contains(question, "Astana") || strings.Contains(strings.ToLower(question), "алмат") || strings.Contains(question, "Almaty") {
			t.Fatal("office review must name actual proposed city", lang, question)
		}
		question = e.intentQuestion(lang, decision("SC29", Values{"contact_field": "email", "new_value": "new-mail@example.com"}))
		fieldLabel := local(lang, "изменяемое поле: электронная почта", "өзгертілетін дерек: электрондық пошта")
		if !strings.Contains(question, fieldLabel) || strings.Contains(question, "телефон") || strings.Contains(question, "new-mail") {
			t.Fatal("contact review must identify the proposed field and mask its value", lang, question)
		}
		if !strings.Contains(question, "***@example.com") {
			t.Fatal("masked value should retain the supplied domain", question)
		}
	}
}

func TestIntentQuestionCoversEveryAcceptedIntent(t *testing.T) {
	e, _ := setup(t)
	d := Decision{Scenarios: []Candidate{
		{ScenarioID: "SC33", Confidence: .95},
		{ScenarioID: "SC29", Confidence: .75},
		{ScenarioID: "SC35", Confidence: .74},
	}, Alternatives: []Candidate{{ScenarioID: "SC34", Confidence: .90}}}
	for _, lang := range []string{"ru", "kk"} {
		question := e.intentQuestion(lang, d)
		for _, id := range []string{"SC33", "SC29"} {
			label := reviewScenarioLabels[id]
			if !strings.Contains(question, local(lang, label.ru, label.kk)) {
				t.Fatal("accepted scenario omitted", id, question)
			}
		}
		for _, id := range []string{"SC35", "SC34"} {
			label := reviewScenarioLabels[id]
			if strings.Contains(question, local(lang, label.ru, label.kk)) {
				t.Fatal("unaccepted scenario included", id, question)
			}
		}
	}
}

func TestIntentQuestionMasksIdentifiersAndContactChanges(t *testing.T) {
	e, _ := setup(t)
	d := decision("SC29", Values{
		"phone": "+77010001234", "iin": "910101345678", "new_driver_iin": "930303777888",
		"drivers_iin": []any{"940404112233", "950505445566"}, "email": "private@example.net",
		"contact_field": "address", "new_value": "Private street 125",
		"complaint_text": "Contact private@example.net or +77010001234 about 910101345678",
	})
	question := e.intentQuestion("ru", d)
	for _, secret := range []string{"+77010001234", "910101345678", "930303777888", "940404112233", "950505445566", "private@example.net", "Private street 125"} {
		if strings.Contains(question, secret) {
			t.Fatal("unmasked sensitive slot", secret, question)
		}
	}
	for _, partial := range []string{"***1234", "***5678", "***7888", "***2233", "***5566", "***@example.net", "[скрыто]"} {
		if !strings.Contains(question, partial) {
			t.Fatal("missing masked identifier", partial, question)
		}
	}
	d.Slots["contact_field"], d.Slots["new_value"] = "phone", "+77015559876"
	question = e.intentQuestion("kk", d)
	if !strings.Contains(question, "***9876") || strings.Contains(question, "+77015559876") {
		t.Fatal("new phone must also be masked", question)
	}
}

func TestIntentQuestionStableSlotOrderAndFaithfulValues(t *testing.T) {
	e, _ := setup(t)
	d := decision("SC11", Values{"trip_start": "2026-10-01", "injured": false, "city": "Astana", "payment_amount": float64(42000)})
	first := e.intentQuestion("ru", d)
	if !strings.Contains(first, "город: Astana; есть пострадавшие: нет; сумма оплаты, ₸: 42000; начало поездки: 2026-10-01") {
		t.Fatal("slot display must preserve proposed values in stable order", first)
	}
	for i := 0; i < 20; i++ {
		if question := e.intentQuestion("ru", d); question != first {
			t.Fatal("map iteration changed question", first, question)
		}
	}
}

func TestIntentQuestionCatalogLabelsComplete(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for id := range c.Scenarios {
		label, ok := reviewScenarioLabels[id]
		if !ok || label.ru == "" || label.kk == "" {
			t.Fatal("missing bilingual scenario label", id)
		}
	}
	for name := range c.Slots {
		label, ok := reviewSlotLabels[name]
		if !ok || label.ru == "" || label.kk == "" {
			t.Fatal("missing bilingual slot label", name)
		}
	}
}

func TestIntentQuestionBoundsFreeTextWithoutChangingUnicode(t *testing.T) {
	e, _ := setup(t)
	d := decision("SC35", Values{"complaint_text": strings.Repeat("Ж", 120) + "\nmore"})
	question := e.intentQuestion("kk", d)
	if !strings.Contains(question, strings.Repeat("Ж", 96)+"…") || strings.Contains(question, strings.Repeat("Ж", 97)) || strings.Contains(question, "\n") {
		t.Fatal("free text must be bounded by runes and displayed on one line", question)
	}
}
