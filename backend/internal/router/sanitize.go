package router

import (
	"regexp"
	"slices"
	"strings"
)

// sanitizeSlots keeps a routing decision usable when the model extracted a
// slot badly: enum values are matched case-insensitively to their catalog
// spelling, and slots that still fail validation are dropped so the workflow
// asks for them again, instead of discarding a correct scenario choice.
// It returns the dropped slot names.
func (c *Catalog) sanitizeSlots(v Values) []string {
	dropped := []string{}
	for k, x := range v {
		if s, ok := c.Slots[k]; ok && x != nil {
			for _, allowed := range s.Values {
				if strings.EqualFold(str(allowed), str(x)) {
					v[k] = allowed
					break
				}
			}
		}
		if err := c.ValidateSlots(Values{k: v[k]}); err != nil {
			delete(v, k)
			dropped = append(dropped, k)
		}
	}
	slices.Sort(dropped)
	return dropped
}

var (
	answerIIN   = regexp.MustCompile(`\b\d{12}\b`)
	answerPhone = regexp.MustCompile(`(?:\+7|\b8)[\s\-]?\(?\d{3}\)?[\s\-]?\d{3}[\s\-]?\d{2}[\s\-]?\d{2}\b`)
	answerEmail = regexp.MustCompile(`\b[^@\s]+(@[^@\s]+\.[A-Za-z]{2,})`)
)

// maskAnswer hides IINs, phone numbers and e-mail local parts in spoken
// answers regardless of how they were worded; LLM wording can repeat them.
func maskAnswer(s string) string {
	tail := func(m string) string {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, m)
		return "***" + digits[len(digits)-4:]
	}
	s = answerIIN.ReplaceAllStringFunc(s, tail)
	s = answerPhone.ReplaceAllStringFunc(s, tail)
	return answerEmail.ReplaceAllString(s, "***$1")
}
