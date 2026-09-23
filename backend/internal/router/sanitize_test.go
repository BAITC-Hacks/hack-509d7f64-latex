package router

import (
	"slices"
	"testing"
)

func TestSanitizeSlotsKeepsDecisionUsable(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	v := Values{"region": "Almaty", "vehicle_type": "car", "car_value": nil, "vehicle_plate": "not a plate", "franchise": "50000", "iin": "910512300456"}
	dropped := c.sanitizeSlots(v)
	if !slices.Equal(dropped, []string{"car_value", "vehicle_plate"}) {
		t.Fatalf("dropped %v", dropped)
	}
	if v["region"] != "almaty" || v["franchise"] != float64(50000) || v["iin"] != "910512300456" {
		t.Fatalf("not canonicalised: %v", v)
	}
	if err := c.ValidateSlots(v); err != nil {
		t.Fatal(err)
	}
}

func TestMaskAnswer(t *testing.T) {
	for in, want := range map[string]string{
		"ИИН 910512300456 и 930824400789.":      "ИИН ***0456 и ***0789.",
		"Телефон +7 701 234 56 78, пишите.":     "Телефон ***5678, пишите.",
		"Номер +77010000010 подтверждён":        "Номер ***0010 подтверждён",
		"Почта aigerim@mail.kz":                 "Почта ***@mail.kz",
		"Полис SQ-OGPO-123456 на 38 000 тенге.": "Полис SQ-OGPO-123456 на 38 000 тенге.",
	} {
		if got := maskAnswer(in); got != want {
			t.Errorf("maskAnswer(%q) = %q, want %q", in, got, want)
		}
	}
}
