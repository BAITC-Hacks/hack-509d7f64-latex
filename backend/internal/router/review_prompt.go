package router

import (
	"regexp"
	"sort"
	"strings"
)

type reviewLabel struct{ ru, kk string }

// Neutral labels describe the scenario, never an example customer's facts.
var reviewScenarioLabels = map[string]reviewLabel{
	"SC01": {"расчёт стоимости ОГПО", "ОГПО құнын есептеу"},
	"SC02": {"оформление ОГПО", "ОГПО рәсімдеу"},
	"SC03": {"условия и стоимость КАСКО", "КАСКО шарттары мен құны"},
	"SC04": {"добавление водителя в автополис", "автополиске жүргізуші қосу"},
	"SC05": {"изменение автомобиля или госномера в полисе", "полистегі автокөлікті немесе мемлекеттік нөмірді өзгерту"},
	"SC06": {"оформление туристической страховки", "саяхат сақтандыруын рәсімдеу"},
	"SC07": {"консультация по страхованию жилья", "тұрғын үйді сақтандыру бойынша кеңес"},
	"SC08": {"консультация по страхованию от несчастных случаев", "жазатайым оқиғалардан сақтандыру бойынша кеңес"},
	"SC09": {"консультация по личному ДМС", "жеке ерікті медициналық сақтандыру бойынша кеңес"},
	"SC10": {"страхование сотрудников или имущества компании", "компания қызметкерлерін немесе мүлкін сақтандыру"},
	"SC11": {"помощь на месте ДТП", "жол апаты орнында көмек алу"},
	"SC12": {"выплата пострадавшему по ОГПО виновника", "кінәлі тараптың ОГПО полисі бойынша жәбірленушіге төлем"},
	"SC13": {"заявление об ущербе по КАСКО", "КАСКО бойынша залал туралы өтініш"},
	"SC14": {"заявление о повреждении застрахованного жилья", "сақтандырылған тұрғын үйдің зақымдануы туралы өтініш"},
	"SC15": {"медицинская помощь за рубежом", "шетелде медициналық көмек алу"},
	"SC16": {"выплата за травму по страховке от несчастных случаев", "жазатайым оқиға кезіндегі жарақат үшін сақтандыру төлемі"},
	"SC17": {"статус страхового дела или выплаты", "сақтандыру ісінің немесе төлемнің мәртебесі"},
	"SC18": {"документы для страховой выплаты", "сақтандыру төлеміне қажетті құжаттар"},
	"SC19": {"обжалование отказа или размера выплаты", "төлемнен бас тартуға немесе төлем мөлшеріне шағымдану"},
	"SC20": {"запись на осмотр повреждённого автомобиля", "зақымданған автокөлікті тексеруге жазылу"},
	"SC21": {"запись к врачу по ДМС", "ерікті медициналық сақтандыру бойынша дәрігерге жазылу"},
	"SC22": {"проверка покрытия медицинской услуги по ДМС", "медициналық қызметтің сақтандырумен қамтылуын тексеру"},
	"SC23": {"список клиник-партнёров", "серіктес клиникалар тізімі"},
	"SC24": {"получение электронной карты ДМС", "ерікті медициналық сақтандырудың электрондық картасын алу"},
	"SC25": {"проверка срока действия полиса", "полистің жарамдылық мерзімін тексеру"},
	"SC26": {"повторная отправка документов полиса", "полис құжаттарын қайта жіберу"},
	"SC27": {"продление полиса", "полисті ұзарту"},
	"SC28": {"расторжение полиса и возврат средств", "полисті бұзу және қаражатты қайтару"},
	"SC29": {"изменение контактных данных", "байланыс деректерін өзгерту"},
	"SC30": {"проверка списания денег без оформления полиса", "полис рәсімделмей ақша алынуын тексеру"},
	"SC31": {"способы оплаты и рассрочка", "төлем тәсілдері мен бөліп төлеу"},
	"SC32": {"класс бонус-малус и изменение цены ОГПО", "бонус-малус сыныбы және ОГПО бағасының өзгеруі"},
	"SC33": {"адреса и часы работы офисов", "кеңселердің мекенжайлары мен жұмыс уақыты"},
	"SC34": {"помощь с приложением или личным кабинетом", "қосымша немесе жеке кабинет бойынша көмек"},
	"SC35": {"жалоба на обслуживание", "қызмет көрсетуге шағым"},
	"SC36": {"заказ обратного звонка", "кері қоңырауға тапсырыс беру"},
	"SC37": {"разговор с оператором", "оператормен сөйлесу"},
	"SC38": {"сообщение о подозрительном звонке или мошенничестве", "күдікті қоңырау немесе алаяқтық туралы хабарлау"},
	"SC39": {"получение справки или копии документа", "анықтама немесе құжат көшірмесін алу"},
	"SC40": {"разъяснение условий страхования", "сақтандыру шарттарын түсіндіру"},
}

var reviewSlotLabels = map[string]reviewLabel{
	"phone": {"телефон", "телефон"}, "iin": {"ИИН", "ЖСН"},
	"policy_number": {"номер полиса", "полис нөмірі"}, "claim_number": {"номер страхового дела", "сақтандыру ісінің нөмірі"},
	"vehicle_plate": {"госномер", "мемлекеттік нөмір"}, "culprit_vehicle_plate": {"госномер виновника", "кінәлі тараптың мемлекеттік нөмірі"},
	"vehicle_type": {"тип автомобиля", "көлік түрі"}, "region": {"регион регистрации", "тіркеу өңірі"},
	"drivers_iin": {"ИИН водителей", "жүргізушілердің ЖСН-дері"}, "new_driver_iin": {"ИИН нового водителя", "жаңа жүргізушінің ЖСН-і"},
	"car_value": {"стоимость автомобиля, ₸", "көлік құны, ₸"}, "car_year": {"год выпуска", "шығарылған жылы"},
	"franchise": {"франшиза, ₸", "франшиза, ₸"}, "product_type": {"вид страховки", "сақтандыру түрі"},
	"trip_country": {"страна поездки", "сапар елі"}, "trip_start": {"начало поездки", "сапардың басталуы"}, "trip_end": {"окончание поездки", "сапардың аяқталуы"},
	"travelers_count": {"число путешественников", "саяхатшылар саны"}, "traveler_max_age": {"возраст старшего путешественника", "ең үлкен саяхатшының жасы"},
	"property_type": {"тип жилья", "тұрғын үй түрі"}, "property_address": {"адрес жилья", "тұрғын үй мекенжайы"},
	"sum_insured": {"страховая сумма, ₸", "сақтандыру сомасы, ₸"}, "incident_date": {"дата происшествия", "оқиға күні"},
	"incident_description": {"описание происшествия", "оқиға сипаттамасы"}, "injured": {"есть пострадавшие", "зардап шеккендер бар"},
	"location": {"местоположение", "орналасқан жері"}, "city": {"город", "қала"},
	"email": {"электронная почта", "электрондық пошта"}, "contact_field": {"изменяемое поле", "өзгертілетін дерек"}, "new_value": {"новое значение", "жаңа мән"},
	"payment_date": {"дата оплаты", "төлем күні"}, "payment_amount": {"сумма оплаты, ₸", "төлем сомасы, ₸"},
	"callback_time": {"время обратного звонка", "кері қоңырау уақыты"}, "doctor_specialty": {"специальность врача", "дәрігер мамандығы"},
	"service_name": {"медицинская услуга", "медициналық қызмет"}, "preferred_date": {"желаемая дата", "қалаған күн"},
	"company_name": {"название компании", "компания атауы"}, "employees_count": {"число сотрудников", "қызметкерлер саны"},
	"cancel_reason": {"причина расторжения", "бұзу себебі"}, "complaint_text": {"суть жалобы", "шағым мәні"},
	"fraud_details": {"обстоятельства подозрительного обращения", "күдікті хабарласу мән-жайы"},
	"document_type": {"вид документа", "құжат түрі"}, "topic": {"тема вопроса", "сұрақ тақырыбы"},
}

var reviewEnumLabels = map[string]map[string]reviewLabel{
	"contact_field": {"phone": {"телефон", "телефон"}, "email": {"электронная почта", "электрондық пошта"}, "address": {"адрес", "мекенжай"}},
	"vehicle_type":  {"car": {"легковой автомобиль", "жеңіл автокөлік"}, "truck": {"грузовой автомобиль", "жүк көлігі"}, "motorcycle": {"мотоцикл", "мотоцикл"}},
	"property_type": {"apartment": {"квартира", "пәтер"}, "house": {"дом", "үй"}},
	"product_type":  {"ogpo": {"ОГПО", "ОГПО"}, "casco": {"КАСКО", "КАСКО"}, "travel": {"туристическая страховка", "саяхат сақтандыруы"}, "property": {"страхование жилья", "тұрғын үйді сақтандыру"}, "accident": {"от несчастных случаев", "жазатайым оқиғалардан сақтандыру"}, "dms": {"ДМС", "ерікті медициналық сақтандыру"}},
	"document_type": {"policy_duplicate": {"дубликат полиса", "полис телнұсқасы"}, "contract_copy": {"копия договора", "шарт көшірмесі"}, "embassy_certificate": {"справка для посольства", "елшілікке анықтама"}, "payment_certificate": {"справка об оплате", "төлем туралы анықтама"}},
}

func (e *Engine) intentQuestion(lang string, d Decision) string {
	intents, seen := []string{}, map[string]bool{}
	for i, candidate := range d.Scenarios {
		if i > 0 && candidate.Confidence < .75 || seen[candidate.ScenarioID] {
			continue
		}
		seen[candidate.ScenarioID] = true
		if label, ok := reviewScenarioLabels[candidate.ScenarioID]; ok {
			intents = append(intents, local(lang, label.ru, label.kk))
		}
	}
	if len(intents) == 0 {
		intents = append(intents, local(lang, "вопрос по страхованию", "сақтандыру туралы сұрақ"))
	}
	question := local(lang, "Правильно ли я понял запрос: ", "Сұрауды дұрыс түсіндім бе: ") + strings.Join(intents, "; ")
	keys := make([]string, 0, len(d.Slots))
	for key := range d.Slots {
		if has(d.Slots, key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	details := make([]string, 0, len(keys))
	for _, key := range keys {
		label, ok := reviewSlotLabels[key]
		if !ok {
			continue // Unknown/internal keys must never become user-facing labels.
		}
		value := strings.Join(strings.Fields(reviewSlotValue(lang, key, d.Slots)), " ")
		if runes := []rune(value); len(runes) > 96 {
			value = string(runes[:96]) + "…"
		}
		details = append(details, local(lang, label.ru, label.kk)+": "+value)
	}
	if len(details) > 0 {
		question += " (" + strings.Join(details, "; ") + ")"
	}
	return question + "?"
}

var reviewEmailPattern = regexp.MustCompile(`[^\s<>@,;]+@[^\s<>@,;]+\.[^\s<>@,;]+`)
var reviewNumberPattern = regexp.MustCompile(`\+?\d[\d ()-]{8,}\d`)

func reviewSlotValue(lang, key string, slots Values) string {
	value := slots[key]
	text := str(value)
	switch key {
	case "phone", "iin", "new_driver_iin":
		return reviewMaskedNumber(text)
	case "drivers_iin":
		values := []string{}
		for _, item := range list(value) {
			values = append(values, reviewMaskedNumber(str(item)))
		}
		return strings.Join(values, ", ")
	case "email":
		return reviewMaskedEmail(text)
	case "new_value":
		switch str(slots["contact_field"]) {
		case "phone":
			return reviewMaskedNumber(text)
		case "email":
			return reviewMaskedEmail(text)
		default:
			return local(lang, "[скрыто]", "[жасырылған]")
		}
	}
	if b, ok := value.(bool); ok {
		if b {
			return local(lang, "да", "иә")
		}
		return local(lang, "нет", "жоқ")
	}
	if label, ok := reviewEnumLabels[key][text]; ok {
		return local(lang, label.ru, label.kk)
	}
	// A customer may mention identifying information inside a free-text slot.
	text = reviewEmailPattern.ReplaceAllStringFunc(text, reviewMaskedEmail)
	text = reviewNumberPattern.ReplaceAllStringFunc(text, func(s string) string {
		digits := reviewDigits(s)
		if len(digits) >= 10 {
			return reviewMaskedNumber(s)
		}
		return s // Preserve ordinary dates and short monetary amounts.
	})
	return text
}

func reviewDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func reviewMaskedNumber(s string) string {
	digits := reviewDigits(s)
	if len(digits) <= 4 {
		return "***"
	}
	return "***" + digits[len(digits)-4:]
}

func reviewMaskedEmail(s string) string {
	if at := strings.LastIndexByte(s, '@'); at >= 0 && at+1 < len(s) {
		return "***" + s[at:]
	}
	return "***"
}
