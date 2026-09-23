package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"voice-router/internal/router"
)

type retrievalRow struct {
	ID        string                  `json:"id"`
	Text      string                  `json:"text"`
	Lang      string                  `json:"lang"`
	Expected  []string                `json:"expected"`
	Shortlist []router.ScoredScenario `json:"shortlist"`
	Rank      int                     `json:"rank"` // 1-based rank of expected[0]; 0 = not in shortlist
	AllIn     bool                    `json:"all_expected_in_shortlist"`
	Micros    int64                   `json:"micros"`
}

type recall struct {
	N     int `json:"n"`
	At1   int `json:"at1"`
	At3   int `json:"at3"`
	At8   int `json:"at8"`
	AllAt int `json:"all_expected_at8"`
}

func (r *recall) add(x retrievalRow) {
	r.N++
	if x.Rank == 1 {
		r.At1++
	}
	if x.Rank >= 1 && x.Rank <= 3 {
		r.At3++
	}
	if x.Rank >= 1 && x.Rank <= 8 {
		r.At8++
	}
	if x.AllIn {
		r.AllAt++
	}
}

// runRetrieval measures the retrieval shortlist offline: no model calls. Only
// utterances whose primary label is a business scenario count; system intents
// are never retrieved.
func runRetrieval(c *router.Catalog, dev []devUtterance, k int) ([]retrievalRow, map[string]*recall) {
	retriever := router.NewRetriever(c)
	rows := []retrievalRow{}
	groups := map[string]*recall{"all": {}}
	for _, u := range dev {
		if len(u.Expected) == 0 || !strings.HasPrefix(u.Expected[0], "SC") {
			continue
		}
		start := time.Now()
		shortlist := retriever.Shortlist(u.Text, k)
		x := retrievalRow{ID: u.ID, Text: u.Text, Lang: u.Lang, Expected: u.Expected, Shortlist: nonNil(shortlist), Micros: time.Since(start).Microseconds(), AllIn: true}
		ids := []string{}
		for _, s := range shortlist {
			ids = append(ids, s.ScenarioID)
		}
		for i, id := range ids {
			if id == u.Expected[0] {
				x.Rank = i + 1
				break
			}
		}
		top := ids[:min(k, len(ids))]
		for _, e := range u.Expected {
			if strings.HasPrefix(e, "SC") && !slices.Contains(top, e) {
				x.AllIn = false
			}
		}
		rows = append(rows, x)
		groups["all"].add(x)
		for _, g := range []string{"lang=" + u.Lang, "type=" + u.Type} {
			if groups[g] == nil {
				groups[g] = &recall{}
			}
			groups[g].add(x)
		}
	}
	return rows, groups
}

func printRetrieval(w io.Writer, rows []retrievalRow, groups map[string]*recall, k int) {
	empty := 0
	var micros int64
	for _, r := range rows {
		if len(r.Shortlist) == 0 {
			empty++
		}
		micros += r.Micros
	}
	fmt.Fprintf(w, "\n=== retrieval: %d dev utterances with a business primary label, k=%d ===\n", len(rows), k)
	if len(rows) > 0 {
		fmt.Fprintf(w, "Mean Shortlist time: %dus; empty shortlists: %d\n", micros/int64(len(rows)), empty)
	}
	for _, g := range sortedKeys(groups) {
		r := groups[g]
		fmt.Fprintf(w, "  %-18s recall@1 %-16s @3 %-16s @8 %-16s all-expected@8 %s\n", g, pct(r.At1, r.N), pct(r.At3, r.N), pct(r.At8, r.N), pct(r.AllAt, r.N))
	}
	missed := 0
	for _, r := range rows {
		if r.Rank == 0 || r.Rank > 3 {
			missed++
		}
	}
	if missed > 0 && missed < len(rows) {
		fmt.Fprintf(w, "Expected primary outside top-3 (%d): id | text | expected | top-3\n", missed)
		for _, r := range rows {
			if r.Rank == 0 || r.Rank > 3 {
				top := []string{}
				for _, s := range r.Shortlist[:min(3, len(r.Shortlist))] {
					top = append(top, fmt.Sprintf("%s:%.2f", s.ScenarioID, s.Score))
				}
				fmt.Fprintf(w, "  %s | %s | %s | %s\n", r.ID, r.Text, strings.Join(r.Expected, ","), strings.Join(top, " "))
			}
		}
	}
}
