package main

import (
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
)

type counter struct {
	N       int `json:"n"`
	Primary int `json:"primary"`
	Exact   int `json:"exact"`
}

func (c *counter) add(r row) {
	c.N++
	if r.PrimaryOK {
		c.Primary++
	}
	if r.ExactOK {
		c.Exact++
	}
}

type percentile struct {
	N    int     `json:"n"`
	P50  int64   `json:"p50"`
	P95  int64   `json:"p95"`
	Mean float64 `json:"mean"`
	Max  int64   `json:"max"`
}

type summary struct {
	Mode          string                 `json:"mode"`
	Turns         int                    `json:"turns"`
	All           counter                `json:"all"`
	ByLang        map[string]*counter    `json:"by_lang"`
	ByPath        map[string]*counter    `json:"by_path"`
	ByType        map[string]*counter    `json:"by_type"`
	ByTag         map[string]*counter    `json:"by_tag"`
	BySession     map[string]*counter    `json:"by_session"`
	Status        map[string]int         `json:"status"`
	FallbackLevel map[int]int            `json:"fallback_level"`
	IntentRecall  [2]int                 `json:"intent_recall"` // found, expected over multi-intent turns
	Latency       map[string]*percentile `json:"latency_ms"`
	ProcessErrors int                    `json:"process_errors"`
	TraceErrors   int                    `json:"trace_errors"`
	Violations    int                    `json:"safety_violations"`
	RefIrrev      int                    `json:"reference_irreversible"`
	RefIrrevDone  int                    `json:"reference_irreversible_executed"`
}

var latencyKeys = []string{"prerouter", "retrieval", "router", "tools", "response", "total", "wall"}

func summarize(mode string, rows []row) *summary {
	s := &summary{Mode: mode, Turns: len(rows), ByLang: map[string]*counter{}, ByPath: map[string]*counter{}, ByType: map[string]*counter{}, ByTag: map[string]*counter{}, BySession: map[string]*counter{}, Status: map[string]int{}, FallbackLevel: map[int]int{}, Latency: map[string]*percentile{}}
	get := func(m map[string]*counter, k string) *counter {
		if k == "" {
			k = "(none)"
		}
		if m[k] == nil {
			m[k] = &counter{}
		}
		return m[k]
	}
	samples := map[string][]int64{}
	for _, r := range rows {
		status := r.Status
		if r.ProcessError != "" {
			status = "process_error"
			s.ProcessErrors++
		}
		s.Status[status]++
		if r.Error != "" {
			s.TraceErrors++
		}
		s.Violations += len(r.Violations)
		for _, name := range r.RefIrreversible {
			s.RefIrrev++
			if executedOK(r, name) {
				s.RefIrrevDone++
			}
		}
		if r.ProcessError == "" {
			s.FallbackLevel[r.FallbackLevel]++
			for k, v := range r.LatencyMS {
				samples[k] = append(samples[k], v)
			}
			samples["wall"] = append(samples["wall"], r.WallMS)
		}
		if len(r.Expected) == 0 {
			continue
		}
		s.All.add(r)
		get(s.ByLang, r.Lang).add(r)
		get(s.ByPath, r.Path).add(r)
		get(s.ByType, r.Type).add(r)
		get(s.BySession, r.Session).add(r)
		for _, t := range r.Tags {
			get(s.ByTag, t).add(r)
		}
		if len(r.Expected) > 1 {
			for _, e := range uniq(r.Expected) {
				s.IntentRecall[1]++
				if slices.Contains(r.Got, e) {
					s.IntentRecall[0]++
				}
			}
		}
	}
	for k, xs := range samples {
		s.Latency[k] = percentiles(xs)
	}
	return s
}

func executedOK(r row, name string) bool {
	for _, a := range r.Actions {
		if a.Name == name && a.Mode == "execute" && a.Error == "" {
			return true
		}
	}
	return false
}

// percentiles uses the nearest-rank method.
func percentiles(xs []int64) *percentile {
	if len(xs) == 0 {
		return &percentile{}
	}
	ys := slices.Clone(xs)
	slices.Sort(ys)
	rank := func(p float64) int64 {
		i := int(math.Ceil(p*float64(len(ys)))) - 1
		return ys[max(0, min(i, len(ys)-1))]
	}
	var sum int64
	for _, y := range ys {
		sum += y
	}
	return &percentile{N: len(ys), P50: rank(.5), P95: rank(.95), Mean: float64(sum) / float64(len(ys)), Max: ys[len(ys)-1]}
}

func pct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.1f%%)", a, b, 100*float64(a)/float64(b))
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func printCounters(w io.Writer, title string, m map[string]*counter, exact bool) {
	if len(m) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", title)
	for _, k := range sortedKeys(m) {
		c := m[k]
		if exact {
			fmt.Fprintf(w, "  %-22s primary %-18s exact %s\n", k, pct(c.Primary, c.N), pct(c.Exact, c.N))
		} else {
			fmt.Fprintf(w, "  %-22s %s\n", k, pct(c.Primary, c.N))
		}
	}
}

func printSummary(w io.Writer, s *summary, rows []row) {
	fmt.Fprintf(w, "\n=== %s: %d turns ===\n", s.Mode, s.Turns)
	fmt.Fprintf(w, "Primary accuracy: %s\n", pct(s.All.Primary, s.All.N))
	fmt.Fprintf(w, "Exact set match:  %s\n", pct(s.All.Exact, s.All.N))
	if s.IntentRecall[1] > 0 {
		fmt.Fprintf(w, "Intent recall (multi-intent turns): %s\n", pct(s.IntentRecall[0], s.IntentRecall[1]))
	}
	printCounters(w, "By language", s.ByLang, true)
	printCounters(w, "By path", s.ByPath, true)
	if s.Mode == "single" {
		printCounters(w, "By type", s.ByType, true)
	} else {
		printCounters(w, "By tag", s.ByTag, false)
	}
	if s.Mode != "single" {
		fmt.Fprintln(w, "By session (per-turn primary):")
		for _, id := range sessionOrder(rows) {
			c := s.BySession[id]
			statuses := []string{}
			for _, r := range rows {
				if r.Session == id {
					st := r.Status
					if r.ProcessError != "" {
						st = "process_error"
					}
					statuses = append(statuses, st)
				}
			}
			line := "n/a"
			if c != nil {
				line = fmt.Sprintf("%d/%d", c.Primary, c.N)
			}
			fmt.Fprintf(w, "  %-5s %-6s %s\n", id, line, strings.Join(statuses, " > "))
		}
	}
	fmt.Fprint(w, "Status:")
	for _, k := range sortedKeys(s.Status) {
		fmt.Fprintf(w, " %s=%d", k, s.Status[k])
	}
	fmt.Fprint(w, "\nFallback level:")
	levels := []int{}
	for k := range s.FallbackLevel {
		levels = append(levels, k)
	}
	slices.Sort(levels)
	for _, k := range levels {
		fmt.Fprintf(w, " L%d=%d", k, s.FallbackLevel[k])
	}
	fmt.Fprintf(w, "\nErrors: process=%d trace=%d\n", s.ProcessErrors, s.TraceErrors)
	fmt.Fprintln(w, "Latency ms (p50 / p95 / max, n):")
	for _, k := range latencyKeys {
		if p := s.Latency[k]; p != nil {
			fmt.Fprintf(w, "  %-10s %6d / %6d / %6d  n=%d\n", k, p.P50, p.P95, p.Max, p.N)
		}
	}
	if s.RefIrrev > 0 {
		fmt.Fprintf(w, "Reference irreversible executions reproduced on the same turn: %s\n", pct(s.RefIrrevDone, s.RefIrrev))
	}
	fmt.Fprintf(w, "Safety violations: %d\n", s.Violations)
	for _, r := range rows {
		for _, v := range r.Violations {
			fmt.Fprintf(w, "  %s | %s | %s\n", r.ID, r.Text, v)
		}
	}
	misses := []row{}
	for _, r := range rows {
		if len(r.Expected) > 0 && !r.PrimaryOK {
			misses = append(misses, r)
		}
	}
	fmt.Fprintf(w, "Primary misses (%d): id | text | expected | got | reason [status path Lfallback error]\n", len(misses))
	for _, r := range misses {
		fmt.Fprintf(w, "  %s | %s | %s | %s | %s [%s]\n", r.ID, r.Text, strings.Join(r.Expected, ","), strings.Join(r.Got, ","), r.Reason, detail(r))
	}
	if s.Mode == "single" {
		partial := []row{}
		for _, r := range rows {
			if r.PrimaryOK && !r.ExactOK {
				partial = append(partial, r)
			}
		}
		if len(partial) > 0 {
			fmt.Fprintf(w, "Primary right, set wrong (%d):\n", len(partial))
			for _, r := range partial {
				fmt.Fprintf(w, "  %s | %s | %s | %s\n", r.ID, r.Text, strings.Join(r.Expected, ","), strings.Join(r.Got, ","))
			}
		}
	}
	errs := []row{}
	for _, r := range rows {
		if r.ProcessError != "" || r.Error != "" {
			errs = append(errs, r)
		}
	}
	if len(errs) > 0 {
		fmt.Fprintf(w, "Turns with errors (%d):\n", len(errs))
		for _, r := range errs {
			fmt.Fprintf(w, "  %s | process=%q trace=%q | status=%s L%d\n", r.ID, r.ProcessError, r.Error, r.Status, r.FallbackLevel)
		}
	}
}

func detail(r row) string {
	parts := []string{r.Status, r.Path, fmt.Sprintf("L%d", r.FallbackLevel)}
	if r.ProcessError != "" {
		parts = append(parts, "process_error="+r.ProcessError)
	}
	if r.Error != "" {
		parts = append(parts, "error="+r.Error)
	}
	return strings.Join(parts, " ")
}

func sessionOrder(rows []row) []string {
	out := []string{}
	for _, r := range rows {
		if !slices.Contains(out, r.Session) {
			out = append(out, r.Session)
		}
	}
	return out
}
