package router

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// templated lists the scenarios whose dataset closing is spoken as written once
// the frame completes: every placeholder is a scalar from a tool result or a
// patterned slot, and the fixed text claims only what the actions did. Some run
// kb_lookup (SC03, SC07, SC08, SC11, SC24, SC32, SC38), but their closings carry
// their own fixed summary and read no knowledge-base field.
var templated = []string{"SC01", "SC02", "SC03", "SC05", "SC07", "SC08", "SC11", "SC12", "SC13", "SC14", "SC16", "SC19", "SC20", "SC21", "SC24", "SC25", "SC26", "SC27", "SC28", "SC29", "SC32", "SC35", "SC36", "SC38", "SC39"}

// untemplated is every other scenario, grouped by why its closing needs LLM wording.
var untemplated = map[string][]string{
	"closing is a free-form knowledge-base answer ({answer})":                                   {"SC09", "SC18", "SC31", "SC34", "SC40"},
	"closing reads English backend text: claim {status}/{next_step}, coverage {note}":           {"SC17", "SC22"},
	"closing reads a list of clinics, or English office hours":                                  {"SC23", "SC33"},
	"closing confirms a transfer that always runs; the handoff path words it":                   {"SC10", "SC15", "SC37"},
	"closing promises an SMS payment link, but the scenario sends no SMS":                       {"SC04"},
	"closing is the pre-purchase quote (\"Оформляем?\"), wrong once the policy is bought":       {"SC06"},
	"closing asserts an unissued payment and a transfer; completing without handoff is neither": {"SC30"},
}

// closingPremise is a result field a closing asserts beyond its action succeeding.
var closingPremise = map[string][3]string{
	"SC25": {"get_policy", "status", "active"}, // "действует до" is false for expired, cancelled or future policies
}

var closingPlaceholder = regexp.MustCompile(`\{(\w+)\}`)

// closingMasked are read back the way confirmation() shows them.
var closingMasked = []string{"phone", "iin", "email", "new_driver_iin", "sent_to"}

// template speaks the scenario's dataset closing when a frame completes, with
// placeholders filled from successful tool results (latest first) and then
// slots. It declines, leaving the wording to the LLM, whenever the closing
// would say something the facts do not back.
func (e *Engine) template(s *Session, facts Values, status string) (string, bool) {
	if status != "completed" {
		return "", false
	}
	var in struct {
		Scenario struct {
			ID string `json:"scenario_id"`
		} `json:"scenario"`
		Slots   Values       `json:"slots"`
		Actions []ActionCall `json:"actions"`
	}
	// JSON types throughout (numbers are float64) whether facts are live or restored.
	if b, err := json.Marshal(facts); err != nil || json.Unmarshal(b, &in) != nil {
		return "", false
	}
	sc, ok := e.Catalog.Scenarios[in.Scenario.ID]
	text := sc.Responses[s.Language]["closing"]
	if !ok || text == "" || !slices.Contains(templated, sc.ID) {
		return "", false
	}
	// The latest call of each action decides, so a failure a retry fixed does not block.
	latest := map[string]ActionCall{}
	for _, call := range in.Actions {
		latest[call.Name] = call
	}
	for name, call := range latest {
		if call.Mode != "execute" || errorCode(call.Result) != "" || name == "transfer_to_operator" {
			return "", false
		}
	}
	for _, name := range sc.Actions {
		if _, ran := latest[name]; !ran && name != "find_client" && name != "transfer_to_operator" {
			return "", false // skipped, e.g. send_sms without a phone, yet the closing claims it
		}
	}
	if p, ok := closingPremise[sc.ID]; ok && str(latest[p[0]].Result[p[1]]) != p[2] {
		return "", false
	}
	filled := true
	out := closingPlaceholder.ReplaceAllStringFunc(text, func(m string) string {
		v, ok := e.closingValue(m[1:len(m)-1], in.Actions, in.Slots)
		filled = filled && ok
		return v
	})
	if !filled || strings.ContainsAny(out, "{}") {
		return "", false
	}
	if len(pendingIDs(s)) > 0 {
		// Two open questions would make the caller's next "да" ambiguous.
		if strings.HasSuffix(sc.Responses["ru"]["closing"], "?") || strings.HasSuffix(sc.Responses["kk"]["closing"], "?") {
			return "", false
		}
		out += " " + local(s.Language, "Вернёмся к вашему предыдущему вопросу?", "Алдыңғы сұрағыңызға оралайық па?")
	}
	return out, true
}

// closingValue resolves one placeholder. Only top-level result fields are read:
// nested ones are ambiguous (get_policy.details.vehicle_plate is the plate
// before an SC05 change), and lists and maps are never spoken.
func (e *Engine) closingValue(key string, calls []ActionCall, slots Values) (string, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if c := calls[i]; c.Mode == "execute" && errorCode(c.Result) == "" && has(c.Result, key) {
			return spokenValue(key, c.Result[key])
		}
	}
	v := slots[key]
	// Slot values are read back only as codes, numbers or dates: enum values are
	// English codes and free text may be in another language than the reply.
	if def, ok := e.Catalog.Slots[key]; ok && def.Pattern == "" {
		if _, number := v.(float64); !number {
			if _, date := spokenDate(str(v)); !date {
				return "", false
			}
		}
	}
	return spokenValue(key, v)
}

func spokenValue(key string, v any) (string, bool) {
	switch x := v.(type) {
	case float64:
		return spokenAmount(x), true
	case string:
		if x == "" {
			return "", false
		}
		if slices.Contains(closingMasked, key) {
			return maskTailValue(x), true
		}
		if d, ok := spokenDate(x); ok {
			return d, true
		}
		return x, true
	}
	return "", false
}

// spokenAmount groups thousands with spaces and uses a decimal comma: 38 000, 1 234,5.
func spokenAmount(n float64) string {
	whole, frac, _ := strings.Cut(strconv.FormatFloat(math.Abs(n), 'f', -1, 64), ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + " " + whole[i:]
	}
	if frac != "" {
		whole += "," + frac
	}
	if n < 0 {
		whole = "-" + whole
	}
	return whole
}

// spokenDate renders ISO dates and datetimes as dd.mm.yyyy [HH:MM], keeping
// the wall-clock time the tool returned.
func spokenDate(s string) (string, bool) {
	for _, l := range [][2]string{{time.DateOnly, "02.01.2006"}, {time.RFC3339, "02.01.2006 15:04"}, {"2006-01-02T15:04:05", "02.01.2006 15:04"}, {"2006-01-02T15:04", "02.01.2006 15:04"}, {"2006-01-02 15:04", "02.01.2006 15:04"}} {
		if t, err := time.Parse(l[0], s); err == nil {
			return t.Format(l[1]), true
		}
	}
	return "", false
}

func maskTailValue(v string) string {
	if len(v) > 4 {
		return "***" + v[len(v)-4:]
	}
	return v
}
