package router

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"
	"voice-router/voice_router_dataset"
)

type Scenario struct {
	ID          string   `json:"scenario_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Boundaries  []Values `json:"not_this_if"`
	Priority    string   `json:"priority"`
	FastPath    bool     `json:"fast_path_eligible"`
	Identify    bool     `json:"requires_identification"`
	Slots       struct {
		Required []string `json:"required"`
		Optional []string `json:"optional"`
	} `json:"slots"`
	Actions []string `json:"actions"`
	Confirm bool     `json:"requires_confirmation"`
	Handoff *struct {
		When  string `json:"when"`
		Queue string `json:"queue"`
	} `json:"handoff"`
	Examples  map[string][]string          `json:"examples"`
	Responses map[string]map[string]string `json:"responses"`
}
type Slot struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Pattern     string            `json:"pattern"`
	Values      []any             `json:"values"`
	Prompt      map[string]string `json:"prompt"`
}
type Action struct {
	Name         string   `json:"name"`
	Inputs       []string `json:"inputs"`
	Irreversible bool     `json:"irreversible"`
}
type Catalog struct {
	Scenarios map[string]Scenario
	Ordered   []Scenario
	Slots     map[string]Slot
	Actions   map[string]Action
	System    map[string]map[string]string
	KB        Values
	Seed      Values
	Queues    []string
	Today     time.Time
}

func LoadCatalog() (*Catalog, error) {
	c := &Catalog{Scenarios: map[string]Scenario{}, Slots: map[string]Slot{}, Actions: map[string]Action{}, System: map[string]map[string]string{}}
	read := func(name string, v any) error {
		b, e := dataset.Files.ReadFile(name)
		if e != nil {
			return e
		}
		return json.Unmarshal(b, v)
	}
	var sc struct {
		Meta struct {
			Date string `json:"as_of_date"`
		} `json:"meta"`
		Scenarios []Scenario `json:"scenarios"`
		System    []struct {
			ID       string            `json:"id"`
			Response map[string]string `json:"response"`
		} `json:"system_intents"`
	}
	if e := read("scenarios.json", &sc); e != nil {
		return nil, e
	}
	c.Ordered = sc.Scenarios
	var e error
	c.Today, e = time.Parse(time.DateOnly, sc.Meta.Date)
	if e != nil {
		return nil, e
	}
	for _, s := range sc.Scenarios {
		c.Scenarios[s.ID] = s
	}
	for _, s := range sc.System {
		c.System[s.ID] = s.Response
	}
	var sl struct {
		Slots []Slot `json:"slots"`
	}
	if e := read("slots.json", &sl); e != nil {
		return nil, e
	}
	for _, s := range sl.Slots {
		c.Slots[s.Name] = s
	}
	var ac struct {
		Actions []Action `json:"actions"`
		Queues  []string `json:"queues"`
	}
	if e := read("actions.json", &ac); e != nil {
		return nil, e
	}
	for _, a := range ac.Actions {
		c.Actions[a.Name] = a
	}
	c.Queues = ac.Queues
	if e := read("knowledge_base.json", &c.KB); e != nil {
		return nil, e
	}
	if e := read("mock_backend.json", &c.Seed); e != nil {
		return nil, e
	}
	if len(c.Scenarios) != 40 {
		return nil, fmt.Errorf("expected 40 scenarios")
	}
	for _, s := range c.Ordered {
		for _, a := range s.Actions {
			if _, ok := c.Actions[a]; !ok {
				return nil, fmt.Errorf("unknown action %s", a)
			}
		}
	}
	return c, nil
}
func (c *Catalog) ValidID(id string) bool {
	_, a := c.Scenarios[id]
	_, b := c.System[id]
	return a || b
}
func (c *Catalog) ValidateSlots(v Values) error {
	for k, x := range v {
		s, ok := c.Slots[k]
		if !ok {
			return fmt.Errorf("unknown slot %s", k)
		}
		if x == nil {
			return fmt.Errorf("null slot %s", k)
		}
		xs := []any{x}
		switch s.Type {
		case "list":
			a, ok := x.([]any)
			if !ok || len(a) == 0 {
				return fmt.Errorf("invalid list %s", k)
			}
			xs = a
		case "integer":
			n, ok := x.(float64)
			if !ok || n != float64(int64(n)) || n < 0 {
				return fmt.Errorf("invalid integer %s", k)
			}
		case "boolean":
			if _, ok := x.(bool); !ok {
				return fmt.Errorf("invalid boolean %s", k)
			}
		case "date":
			if _, e := time.Parse(time.DateOnly, str(x)); e != nil {
				return fmt.Errorf("invalid date %s", k)
			}
		case "string", "text":
			if _, ok := x.(string); !ok || str(x) == "" {
				return fmt.Errorf("invalid string %s", k)
			}
		}
		for _, item := range xs {
			if s.Pattern != "" {
				ok, e := regexp.MatchString(s.Pattern, str(item))
				if e != nil || !ok {
					return fmt.Errorf("invalid format %s", k)
				}
			}
		}
		if len(s.Values) > 0 && !slices.ContainsFunc(s.Values, func(a any) bool { return str(a) == str(x) }) {
			return fmt.Errorf("invalid value %s", k)
		}
	}
	return nil
}
