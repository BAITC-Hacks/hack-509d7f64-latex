package router

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// Backend implements synthetic actions only: no real policies, calls, SMS or money.
type Backend struct {
	mu       sync.Mutex
	c        *Catalog
	data     Values
	receipts map[string]Values
	seq      int
}

func NewBackend(c *Catalog) *Backend {
	return &Backend{c: c, data: clone(c.Seed), receipts: map[string]Values{}, seq: mockSequenceStart}
}
func failure(code, msg string) Values { return Values{"error": Values{"code": code, "message": msg}} }
func errorCode(v Values) string       { return str(asMap(v["error"])["code"]) }
func asMap(v any) Values {
	switch m := v.(type) {
	case map[string]any:
		return m
	case Values:
		return m
	}
	return Values{}
}
func list(v any) []any { a, _ := v.([]any); return a }
func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	var f float64
	_, _ = fmt.Sscan(str(v), &f)
	return f
}
func path(v Values, keys ...string) any {
	var x any = v
	for _, k := range keys {
		x = asMap(x)[k]
	}
	return x
}
func (b *Backend) rows(table string) []any { return list(b.data[table]) }
func (b *Backend) find(table, key string, value any) Values {
	if str(value) == "" {
		return nil
	}
	for _, row := range b.rows(table) {
		m := asMap(row)
		if str(m[key]) == str(value) {
			return m
		}
	}
	return nil
}
func (b *Backend) append(table string, row Values) { b.data[table] = append(b.rows(table), row) }
func (b *Backend) next(prefix string) string       { b.seq++; return fmt.Sprintf("%s%06d", prefix, b.seq) }
func (b *Backend) policy(a Values) Values {
	if p := b.find("policies", "policy_number", a["policy_number"]); p != nil {
		return p
	}
	if has(a, "vehicle_plate") {
		for _, row := range b.rows("policies") {
			p := asMap(row)
			if path(p, "details", "vehicle_plate") == a["vehicle_plate"] {
				if has(a, "product_type") && p["product"] != a["product_type"] {
					continue
				}
				return p
			}
		}
	}
	return nil
}
func (b *Backend) status(p Values) string {
	if s := str(p["status"]); s != "" {
		return s
	}
	today := b.c.Today.Format(time.DateOnly)
	if str(p["start_date"]) > today {
		return "not_yet_active"
	}
	if str(p["end_date"]) < today {
		return "expired"
	}
	return "active"
}
func (b *Backend) bm(iin any) string {
	if c := b.find("clients", "iin", iin); c != nil {
		return str(c["bm_class"])
	}
	return "3"
}
func (b *Backend) Execute(name string, a Values, key string) Values {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.receipts[key]; ok {
		return clone(r)
	}
	action, ok := b.c.Actions[name]
	if !ok {
		return failure("invalid_input", "Unknown action")
	}
	for _, group := range action.Inputs {
		present := false
		for _, k := range strings.Split(group, "|") {
			present = present || has(a, k)
		}
		if !present {
			return failure("invalid_input", "Missing "+group)
		}
	}
	r := b.execute(name, a)
	// Read-only results must not be reused after a retry with corrected arguments.
	if errorCode(r) == "" && (action.Irreversible || slices.Contains([]string{"send_sms", "create_callback", "create_complaint", "report_fraud", "transfer_to_operator", "resend_documents", "request_document"}, name)) {
		b.receipts[key] = clone(r)
	}
	return clone(r)
}
func (b *Backend) execute(name string, a Values) Values {
	p := b.policy(a)
	if p != nil && has(a, "client_id") && p["client_id"] != a["client_id"] && a["victim_lookup"] != true {
		return failure("not_found", "Policy does not belong to identified client")
	}
	switch name {
	case "find_client":
		c := b.find("clients", "phone", a["phone"])
		if c == nil {
			c = b.find("clients", "iin", a["iin"])
		}
		if c == nil {
			return failure("not_found", "Client not found")
		}
		return clone(c)
	case "get_policies":
		policies := []any{}
		for _, r := range b.rows("policies") {
			m := asMap(r)
			if m["client_id"] == a["client_id"] {
				v := clone(m)
				v["status"] = b.status(m)
				policies = append(policies, v)
			}
		}
		if len(policies) == 0 {
			return failure("not_found", "No policies")
		}
		return Values{"policies": policies}
	case "get_policy":
		if p == nil {
			return failure("not_found", "Policy not found")
		}
		r := clone(p)
		r["status"] = b.status(p)
		if a["victim_lookup"] == true {
			return Values{"policy_number": p["policy_number"], "product": p["product"], "status": r["status"], "end_date": p["end_date"]}
		}
		return r
	case "get_bm_class":
		return Values{"bm_class": b.bm(a["iin"])}
	case "calc_ogpo_price":
		pricing := asMap(path(b.c.KB, "products", "ogpo", "pricing"))
		base := number(path(pricing, "base_by_region_kzt", str(a["region"])))
		vehicle := number(path(pricing, "vehicle_type_coef", str(a["vehicle_type"])))
		worst := 0.0
		for _, iin := range list(a["drivers_iin"]) {
			worst = math.Max(worst, number(path(pricing, "bm_coef", b.bm(iin))))
		}
		if base == 0 || vehicle == 0 || worst == 0 {
			return failure("invalid_input", "Invalid OGPO pricing parameters")
		}
		return Values{"price": math.Round(base * vehicle * worst), "term_months": 12}
	case "calc_casco_price":
		age := b.c.Today.Year() - int(number(a["car_year"]))
		if age < 0 {
			return failure("invalid_input", "Car year is in the future")
		}
		if age > 10 {
			return failure("not_eligible", "Standard CASCO covers vehicles up to 10 years old")
		}
		rate := 0.04
		if age > 3 {
			rate = .05
		}
		if age > 7 {
			rate = .065
		}
		coef := number(path(b.c.KB, "products", "casco", "pricing", "franchise_coef", str(a["franchise"])))
		if coef == 0 || number(a["car_value"]) <= 0 {
			return failure("invalid_input", "Invalid car value or franchise")
		}
		return Values{"price": math.Round(number(a["car_value"]) * rate * coef), "package": "Standard"}
	case "calc_travel_price":
		return b.travel(a)
	case "calc_property_price", "calc_accident_price":
		product := "accident"
		if name == "calc_property_price" {
			product = "property"
		}
		price := number(path(b.c.KB, "products", product, "price_per_year_kzt", str(a["sum_insured"])))
		if price == 0 {
			return failure("not_eligible", "Unsupported sum insured for this product")
		}
		if a["property_type"] == "house" {
			price *= number(path(b.c.KB, "products", "property", "house_coef"))
		}
		return Values{"price": price}
	case "get_claim":
		r := b.find("claims", "claim_number", a["claim_number"])
		if r == nil && !has(a, "claim_number") {
			r = b.find("claims", "client_id", a["client_id"])
		}
		if r == nil || (has(a, "client_id") && r["client_id"] != a["client_id"]) {
			return failure("not_found", "Claim not found for client")
		}
		return clone(r)
	case "check_payment":
		for _, row := range b.rows("payments") {
			r := asMap(row)
			if r["client_id"] == a["client_id"] && r["date"] == a["payment_date"] {
				v := clone(r)
				v["payment_status"] = r["status"]
				return v
			}
		}
		return failure("not_found", "Payment not found")
	case "get_offices", "list_clinics":
		table, key := "offices", "offices"
		if name == "list_clinics" {
			table = "clinics"
			key = "clinics"
		}
		rows := []any{}
		for _, row := range list(b.c.KB[table]) {
			if strings.EqualFold(str(asMap(row)["city"]), str(a["city"])) {
				rows = append(rows, row)
			}
		}
		if len(rows) == 0 {
			return failure("not_found", "No locations for this city")
		}
		return Values{key: rows}
	case "kb_lookup":
		if a["topic"] == "all" {
			return Values{"answer": b.c.KB}
		}
		var result any = b.c.KB
		for _, key := range strings.Split(str(a["topic"]), ".") {
			result = asMap(result)[key]
			if result == nil {
				return failure("not_found", "Knowledge topic not found")
			}
		}
		return Values{"answer": result}
	case "check_coverage":
		return b.coverage(p, a)
	case "create_policy":
		product := str(a["product_type"])
		prefix := map[string]string{"ogpo": "OGPO", "casco": "CASCO", "travel": "TRVL", "property": "PROP", "accident": "NS", "dms": "DMS"}[product]
		if prefix == "" {
			return failure("invalid_input", "Unknown product")
		}
		client := b.find("clients", "phone", a["phone"])
		if client == nil {
			client = Values{"client_id": b.next("C"), "phone": a["phone"]}
			b.append("clients", client)
		}
		r := Values{"policy_number": b.next("SQ-" + prefix + "-"), "client_id": client["client_id"], "product": product, "status": "pending_payment", "start_date": b.c.Today.Format(time.DateOnly), "end_date": b.c.Today.AddDate(1, 0, -1).Format(time.DateOnly), "premium": a["price"], "details": clone(a)}
		b.append("policies", r)
		return Values{"policy_number": r["policy_number"], "status": "pending_payment", "mock": true}
	case "renew_policy":
		if p == nil {
			return failure("not_found", "Policy not found")
		}
		if b.status(p) == "cancelled" {
			return failure("not_eligible", "Cancelled policy cannot be renewed")
		}
		r := clone(p)
		r["policy_number"] = b.next("SQ-" + strings.ToUpper(str(p["product"])) + "-")
		r["status"] = "pending_payment"
		start, _ := time.Parse(time.DateOnly, str(p["end_date"]))
		start = start.AddDate(0, 0, 1)
		if start.Before(b.c.Today) {
			start = b.c.Today
		}
		r["start_date"] = start.Format(time.DateOnly)
		r["end_date"] = start.AddDate(1, 0, -1).Format(time.DateOnly)
		b.append("policies", r)
		return Values{"policy_number": r["policy_number"], "price": r["premium"], "status": "pending_payment", "mock": true}
	case "update_policy":
		if p == nil {
			return failure("not_found", "Policy not found")
		}
		if b.status(p) != "active" {
			return failure("policy_inactive", "Policy is not active")
		}
		details := asMap(p["details"])
		if has(a, "new_driver_iin") {
			drivers := list(details["drivers_iin"])
			if slices.ContainsFunc(drivers, func(v any) bool { return v == a["new_driver_iin"] }) {
				return failure("already_done", "Driver already listed")
			}
			details["drivers_iin"] = append(drivers, a["new_driver_iin"])
		} else if has(a, "vehicle_plate") {
			details["vehicle_plate"] = a["vehicle_plate"]
		} else {
			return failure("invalid_input", "Missing policy change")
		}
		p["details"] = details
		return Values{"extra_premium": 0, "mock": true}
	case "cancel_policy":
		if p == nil {
			return failure("not_found", "Policy not found")
		}
		if b.status(p) == "cancelled" {
			return failure("already_done", "Policy already cancelled")
		}
		if b.status(p) != "active" {
			return failure("policy_inactive", "Policy not active")
		}
		for _, row := range b.rows("claims") {
			r := asMap(row)
			if r["policy_number"] == p["policy_number"] && r["status"] == "paid" {
				return failure("not_eligible", "A claim was paid under this policy")
			}
		}
		end, _ := time.Parse(time.DateOnly, str(p["end_date"]))
		months := 0
		for b.c.Today.AddDate(0, months+1, 0).Compare(end) <= 0 {
			months++
		}
		refund := math.Round(number(p["premium"]) * float64(months) / 12 * .9)
		p["status"] = "cancelled"
		return Values{"refund_amount": refund, "mock": true}
	case "create_claim":
		if p == nil {
			return failure("not_found", "Policy not found")
		}
		incident, e := time.Parse(time.DateOnly, str(a["incident_date"]))
		if e != nil || incident.After(b.c.Today) {
			return failure("invalid_input", "Invalid incident date")
		}
		if str(a["incident_date"]) < str(p["start_date"]) || str(a["incident_date"]) > str(p["end_date"]) {
			return failure("policy_inactive", "Policy inactive on incident date")
		}
		r := Values{"claim_number": b.next("CL-"), "client_id": a["client_id"], "policy_number": p["policy_number"], "claim_type": a["product_type"], "incident_date": a["incident_date"], "incident_description": a["incident_description"], "status": "registered", "next_step": "Submit claim documents"}
		b.append("claims", r)
		return Values{"claim_number": r["claim_number"], "mock": true}
	case "update_contact":
		c := b.find("clients", "client_id", a["client_id"])
		if c == nil {
			return failure("not_found", "Client not found")
		}
		field := str(a["contact_field"])
		if !slices.Contains([]string{"phone", "email", "address"}, field) {
			return failure("invalid_input", "Unknown contact field")
		}
		if field != "address" {
			if e := b.c.ValidateSlots(Values{field: a["new_value"]}); e != nil {
				return failure("invalid_input", "Invalid new contact")
			}
		}
		c[field] = a["new_value"]
		return Values{"updated": true, "mock": true}
	case "create_dispute":
		claim := b.find("claims", "claim_number", a["claim_number"])
		if claim == nil || (has(a, "client_id") && claim["client_id"] != a["client_id"]) {
			return failure("not_found", "Claim not found")
		}
		return b.record("disputes", "DSP-", a)
	case "book_inspection", "book_appointment":
		return b.book(name, p, a)
	case "resend_documents", "request_document":
		if p == nil {
			return failure("not_found", "Policy not found")
		}
		c := b.find("clients", "client_id", p["client_id"])
		to := a["email"]
		if !has(a, "email") {
			to = c["email"]
		}
		if str(to) == "" {
			to = c["phone"]
		}
		if str(to) == "" {
			return failure("invalid_input", "No delivery contact")
		}
		return Values{"sent_to": to, "mock": true}
	case "send_sms":
		return Values{"sent_to": a["phone"], "mock": true}
	case "create_callback":
		return b.record("callbacks", "CB-", a)
	case "create_complaint":
		return b.record("complaints", "CMP-", a)
	case "report_fraud":
		return b.record("fraud_reports", "FR-", a)
	case "transfer_to_operator":
		if !slices.Contains(b.c.Queues, str(a["queue"])) {
			return failure("invalid_input", "Unknown queue")
		}
		r := b.record("handoffs", "HO-", a)
		r["queue"] = a["queue"]
		r["context"] = a["context"]
		return r
	}
	return failure("service_unavailable", "Action unavailable")
}
func (b *Backend) record(table, prefix string, a Values) Values {
	r := clone(a)
	r["ticket_id"] = b.next(prefix)
	b.append(table, r)
	return Values{"ticket_id": r["ticket_id"], "mock": true}
}
func (b *Backend) travel(a Values) Values {
	start, e1 := time.Parse(time.DateOnly, str(a["trip_start"]))
	end, e2 := time.Parse(time.DateOnly, str(a["trip_end"]))
	if e1 != nil || e2 != nil || start.Before(b.c.Today) || end.Before(start) || number(a["travelers_count"]) < 1 {
		return failure("invalid_input", "Invalid trip dates or travelers")
	}
	age := number(a["traveler_max_age"])
	if age > 75 {
		return failure("not_eligible", "Over 75: operator required")
	}
	country := strings.ToLower(str(a["trip_country"]))
	zone := ""
	groups := map[string][]string{"A": {"russia", "россия", "ресей", "georgia", "грузия", "uzbekistan", "узбекистан", "өзбекстан", "kyrgyzstan", "кыргызстан", "қырғызстан", "armenia", "армения", "azerbaijan", "азербайджан", "belarus", "беларусь", "tajikistan", "таджикистан"}, "B": {"germany", "германия", "france", "франция", "italy", "италия", "spain", "испания", "uk", "united kingdom", "великобритания", "ұлыбритания", "poland", "польша", "greece", "греция", "austria", "австрия"}, "C": {"turkey", "турция", "түркия", "uae", "оаэ", "баә", "thailand", "таиланд", "тайланд", "egypt", "египет", "мысыр"}, "D": {"usa", "сша", "ақш", "canada", "канада"}}
	for z, countries := range groups {
		if slices.Contains(countries, country) {
			zone = z
		}
	}
	if zone == "" {
		return failure("not_eligible", "Country zone needs operator verification")
	}
	z := asMap(path(b.c.KB, "products", "travel", "zones", zone))
	coef := 1.0
	if age >= 65 {
		coef = 2
	}
	return Values{"price": number(z["rate_per_day_kzt"]) * (end.Sub(start).Hours()/24 + 1) * number(a["travelers_count"]) * coef, "zone": zone, "coverage": z["coverage"]}
}
func (b *Backend) coverage(p, a Values) Values {
	if p == nil || p["product"] != "dms" {
		return failure("not_found", "DMS policy not found")
	}
	if b.status(p) != "active" {
		return failure("policy_inactive", "Policy inactive")
	}
	pack := str(path(p, "details", "package"))
	rules := path(b.c.KB, "products", "dms", "packages", pack)
	service := strings.ToLower(str(a["service_name"]))
	known := map[string]string{"мрт": "MRI and CT", "mri": "MRI and CT", "кт": "MRI and CT", "стоматолог": "Dentistry", "dentist": "Dentistry", "терапевт": "Therapist visits", "therapist": "Therapist visits"}
	label := known[service]
	if label == "" {
		return Values{"covered": nil, "note": "Verify this service against package rules; do not guarantee coverage", "package": pack, "rules": rules}
	}
	covered := label == "Therapist visits" || pack == "Comfort"
	return Values{"covered": covered, "package": pack, "rules": rules, "note": "Referral and package restrictions still apply"}
}
func (b *Backend) book(name string, p, a Values) Values {
	date, e := time.Parse(time.DateOnly, str(a["preferred_date"]))
	if e != nil || date.Before(b.c.Today) {
		return failure("invalid_input", "Appointment must be in the future")
	}
	// The dataset has no real calendars. Use explicitly synthetic 10:00 slots,
	// avoid double booking, and return a concrete alternative when occupied.
	table := "inspection_points"
	if name == "book_appointment" {
		table = "clinics"
		if p == nil || p["product"] != "dms" {
			return failure("not_found", "DMS policy not found")
		}
		if b.status(p) != "active" {
			return failure("policy_inactive", "Policy inactive")
		}
		if str(path(p, "details", "package")) == "Basic" && a["doctor_specialty"] != "therapist" {
			return failure("not_covered", "Basic specialists require therapist referral; use operator")
		}
	}
	if name == "book_inspection" {
		claim := b.find("claims", "claim_number", a["claim_number"])
		if claim == nil || (has(a, "client_id") && claim["client_id"] != a["client_id"]) {
			return failure("not_found", "Claim not found")
		}
	}
	for _, row := range list(b.c.KB[table]) {
		r := asMap(row)
		if r["city"] != a["city"] {
			continue
		}
		if name == "book_appointment" && !slices.ContainsFunc(list(r["specialties"]), func(s any) bool { return strings.EqualFold(str(s), str(a["doctor_specialty"])) }) {
			continue
		}
		datetime := date.Format(time.DateOnly) + "T10:00:00+05:00"
		location := str(r["address"])
		for _, booking := range b.rows("bookings") {
			v := asMap(booking)
			if v["address"] == location && v["slot_datetime"] == datetime {
				return Values{"error": Values{"code": "no_availability", "message": "Mock slot occupied"}, "nearest_date": date.AddDate(0, 0, 1).Format(time.DateOnly)}
			}
		}
		result := Values{"clinic_name": r["name"], "address": location, "slot_datetime": datetime, "mock": true}
		b.append("bookings", result)
		return result
	}
	return failure("no_availability", "No matching mock location or specialist")
}
