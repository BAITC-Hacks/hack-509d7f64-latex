package router

import (
	"fmt"
	"testing"
)

func backend(t *testing.T) *Backend {
	t.Helper()
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return NewBackend(c)
}
func TestPricingUsesDataset(t *testing.T) {
	tests := []struct {
		name  string
		args  Values
		price float64
	}{
		{"calc_ogpo_price", Values{"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121"}}, 30400},
		{"calc_ogpo_price", Values{"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"850314300121", "890922300345"}}, 45600},
		{"calc_casco_price", Values{"car_value": 12000000, "car_year": 2019, "franchise": "50000"}, 540000},
		{"calc_travel_price", Values{"trip_country": "Turkey", "trip_start": "2026-10-02", "trip_end": "2026-10-08", "travelers_count": 2, "traveler_max_age": 70}, 30800},
		{"calc_property_price", Values{"property_type": "house", "sum_insured": "10000000"}, 37500},
		{"calc_accident_price", Values{"sum_insured": "3000000"}, 15000},
	}
	b := backend(t)
	for i, tt := range tests {
		t.Run(fmt.Sprintf("%s-%d", tt.name, i), func(t *testing.T) {
			r := b.Execute(tt.name, tt.args, "")
			if errorCode(r) != "" || number(r["price"]) != tt.price {
				t.Fatalf("want %v, got %v", tt.price, r)
			}
		})
	}
}
func TestBackendRejectsWrongOwnerAndInactivePolicy(t *testing.T) {
	b := backend(t)
	r := b.Execute("get_policy", Values{"policy_number": "SQ-OGPO-104501", "client_id": "C003"}, "")
	if errorCode(r) != "not_found" {
		t.Fatal(r)
	}
	r = b.Execute("get_claim", Values{"claim_number": "CL-500287", "client_id": "C001"}, "")
	if errorCode(r) != "not_found" {
		t.Fatal(r)
	}
	p := b.find("policies", "policy_number", "SQ-OGPO-104501")
	p["end_date"] = "2026-09-01"
	r = b.Execute("update_policy", Values{"policy_number": "SQ-OGPO-104501", "new_driver_iin": "920607400233"}, "update")
	if errorCode(r) != "policy_inactive" {
		t.Fatal(r)
	}
}
func TestBackendMutationIdempotency(t *testing.T) {
	b := backend(t)
	args := Values{"phone": "+77010000001", "product_type": "ogpo", "price": 30400}
	r := b.Execute("create_policy", args, "same")
	again := b.Execute("create_policy", args, "same")
	if !equalJSON(r, again) || len(b.rows("policies")) != 12 {
		t.Fatalf("duplicate mutation: %v %v", r, again)
	}
	if !b.c.ValidID("SC40") {
		t.Fatal("catalog incomplete")
	}
}
func TestPolicyCancellationRefundAndPaidClaim(t *testing.T) {
	b := backend(t)
	r := b.Execute("cancel_policy", Values{"policy_number": "SQ-CASCO-204118", "cancel_reason": "sold"}, "paid")
	if errorCode(r) != "not_eligible" {
		t.Fatal(r)
	}
	r = b.Execute("cancel_policy", Values{"policy_number": "SQ-OGPO-104501", "cancel_reason": "sold"}, "cancel")
	if number(r["refund_amount"]) != 11400 {
		t.Fatal(r)
	}
	r = b.Execute("cancel_policy", Values{"policy_number": "SQ-OGPO-104501", "cancel_reason": "sold"}, "again")
	if errorCode(r) != "already_done" {
		t.Fatal(r)
	}
}
func TestMockBookingAvailabilityAndCoverage(t *testing.T) {
	b := backend(t)
	args := Values{"policy_number": "SQ-DMS-604220", "doctor_specialty": "therapist", "city": "Astana", "preferred_date": "2026-10-03"}
	first := b.Execute("book_appointment", args, "first")
	if errorCode(first) != "" || first["mock"] != true {
		t.Fatal(first)
	}
	second := b.Execute("book_appointment", args, "second")
	if errorCode(second) != "no_availability" || second["nearest_date"] != "2026-10-04" {
		t.Fatal(second)
	}
	r := b.Execute("check_coverage", Values{"policy_number": "SQ-DMS-604220", "service_name": "мрт"}, "")
	if r["covered"] != true || r["rules"] == nil {
		t.Fatal(r)
	}
}
func TestAllCatalogActionsHaveConcreteImplementations(t *testing.T) {
	args := map[string]Values{
		"find_client": {"phone": "+77010000001"}, "get_policies": {"client_id": "C001"}, "get_policy": {"policy_number": "SQ-OGPO-104501"}, "get_bm_class": {"iin": "111111111111"},
		"calc_ogpo_price": {"region": "almaty", "vehicle_type": "car", "drivers_iin": []any{"111111111111"}}, "calc_casco_price": {"car_value": 10000000, "car_year": 2020, "franchise": "0"}, "calc_travel_price": {"trip_country": "Turkey", "trip_start": "2026-10-02", "trip_end": "2026-10-04", "travelers_count": 1, "traveler_max_age": 30}, "calc_property_price": {"property_type": "apartment", "sum_insured": "5000000"}, "calc_accident_price": {"sum_insured": "1000000"},
		"create_policy": {"phone": "+77010000001", "product_type": "ogpo"}, "renew_policy": {"policy_number": "SQ-OGPO-104501"}, "update_policy": {"policy_number": "SQ-OGPO-104501", "new_driver_iin": "111111111111"}, "cancel_policy": {"policy_number": "SQ-OGPO-104501", "cancel_reason": "sold"}, "create_claim": {"policy_number": "SQ-OGPO-104501", "product_type": "ogpo", "incident_date": "2026-09-30", "incident_description": "collision"}, "get_claim": {"claim_number": "CL-500198"}, "create_dispute": {"claim_number": "CL-500287", "complaint_text": "low payout"},
		"book_inspection": {"claim_number": "CL-500287", "city": "Almaty", "preferred_date": "2026-10-02"}, "book_appointment": {"policy_number": "SQ-DMS-604220", "doctor_specialty": "therapist", "city": "Astana", "preferred_date": "2026-10-02"}, "check_coverage": {"policy_number": "SQ-DMS-604220", "service_name": "therapist"}, "list_clinics": {"city": "Almaty"}, "resend_documents": {"policy_number": "SQ-OGPO-104501"}, "check_payment": {"client_id": "C003", "payment_date": "2026-09-30"}, "update_contact": {"client_id": "C001", "contact_field": "email", "new_value": "new@mail.example"}, "request_document": {"policy_number": "SQ-OGPO-104501", "document_type": "policy_duplicate", "email": "copy@mail.example"}, "get_offices": {"city": "Almaty"}, "kb_lookup": {"topic": "payments"}, "send_sms": {"phone": "+77010000001"}, "create_callback": {"phone": "+77010000001", "callback_time": "tomorrow"}, "create_complaint": {"complaint_text": "slow"}, "report_fraud": {"fraud_details": "code requested"}, "transfer_to_operator": {"queue": "operator_general"},
	}
	c, _ := LoadCatalog()
	for name := range c.Actions {
		t.Run(name, func(t *testing.T) {
			a, ok := args[name]
			if !ok {
				t.Fatal("missing action fixture")
			}
			r := backend(t).Execute(name, a, name)
			if errorCode(r) != "" {
				t.Fatalf("action failed: %v", r)
			}
		})
	}
}
