package router

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// preRoute recognises, without a model call, a continuation of the scenario
// the model chose on an earlier turn. It needs two independent witnesses: the
// engine's own state says the last turn asked this frame for a slot (or for a
// yes/no on a preview), and the whole utterance is exactly such an answer.
// Anything more ("да, но телефон другой") goes to the model, which alone picks
// scenarios; this only continues one.
func (e *Engine) preRoute(s *Session, in Input) (Decision, bool) {
	f := s.Active
	if f == nil || len(s.Turns) == 0 {
		return Decision{}, false
	}
	last := s.Turns[len(s.Turns)-1].Output
	if last == nil || last.ActiveScenario != f.ScenarioID {
		return Decision{}, false
	}
	d := Decision{Alternatives: []Candidate{}, Language: preLanguage(s, in), Slots: Values{}, IsContinuation: true}
	reason := ""
	confirming := last.Status == "awaiting_confirmation" && f.Pending != nil
	switch {
	case last.Status == "awaiting_slot" && len(f.Awaiting) > 0:
		name, value, ok := slotReader{e.Catalog}.awaited(f.Awaiting, in.Text)
		if !ok {
			return Decision{}, false
		}
		d.Slots[name] = value
		reason = "deterministic continuation: utterance is exactly the awaited slot " + name
	case confirming && explicitYes(in.Text):
		reason = "deterministic continuation: explicit yes to the pending " + f.Pending.Action + " preview"
	case confirming && explicitNo(in.Text):
		reason = "deterministic continuation: explicit no to the pending " + f.Pending.Action + " preview"
	default:
		return Decision{}, false
	}
	d.Scenarios = []Candidate{{ScenarioID: f.ScenarioID, Confidence: 1, Reason: reason}}
	if e.validateDecision(d) != nil {
		return Decision{}, false
	}
	return d, true
}

func preLanguage(s *Session, in Input) string {
	for _, lang := range []string{in.ReplyLanguage, in.Language, s.Language} {
		if lang == "ru" || lang == "kk" {
			return lang
		}
	}
	return "ru"
}

// preNormalize is the matching form of an utterance: lowercase, single spaces,
// no trailing punctuation.
func preNormalize(text string) string {
	text = strings.ReplaceAll(strings.ToLower(text), "ё", "е")
	return strings.TrimRight(strings.Join(strings.Fields(text), " "), ".,!? ")
}

// slotReader parses a whole utterance as one typed slot value. Every value it
// returns has the JSON shape of model output and passed Catalog.ValidateSlots.
type slotReader struct{ catalog *Catalog }

// awaited reads the utterance as one of the slots the last question asked for.
// Two slots accepting the same text would be a guess, so that defers too.
func (r slotReader) awaited(names []string, text string) (string, any, bool) {
	t := preNormalize(text)
	found, value := "", any(nil)
	for _, name := range names {
		v, ok := r.parse(name, t)
		if !ok || name == found {
			continue
		}
		if found != "" {
			return "", nil, false
		}
		found, value = name, v
	}
	return found, value, found != ""
}

func (r slotReader) parse(name, t string) (any, bool) {
	slot, ok := r.catalog.Slots[name]
	if !ok || t == "" {
		return nil, false
	}
	var v any
	switch slot.Type {
	case "string":
		// Only patterned strings; free text (trip_country, location, new_value…)
		// has no shape that proves the utterance is nothing but the value.
		if parse := preStrings[name]; parse != nil && slot.Pattern != "" {
			if x, ok := parse(t); ok {
				v = x
			}
		}
	case "list":
		if name == "drivers_iin" {
			if xs, ok := preIINs(t); ok {
				v = xs
			}
		}
	case "integer":
		if n, ok := preInteger(t); ok {
			v = n
		}
	case "enum":
		v = r.enum(name, slot, t)
	case "date":
		if x, ok := r.date(t); ok {
			v = x
		}
	case "boolean":
		if b, ok := preBool(name, slot, t); ok {
			v = b
		}
	}
	if v == nil || r.catalog.ValidateSlots(Values{name: v}) != nil {
		return nil, false
	}
	return v, true
}

var preStrings = map[string]func(string) (string, bool){
	"phone": prePhone, "iin": preIIN, "new_driver_iin": preIIN, "policy_number": prePolicy, "claim_number": preClaim,
	"vehicle_plate": prePlate, "culprit_vehicle_plate": prePlate, "email": preEmail,
}

var (
	prePhoneShape   = regexp.MustCompile(`^\+?[\d ()-]+$`)
	preDigitsSpaces = regexp.MustCompile(`^[\d ]+$`)
	preDigits       = regexp.MustCompile(`^\d+$`)
	prePolicyShape  = regexp.MustCompile(`^SQ([A-Z]+)(\d{6})$`)
	preClaimShape   = regexp.MustCompile(`^CL(\d{6})$`)
	preEmailShape   = regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,}$`)
	preIntegerShape = regexp.MustCompile(`^(\d{1,3}( \d{3})+|\d+)$`)
	preAmountShape  = regexp.MustCompile(`^(\d{1,3}) (тыс|тысяч|тысячи|тысяча|мың|млн|миллион|миллиона|миллионов)$`)
	preDMY          = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})(\.(\d{4}))?$`)
	preDayMonth     = regexp.MustCompile(`^(\d{1,2}) (\p{L}+)( (\d{4}))?( (года|г|жыл|жылы))?$`)
	// Kazakh plates are Latin; speech normalisation may emit Cyrillic look-alikes.
	preHomoglyphs = strings.NewReplacer("А", "A", "В", "B", "Е", "E", "К", "K", "М", "M", "Н", "H", "О", "O", "Р", "P", "С", "C", "Т", "T", "У", "Y", "Х", "X")
)

// prePhone accepts +7XXXXXXXXXX, 7XXXXXXXXXX and 8XXXXXXXXXX with spaces,
// dashes or brackets. Ten bare digits could be a truncated number, so no.
func prePhone(t string) (string, bool) {
	if !prePhoneShape.MatchString(t) {
		return "", false
	}
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, t)
	switch {
	case len(d) != 11:
		return "", false
	case d[0] == '7':
		return "+" + d, true
	case d[0] == '8' && t[0] != '+':
		return "+7" + d[1:], true
	}
	return "", false
}

func preIIN(t string) (string, bool) {
	if !preDigitsSpaces.MatchString(t) {
		return "", false
	}
	return strings.ReplaceAll(t, " ", ""), true
}

// preIINs reads IINs separated by commas, "и", "және" or spaces. Spoken digit
// groups ("850314 300121") join until they make exactly 12 digits.
func preIINs(t string) ([]any, bool) {
	items, current, separated := []any{}, "", true
	for _, token := range strings.Fields(strings.NewReplacer(",", " , ", ";", " , ").Replace(t)) {
		switch {
		case token == "," || token == "и" || token == "және":
			if separated || current != "" {
				return nil, false
			}
			separated = true
		case preDigits.MatchString(token) && len(current)+len(token) <= 12:
			current += token
			separated = false
			if len(current) == 12 {
				if !slices.Contains(items, any(current)) {
					items = append(items, current)
				}
				current = ""
			}
		default:
			return nil, false
		}
	}
	return items, len(items) > 0 && current == "" && !separated
}

func preCompact(t string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(t))
}

// prePolicy restores the dashes; the catalog pattern checks the product code.
func prePolicy(t string) (string, bool) {
	m := prePolicyShape.FindStringSubmatch(preCompact(t))
	if m == nil {
		return "", false
	}
	return "SQ-" + m[1] + "-" + m[2], true
}

func preClaim(t string) (string, bool) {
	m := preClaimShape.FindStringSubmatch(preCompact(t))
	if m == nil {
		return "", false
	}
	return "CL-" + m[1], true
}

func prePlate(t string) (string, bool) { return preHomoglyphs.Replace(preCompact(t)), true }

func preEmail(t string) (string, bool) { return t, preEmailShape.MatchString(t) }

// preInteger accepts digits, optionally grouped by thousands ("8 000 000").
func preInteger(t string) (float64, bool) {
	if !preIntegerShape.MatchString(t) {
		return 0, false
	}
	t = strings.ReplaceAll(t, " ", "")
	if len(t) > 15 {
		return 0, false
	}
	n, err := strconv.ParseFloat(t, 64)
	return n, err == nil
}

// preAmount also reads "50 тысяч" or "5 млн"; used only for numeric enums,
// where the result must still be one of the catalog's values.
func preAmount(t string) (float64, bool) {
	for _, currency := range []string{" тенге", " теңге", " тг"} {
		t = strings.TrimSuffix(t, currency)
	}
	if n, ok := preInteger(t); ok {
		return n, true
	}
	m := preAmountShape.FindStringSubmatch(t)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	if strings.HasPrefix(m[2], "т") || m[2] == "мың" {
		return n * 1e3, true
	}
	return n * 1e6, true
}

func (r slotReader) enum(name string, slot Slot, t string) any {
	forms := []string{t}
	for _, prefix := range preEnumPrefixes[name] {
		if rest, ok := strings.CutPrefix(t, prefix+" "); ok {
			forms = append(forms, rest)
		}
	}
	if name == "city" || name == "region" {
		for _, suffix := range []string{" қаласы", " қаласында"} {
			if rest, ok := strings.CutSuffix(t, suffix); ok {
				forms = append(forms, rest)
			}
		}
	}
	for _, form := range forms {
		canonical, ok := preEnumWords[name][form]
		for _, v := range slot.Values {
			if strings.ToLower(str(v)) == form || (ok && str(v) == canonical) {
				return v
			}
		}
	}
	if n, ok := preAmount(t); ok {
		for _, v := range slot.Values {
			if x, ok := v.(float64); ok && x == n {
				return v
			}
		}
	}
	return nil
}

var preEnumPrefixes = map[string][]string{
	"city":         {"в", "во", "г", "г.", "город", "в г", "в г.", "в городе"},
	"region":       {"в", "во", "г", "г.", "город", "в г", "в г.", "в городе"},
	"product_type": {"по", "про"},
}

var preCities = map[string][]string{
	"Almaty":    {"алматы", "алма-ата", "алмате", "алматыда"},
	"Astana":    {"астана", "астане", "астанада"},
	"Shymkent":  {"шымкент", "шымкенте", "шымкентте", "чимкент", "чимкенте"},
	"Karaganda": {"караганда", "караганде", "караганды", "қарағанды", "қарағандыда"},
	"Aktobe":    {"актобе", "ақтөбе", "ақтөбеде", "актюбинск", "актюбинске"},
	"Atyrau":    {"атырау", "атырауда"},
	"Pavlodar":  {"павлодар", "павлодаре", "павлодарда"},
	"Oskemen":   {"оскемен", "өскемен", "өскеменде", "усть-каменогорск", "усть-каменогорске"},
}

// preEnumSynonyms maps slot → catalog value → spoken ru/kk forms. Canonical
// values themselves (case-insensitive) are always accepted.
var preEnumSynonyms = func() map[string]map[string][]string {
	region := map[string][]string{"almaty": preCities["Almaty"], "astana": preCities["Astana"], "other": {"другой", "другой город", "другой регион", "другая область", "басқа", "басқа қала", "басқа өңір"}}
	for city, forms := range preCities {
		if city != "Almaty" && city != "Astana" {
			region["other"] = append(append(region["other"], strings.ToLower(city)), forms...)
		}
	}
	return map[string]map[string][]string{
		"city":   preCities,
		"region": region,
		"vehicle_type": {
			"car":        {"легковая", "легковой", "легковушка", "легковая машина", "легковой автомобиль", "жеңіл", "жеңіл көлік", "жеңіл автокөлік"},
			"truck":      {"грузовик", "грузовая", "грузовой", "грузовая машина", "грузовой автомобиль", "жүк көлігі", "жүк машинасы"},
			"motorcycle": {"мотоцикл", "мото", "мотоцикл көлігі"},
		},
		"property_type": {
			"apartment": {"квартира", "квартиру", "пәтер"},
			"house":     {"дом", "частный дом", "үй", "жеке үй"},
		},
		"product_type": {
			"ogpo":     {"огпо", "автогражданка"},
			"casco":    {"каско", "kasko"},
			"travel":   {"туристическая", "туристическая страховка", "для путешествий", "туристік", "туристік сақтандыру"},
			"property": {"имущество", "страхование имущества", "недвижимость", "жилье", "мүлік", "мүлікті сақтандыру"},
			"accident": {"несчастный случай", "от несчастного случая", "нс", "жазатайым оқиға", "жазатайым оқиғадан"},
			"dms":      {"дмс", "медицинская страховка", "медстраховка", "ерікті медициналық сақтандыру"},
		},
		"document_type": {
			"policy_duplicate":    {"дубликат", "дубликат полиса", "полис телнұсқасы", "полистің телнұсқасы"},
			"contract_copy":       {"копия договора", "копию договора", "шарт көшірмесі", "шарттың көшірмесі"},
			"embassy_certificate": {"справка для посольства", "справку для посольства", "для посольства", "елшілікке анықтама", "елшілік үшін анықтама"},
			"payment_certificate": {"справка об оплате", "справку об оплате", "подтверждение оплаты", "төлем туралы анықтама"},
		},
		"contact_field": {
			"phone":   {"телефон", "номер телефона", "телефон нөмірі"},
			"email":   {"почта", "почту", "электронная почта", "электронную почту", "e-mail", "имейл", "пошта", "поштаны", "электрондық пошта"},
			"address": {"адрес", "мекенжай", "мекен-жай", "мекенжайды"},
		},
		"franchise": {"0": {"без франшизы", "без нее", "франшизасыз"}},
	}
}()

var preEnumWords = func() map[string]map[string]string {
	out := map[string]map[string]string{}
	for slot, values := range preEnumSynonyms {
		out[slot] = map[string]string{}
		for canonical, words := range values {
			for _, w := range words {
				out[slot][w] = canonical
			}
		}
	}
	return out
}()

var preRelativeDays = map[string]int{"сегодня": 0, "бүгін": 0, "завтра": 1, "ертең": 1, "послезавтра": 2, "бүрсігүні": 2, "бүрсүгүні": 2, "вчера": -1, "кеше": -1, "позавчера": -2}

var preMonths = map[string]time.Month{
	"января": 1, "февраля": 2, "марта": 3, "апреля": 4, "мая": 5, "июня": 6, "июля": 7, "августа": 8, "сентября": 9, "октября": 10, "ноября": 11, "декабря": 12,
	"қаңтар": 1, "ақпан": 2, "наурыз": 3, "сәуір": 4, "мамыр": 5, "маусым": 6, "шілде": 7, "тамыз": 8, "қыркүйек": 9, "қазан": 10, "қараша": 11, "желтоқсан": 12,
}

// date reads dd.mm[.yyyy], yyyy-mm-dd, "15 октября [2026]" and relative days
// against the dataset's today; a missing year is today's year.
func (r slotReader) date(t string) (string, bool) {
	today := r.catalog.Today
	if n, ok := preRelativeDays[t]; ok {
		return today.AddDate(0, 0, n).Format(time.DateOnly), true
	}
	if d, err := time.Parse(time.DateOnly, t); err == nil {
		return d.Format(time.DateOnly), true
	}
	var day, year int
	var month time.Month
	if m := preDMY.FindStringSubmatch(t); m != nil {
		day, _ = strconv.Atoi(m[1])
		n, _ := strconv.Atoi(m[2])
		month = time.Month(n)
		year, _ = strconv.Atoi(m[4])
	} else if m := preDayMonth.FindStringSubmatch(t); m != nil && preMonths[m[2]] != 0 {
		day, _ = strconv.Atoi(m[1])
		month = preMonths[m[2]]
		year, _ = strconv.Atoi(m[4])
	} else {
		return "", false
	}
	if year == 0 {
		year = today.Year()
	}
	d := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if d.Day() != day || d.Month() != month {
		return "", false
	}
	return d.Format(time.DateOnly), true
}

var preBoolWords = map[string]map[string]bool{
	"injured": {
		"есть": true, "есть пострадавшие": true, "есть раненые": true, "бар": true, "зардап шеккендер бар": true,
		"все целы": false, "никто не пострадал": false, "нет пострадавших": false, "пострадавших нет": false,
		"барлығы аман": false, "бәрі аман": false, "ешкім зардап шеккен жоқ": false, "зардап шеккендер жоқ": false,
	},
}

func preBool(name string, slot Slot, t string) (bool, bool) {
	if v, ok := preBoolWords[name][t]; ok {
		return v, true
	}
	// A bare yes/no to "Все целы? Есть пострадавшие?" does not say which of
	// the two opposite questions it answers.
	if strings.Count(slot.Prompt["ru"], "?") > 1 || strings.Count(slot.Prompt["kk"], "?") > 1 {
		return false, false
	}
	if explicitYes(t) {
		return true, true
	}
	return false, explicitNo(t)
}
