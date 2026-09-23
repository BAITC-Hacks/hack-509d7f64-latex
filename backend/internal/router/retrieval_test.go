package router

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func testRetriever(t *testing.T) (*Retriever, *Catalog) {
	t.Helper()
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return NewRetriever(c), c
}

func checkShortlist(t *testing.T, c *Catalog, got []ScoredScenario) {
	t.Helper()
	seen := map[string]bool{}
	for i, s := range got {
		if _, ok := c.Scenarios[s.ScenarioID]; !ok || seen[s.ScenarioID] {
			t.Fatalf("unknown or duplicate scenario %q in %v", s.ScenarioID, got)
		}
		seen[s.ScenarioID] = true
		if math.IsNaN(s.Score) || s.Score < 0 || s.Score > 1 {
			t.Fatalf("score out of range: %v", s)
		}
		if i > 0 && (got[i-1].Score < s.Score || got[i-1].Score == s.Score && got[i-1].ScenarioID > s.ScenarioID) {
			t.Fatalf("not sorted by score desc then ID: %v", got)
		}
	}
}

func TestShortlistBoundsAndOrder(t *testing.T) {
	r, c := testRetriever(t)
	for _, text := range []string{"Сколько стоит обязательная страховка на машину?", "Көлікке міндетті сақтандыру рәсімдегім келеді", "Каско керек, машина жаңа, посчитайте", "где офис"} {
		for _, k := range []int{0, 1, 3, 8, 40, 100} {
			got := r.Shortlist(text, k)
			checkShortlist(t, c, got)
			if len(got) > k {
				t.Fatalf("k=%d returned %d", k, len(got))
			}
		}
		if got := r.Shortlist(text, 8); len(got) != 8 {
			t.Fatalf("expected a full shortlist for %q, got %v", text, got)
		}
	}
	if got := r.Shortlist("Подскажите адрес офиса в Караганде и до скольки работаете", 3); got[0].ScenarioID != "SC33" || got[0].Score <= got[1].Score {
		t.Fatalf("office question: %v", got)
	}
}

func TestShortlistForcedIDs(t *testing.T) {
	r, c := testRetriever(t)
	text := "Где ваш офис в Алматы?"
	full := r.Shortlist(text, 40)
	var far ScoredScenario
	for _, s := range full {
		if s.ScenarioID == "SC12" {
			far = s
		}
	}
	got := r.Shortlist(text, 3, "SC12", "SYS_GOODBYE", "nope", "SC12", full[0].ScenarioID)
	checkShortlist(t, c, got)
	if len(got) != 4 || !slices.Contains(got, far) {
		t.Fatalf("forced SC12 must be added once with its real score %v: %v", far, got)
	}
	for _, s := range got {
		if strings.HasPrefix(s.ScenarioID, "SYS_") {
			t.Fatalf("system intent in shortlist: %v", got)
		}
	}
	if got := r.Shortlist("", 8, "SC02"); len(got) != 1 || got[0] != (ScoredScenario{ScenarioID: "SC02"}) {
		t.Fatalf("empty text keeps only forced IDs: %v", got)
	}
}

func TestShortlistDeterministicAndSafe(t *testing.T) {
	r, c := testRetriever(t)
	text := "Сәлеметсіз бе, полисті продлить ету керек, ссылку на оплату жіберіңіз"
	first := r.Shortlist(text, 40)
	for i := 0; i < 3; i++ {
		for _, again := range []*Retriever{r, NewRetriever(c)} {
			if got := again.Shortlist(text, 40); !slices.Equal(got, first) {
				t.Fatalf("run %d differs: %v vs %v", i, got, first)
			}
		}
	}
	for _, text := range []string{"", "   ", "?!…", "12345 67890", "zzzzqqqq xxyyzz", strings.Repeat("ааааа ", 2000), "\x00\xff\xfe", "🙂🙂🙂"} {
		got := r.Shortlist(text, 8)
		checkShortlist(t, c, got)
		if len(got) > 8 {
			t.Fatalf("%q: %v", text, got)
		}
	}
	for _, text := range []string{"", "?!…", "12345"} {
		if got := r.Shortlist(text, 8); len(got) != 0 {
			t.Fatalf("no evidence must mean no candidates for %q: %v", text, got)
		}
	}
}

func TestNormalizeText(t *testing.T) {
	for in, want := range map[string]string{
		"  Всё   ОК!!  ":         "все ок",
		"Сақтандыру — МРТ-ны?":   "сақтандыру мрт ны",
		"ӘІҢҒҮҰҚӨҺ, ёлка":        "әіңғүұқөһ елка",
		"Saqta,SMS\tкод\n+7 707": "saqta sms код 7 707",
	} {
		if got := normalizeText(in); got != want {
			t.Fatalf("normalizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// labelled is one utterance with its expected first scenario.
type labelled struct{ text, lang, want string }

func devUtterances(t *testing.T) (business, system []labelled) {
	t.Helper()
	b, err := os.ReadFile("../../../voice_router_dataset/dev_utterances.json")
	if err != nil {
		t.Skip("dataset not available:", err)
	}
	var d struct {
		Utterances []struct {
			Text     string   `json:"text"`
			Lang     string   `json:"lang"`
			Expected []string `json:"expected"`
		} `json:"utterances"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	for _, u := range d.Utterances {
		item := labelled{u.Text, u.Lang, u.Expected[0]}
		if strings.HasPrefix(u.Expected[0], "SYS_") {
			system = append(system, item)
		} else {
			business = append(business, item)
		}
	}
	return business, system
}

// dialogTopicStarts returns client turns whose first scenario differs from
// the previous client turn's, i.e. where a new topic begins.
func dialogTopicStarts(t *testing.T) []labelled {
	t.Helper()
	b, err := os.ReadFile("../../../voice_router_dataset/dialogs_sample.json")
	if err != nil {
		t.Skip("dataset not available:", err)
	}
	var d struct {
		Dialogs []struct {
			Turns []struct {
				Role      string   `json:"role"`
				Text      string   `json:"text"`
				Lang      string   `json:"lang"`
				Scenarios []string `json:"scenarios"`
			} `json:"turns"`
		} `json:"dialogs"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	out := []labelled{}
	for _, dialog := range d.Dialogs {
		prev := ""
		for _, turn := range dialog.Turns {
			if turn.Role != "client" || len(turn.Scenarios) == 0 {
				continue
			}
			if first := turn.Scenarios[0]; first != prev && !strings.HasPrefix(first, "SYS_") {
				out = append(out, labelled{turn.Text, turn.Lang, first})
			}
			prev = turn.Scenarios[0]
		}
	}
	return out
}

type recallAt struct{ n, at1, at3, at8 int }

func (r recallAt) String() string {
	f := func(k int) float64 { return float64(k) / float64(max(r.n, 1)) }
	return fmt.Sprintf("n=%d r@1=%.3f r@3=%.3f r@8=%.3f", r.n, f(r.at1), f(r.at3), f(r.at8))
}

func measureRecall(r *Retriever, items []labelled) map[string]*recallAt {
	out := map[string]*recallAt{"all": {}}
	for _, it := range items {
		rank := slices.IndexFunc(r.Shortlist(it.text, 40), func(s ScoredScenario) bool { return s.ScenarioID == it.want }) + 1
		for _, key := range []string{"all", it.lang} {
			if out[key] == nil {
				out[key] = &recallAt{}
			}
			m := out[key]
			m.n++
			if rank > 0 && rank <= 1 {
				m.at1++
			}
			if rank > 0 && rank <= 3 {
				m.at3++
			}
			if rank > 0 && rank <= 8 {
				m.at8++
			}
		}
	}
	return out
}

func TestRetrievalRecall(t *testing.T) {
	r, _ := testRetriever(t)
	dev, _ := devUtterances(t)
	for _, set := range []struct {
		name  string
		items []labelled
	}{{"dev", dev}, {"dialog topic starts", dialogTopicStarts(t)}} {
		m := measureRecall(r, set.items)
		for _, key := range []string{"all", "ru", "kk", "mixed"} {
			if m[key] != nil {
				t.Logf("%s %-5s %v", set.name, key, m[key])
			}
		}
		if set.name == "dev" && float64(m["all"].at8) < .9*float64(m["all"].n) {
			t.Fatalf("dev recall@8 below 0.9: %v", m["all"])
		}
	}
}

// TestRetrievalHeldOutExamples is the tuning objective: hold out example j of
// every scenario in turn, index the rest and rank the held-out examples.
func TestRetrievalHeldOutExamples(t *testing.T) {
	_, c := testRetriever(t)
	total := map[string]*recallAt{}
	for _, lang := range []string{"ru", "kk"} {
		for j := 0; ; j++ {
			held, reduced := []labelled{}, *c
			reduced.Ordered = nil
			for _, sc := range c.Ordered {
				examples := map[string][]string{}
				for l, xs := range sc.Examples {
					examples[l] = slices.Clone(xs)
				}
				if j < len(examples[lang]) {
					held = append(held, labelled{examples[lang][j], lang, sc.ID})
					examples[lang] = slices.Delete(examples[lang], j, j+1)
				}
				sc.Examples = examples
				reduced.Ordered = append(reduced.Ordered, sc)
			}
			if len(held) == 0 {
				break
			}
			for key, m := range measureRecall(NewRetriever(&reduced), held) {
				if total[key] == nil {
					total[key] = &recallAt{}
				}
				total[key].n, total[key].at1, total[key].at3, total[key].at8 = total[key].n+m.n, total[key].at1+m.at1, total[key].at3+m.at3, total[key].at8+m.at8
			}
		}
	}
	for _, key := range []string{"all", "ru", "kk"} {
		t.Logf("held-out catalog examples %-3s %v", key, total[key])
	}
}

func TestRetrievalLatency(t *testing.T) {
	r, _ := testRetriever(t)
	dev, sys := devUtterances(t)
	start := time.Now()
	n := 0
	for i := 0; i < 5; i++ {
		for _, it := range append(dev, sys...) {
			r.Shortlist(it.text, 8, "SC01", "SC17")
			n++
		}
	}
	avg := time.Since(start) / time.Duration(n)
	t.Logf("average Shortlist latency %v over %d calls", avg, n)
	if avg > 2*time.Millisecond {
		t.Fatalf("Shortlist too slow: %v", avg)
	}
}

func BenchmarkShortlist(b *testing.B) {
	c, err := LoadCatalog()
	if err != nil {
		b.Fatal(err)
	}
	r := NewRetriever(c)
	for b.Loop() {
		r.Shortlist("Сәлеметсіз бе, полисті продлить ету керек, ссылку на оплату жіберіңіз", 8, "SC17")
	}
}

type scored struct {
	top1, want    string
	score, margin float64
}

func topScores(r *Retriever, items []labelled) []scored {
	out := []scored{}
	for _, it := range items {
		list := r.Shortlist(it.text, 2)
		s := scored{want: it.want}
		if len(list) > 0 {
			s.top1, s.score, s.margin = list[0].ScenarioID, list[0].Score, list[0].Score
		}
		if len(list) > 1 {
			s.margin -= list[1].Score
		}
		out = append(out, s)
	}
	return out
}

type gatePick struct {
	minScore, margin float64
	pass, correct    int
}

func thresholds(hi float64) []float64 {
	out := []float64{}
	for v := 0.0; v <= hi+1e-9; v += .005 {
		out = append(out, math.Round(v*1000)/1000)
	}
	return out
}

// gate counts the rows the fast gate would admit: top-1 fast-path eligible,
// score and lead at or above the thresholds.
func gate(c *Catalog, rows []scored, minScore, margin float64) gatePick {
	p := gatePick{minScore: minScore, margin: margin}
	for _, s := range rows {
		if s.top1 != "" && c.Scenarios[s.top1].FastPath && s.score >= minScore && s.margin >= margin {
			p.pass++
			if s.top1 == s.want {
				p.correct++
			}
		}
	}
	return p
}

// fastFrontier returns, per FastMargin, the loosest FastMinScore whose top-1
// precision is >= 0.97, i.e. the most correct fast routes at that margin.
func fastFrontier(c *Catalog, rows []scored) []gatePick {
	out := []gatePick{}
	for _, mg := range thresholds(.5) {
		for _, ms := range thresholds(.8) {
			if p := gate(c, rows, ms, mg); p.pass > 0 && float64(p.correct) >= .97*float64(p.pass) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// l2Curve returns top-1 precision over business utterances per L2Margin.
func l2Curve(rows []scored) []gatePick {
	out := []gatePick{}
	for _, mg := range thresholds(.5) {
		p := gatePick{margin: mg}
		for _, s := range rows {
			if s.top1 != "" && s.margin >= mg {
				p.pass++
				if s.top1 == s.want {
					p.correct++
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// TestRetrievalCalibration reports gate thresholds measured on dev: the fast
// gate (FastMinScore, FastMargin) with top-1 precision >= 0.97 among fast-path
// eligible top-1s, and the L2 margin with top-1 precision >= 0.9 over business
// utterances. It only logs; DefaultPolicy takes its values from this output.
func TestRetrievalCalibration(t *testing.T) {
	r, c := testRetriever(t)
	dev, sys := devUtterances(t)
	rows, sysRows := topScores(r, dev), topScores(r, sys)
	eligible := 0
	for _, s := range rows {
		if c.Scenarios[s.want].FastPath {
			eligible++
		}
	}
	for _, s := range rows {
		if c.Scenarios[s.top1].FastPath {
			t.Logf("fast-eligible top-1: want=%s top1=%s score=%.3f margin=%.3f", s.want, s.top1, s.score, s.margin)
		}
	}
	best := gatePick{}
	for _, p := range fastFrontier(c, rows) {
		if p.correct > best.correct {
			best = p
		}
		if int(math.Round(p.margin*1000))%50 == 0 {
			t.Logf("fast frontier: FastMargin=%.3f loosest FastMinScore=%.3f precision=%d/%d", p.margin, p.minScore, p.correct, p.pass)
		}
	}
	t.Logf("fast gate max coverage: FastMinScore=%.3f FastMargin=%.3f precision=%d/%d coverage=%d of %d fast-path labelled dev utterances, system intents admitted=%d/%d",
		best.minScore, best.margin, best.correct, best.pass, best.correct, eligible, gate(c, sysRows, best.minScore, best.margin).pass, len(sysRows))
	for _, ms := range []float64{.25, .275, .3, .325, .35} {
		line := fmt.Sprintf("fast grid FastMinScore=%.3f:", ms)
		for _, mg := range []float64{0, .05, .1, .15, .2} {
			p := gate(c, rows, ms, mg)
			line += fmt.Sprintf("  margin %.2f %d/%d", mg, p.correct, p.pass)
		}
		t.Log(line)
	}
	def := DefaultPolicy()
	p := gate(c, rows, def.FastMinScore, def.FastMargin)
	t.Logf("fast gate DefaultPolicy: FastMinScore=%.3f FastMargin=%.3f precision=%d/%d", def.FastMinScore, def.FastMargin, p.correct, p.pass)
	curve := l2Curve(rows)
	first, stable := -1.0, -1.0
	for i, p := range curve {
		ok := p.pass == 0 || float64(p.correct) >= .9*float64(p.pass)
		if ok && first < 0 && p.pass > 0 {
			first = p.margin
		}
		if !ok {
			stable = -1
		} else if stable < 0 {
			stable = curve[i].margin
		}
		if int(math.Round(p.margin*1000))%25 == 0 && p.pass > 0 {
			t.Logf("L2 curve: L2Margin=%.3f precision=%d/%d coverage=%d/%d", p.margin, p.correct, p.pass, p.pass, len(rows))
		}
	}
	for _, mg := range []float64{first, stable, def.L2Margin} {
		for _, p := range curve {
			if math.Abs(p.margin-mg) < 1e-9 {
				t.Logf("L2 pick: L2Margin=%.3f precision=%d/%d=%.3f coverage=%d/%d", p.margin, p.correct, p.pass, float64(p.correct)/float64(p.pass), p.pass, len(rows))
			}
		}
	}
}
