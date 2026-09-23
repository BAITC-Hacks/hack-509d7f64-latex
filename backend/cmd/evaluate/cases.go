package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"voice-router/internal/router"
)

// turnCase is one client utterance with its expected scenarios.
type turnCase struct {
	ID       string
	Text     string
	Lang     string
	Expected []string
	Slots    map[string]any // expected slots (dialogs only; informational)
	Type     string         // dev utterance type or probe category
	Tags     []string       // dialog tags, probe tags and turn tags
	// RefIrreversible lists irreversible actions the reference bot turn right
	// after this client turn executes (dialogs only).
	RefIrreversible []string
}

// sessionCase is one conversation: a dev utterance, a sample dialog or a probe.
type sessionCase struct {
	ID    string
	Title string
	Tags  []string
	Turns []turnCase
}

type devUtterance struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Lang     string   `json:"lang"`
	Expected []string `json:"expected"`
	Type     string   `json:"type"`
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func loadDev(path string) ([]devUtterance, error) {
	var f struct {
		Utterances []devUtterance `json:"utterances"`
	}
	if err := readJSON(path, &f); err != nil {
		return nil, err
	}
	if len(f.Utterances) == 0 {
		return nil, fmt.Errorf("%s: no utterances", path)
	}
	return f.Utterances, nil
}

func devCases(us []devUtterance) []sessionCase {
	out := make([]sessionCase, 0, len(us))
	for _, u := range us {
		out = append(out, sessionCase{ID: u.ID, Tags: []string{u.Type}, Turns: []turnCase{{ID: u.ID, Text: u.Text, Lang: u.Lang, Expected: u.Expected, Type: u.Type, Tags: []string{u.Type}}}})
	}
	return out
}

// loadDialogs keeps client turns in order; bot turns are reference answers and
// only contribute the irreversible actions they execute.
func loadDialogs(path string, c *router.Catalog) ([]sessionCase, error) {
	var f struct {
		Dialogs []struct {
			ID    string   `json:"dialog_id"`
			Title string   `json:"title"`
			Tags  []string `json:"tags"`
			Turns []struct {
				Role      string         `json:"role"`
				Text      string         `json:"text"`
				Lang      string         `json:"lang"`
				Scenarios []string       `json:"scenarios"`
				Slots     map[string]any `json:"slots"`
				Actions   []struct {
					Name string `json:"name"`
					Mode string `json:"mode"`
				} `json:"actions"`
			} `json:"turns"`
		} `json:"dialogs"`
	}
	if err := readJSON(path, &f); err != nil {
		return nil, err
	}
	out := []sessionCase{}
	for _, d := range f.Dialogs {
		sc := sessionCase{ID: d.ID, Title: d.Title, Tags: d.Tags}
		for _, t := range d.Turns {
			switch t.Role {
			case "client":
				n := len(sc.Turns) + 1
				sc.Turns = append(sc.Turns, turnCase{ID: fmt.Sprintf("%s.%d", d.ID, n), Text: t.Text, Lang: t.Lang, Expected: t.Scenarios, Slots: t.Slots, Type: "dialog", Tags: d.Tags})
			case "bot":
				if len(sc.Turns) == 0 {
					continue
				}
				last := &sc.Turns[len(sc.Turns)-1]
				for _, a := range t.Actions {
					if a.Mode == "execute" && c.Actions[a.Name].Irreversible {
						last.RefIrreversible = append(last.RefIrreversible, a.Name)
					}
				}
			}
		}
		if len(sc.Turns) > 0 {
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no dialogs", path)
	}
	return out, nil
}

func loadProbes(path string) ([]sessionCase, error) {
	var f struct {
		Probes []struct {
			ID       string   `json:"id"`
			Category string   `json:"category"`
			Tags     []string `json:"tags"`
			Note     string   `json:"note"`
			Turns    []struct {
				Text     string   `json:"text"`
				Lang     string   `json:"lang"`
				Expected []string `json:"expected"`
				Tags     []string `json:"tags"`
			} `json:"turns"`
		} `json:"probes"`
	}
	if err := readJSON(path, &f); err != nil {
		return nil, err
	}
	out := []sessionCase{}
	for _, p := range f.Probes {
		tags := append([]string{p.Category}, p.Tags...)
		sc := sessionCase{ID: p.ID, Title: p.Note, Tags: uniq(tags)}
		for i, t := range p.Turns {
			id := p.ID
			if len(p.Turns) > 1 {
				id = fmt.Sprintf("%s.%d", p.ID, i+1)
			}
			sc.Turns = append(sc.Turns, turnCase{ID: id, Text: t.Text, Lang: t.Lang, Expected: t.Expected, Type: p.Category, Tags: uniq(append(append([]string{}, sc.Tags...), t.Tags...))})
		}
		out = append(out, sc)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no probes", path)
	}
	return out, nil
}

func uniq(xs []string) []string {
	out := []string{}
	for _, x := range xs {
		if x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// selectCases applies -ids and -limit.
func selectCases(cases []sessionCase, ids string, limit int) []sessionCase {
	if ids != "" {
		want := map[string]bool{}
		for _, id := range strings.Split(ids, ",") {
			want[strings.TrimSpace(id)] = true
		}
		kept := []sessionCase{}
		for _, c := range cases {
			if want[c.ID] {
				kept = append(kept, c)
			}
		}
		cases = kept
	}
	if limit > 0 && limit < len(cases) {
		cases = cases[:limit]
	}
	return cases
}
