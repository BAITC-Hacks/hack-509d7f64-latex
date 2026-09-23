package router

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Retrieval tunables, chosen by leave-one-example-out recall over the
// catalog's own examples; dev utterances only measure the result.
const (
	gramMin, gramMax = 3, 5
	// A scenario scores bestWeight·(best single example) + (1−bestWeight)·
	// (scenario centroid). The centroid generalises better from seven
	// examples; the best example keeps near-paraphrases clearly ahead.
	bestWeight = .2
)

var placeholder = regexp.MustCompile(`\{[a-z_]+\}`)

// Retriever ranks business scenarios by lexical similarity to an utterance:
// TF-IDF over word-boundary character n-grams with cosine similarity. The
// n-grams tolerate Russian and Kazakh inflection and mixed speech without a
// stemmer or a network call. Retrieval narrows what the model reads and gives
// the uncertainty score a second opinion; it never picks a scenario.
type Retriever struct {
	ids      []string       // business scenarios in catalog order
	index    map[string]int // scenario ID -> position in ids
	features map[string]int // n-gram -> feature
	idf      []float64
	unseen   float64     // idf of an n-gram the catalog never uses
	postings [][]posting // feature -> documents that contain it
	owner    []int       // document -> scenario position
	centroid []bool      // document is its scenario's centroid
}

type posting struct {
	doc    int32
	weight float64
}

type gram struct {
	text string
	tf   float64
}

type entry struct {
	feature int
	weight  float64
}

// NewRetriever indexes every business scenario once. Examples (ru + kk) are
// matched individually and also form the centroid together with the name,
// description and response templates. not_this_if conditions are not indexed:
// they describe other scenarios.
func NewRetriever(c *Catalog) *Retriever {
	r := &Retriever{index: map[string]int{}, features: map[string]int{}}
	docs, matched, df := [][][]gram{}, []int{}, []int{}
	for i, sc := range c.Ordered {
		r.ids = append(r.ids, sc.ID)
		r.index[sc.ID] = i
		d := [][]gram{}
		for _, lang := range []string{"ru", "kk"} {
			for _, text := range sc.Examples[lang] {
				d = append(d, grams(text))
			}
		}
		matched = append(matched, len(d))
		d = append(d, grams(sc.Name+" "+sc.Description))
		for _, lang := range []string{"ru", "kk"} {
			for _, key := range []string{"opening", "closing"} {
				d = append(d, grams(placeholder.ReplaceAllString(sc.Responses[lang][key], " ")))
			}
		}
		docs = append(docs, d)
		// Document frequency counts scenarios, so an n-gram that every
		// scenario uses weighs nothing however often it occurs.
		seen := map[string]bool{}
		for _, doc := range d {
			for _, g := range doc {
				if seen[g.text] {
					continue
				}
				seen[g.text] = true
				f, ok := r.features[g.text]
				if !ok {
					f = len(df)
					r.features[g.text] = f
					df = append(df, 0)
				}
				df[f]++
			}
		}
	}
	n := float64(len(r.ids))
	for _, k := range df {
		r.idf = append(r.idf, math.Log((1+n)/(1+float64(k))))
	}
	r.unseen = math.Log(1 + n)
	r.postings = make([][]posting, len(df))
	add := func(owner int, centroid bool, v []entry) {
		doc := int32(len(r.owner))
		r.owner = append(r.owner, owner)
		r.centroid = append(r.centroid, centroid)
		for _, e := range v {
			r.postings[e.feature] = append(r.postings[e.feature], posting{doc, e.weight})
		}
	}
	for i, d := range docs {
		sum := make([]float64, len(df))
		for j, doc := range d {
			v := r.vector(doc)
			if j < matched[i] {
				add(i, false, v)
			}
			for _, e := range v {
				sum[e.feature] += e.weight
			}
		}
		centroid := []entry{}
		for f, w := range sum {
			if w > 0 {
				centroid = append(centroid, entry{f, w})
			}
		}
		add(i, true, unit(centroid))
	}
	return r
}

// vector weights a document's n-grams by idf and scales it to unit length.
func (r *Retriever) vector(doc []gram) []entry {
	v := []entry{}
	for _, g := range doc {
		if f := r.features[g.text]; r.idf[f] > 0 {
			v = append(v, entry{f, g.tf * r.idf[f]})
		}
	}
	return unit(v)
}

func unit(v []entry) []entry {
	norm := 0.0
	for _, e := range v {
		norm += e.weight * e.weight
	}
	if norm == 0 {
		return nil
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i].weight /= norm
	}
	return v
}

// normalizeText lowercases, folds ё into е and reduces everything except
// letters and digits (Kazakh letters included) to single spaces.
func normalizeText(s string) string {
	var b strings.Builder
	space := true
	for _, c := range strings.ToLower(s) {
		if c == 'ё' {
			c = 'е'
		}
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			b.WriteRune(c)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// grams returns the word-boundary padded character n-grams of text with
// sublinear term frequency, sorted so every floating-point sum over them runs
// in the same order and scores are bit-for-bit reproducible.
func grams(text string) []gram {
	text = normalizeText(text)
	counts := make(map[string]int, 3*len(text)/2)
	for _, w := range strings.Fields(text) {
		if strings.IndexFunc(w, func(c rune) bool { return !unicode.IsDigit(c) }) < 0 {
			continue // numbers are slot values, not intent
		}
		rs := []rune(" " + w + " ")
		for n := gramMin; n <= gramMax; n++ {
			for i := 0; i+n <= len(rs); i++ {
				counts[string(rs[i:i+n])]++
			}
		}
	}
	out := make([]gram, 0, len(counts))
	for g, k := range counts {
		out = append(out, gram{g, 1 + math.Log(float64(k))})
	}
	slices.SortFunc(out, func(a, b gram) int { return strings.Compare(a.text, b.text) })
	return out
}

// scores returns one similarity in [0, 1] per business scenario. N-grams the
// catalog never uses still count toward the utterance's length, so text the
// catalog cannot explain scores low for every scenario.
func (r *Retriever) scores(text string) []float64 {
	scores := make([]float64, len(r.ids))
	dots := make([]float64, len(r.owner))
	norm := 0.0
	for _, g := range grams(text) {
		f, ok := r.features[g.text]
		if !ok {
			norm += g.tf * r.unseen * g.tf * r.unseen
			continue
		}
		w := g.tf * r.idf[f]
		norm += w * w
		for _, p := range r.postings[f] {
			dots[p.doc] += w * p.weight
		}
	}
	if norm == 0 {
		return scores
	}
	norm = math.Sqrt(norm)
	best, centroid := make([]float64, len(r.ids)), make([]float64, len(r.ids))
	for doc, dot := range dots {
		s, o := dot/norm, r.owner[doc]
		if r.centroid[doc] {
			centroid[o] = s
		} else {
			best[o] = max(best[o], s)
		}
	}
	for i := range scores {
		scores[i] = min(1, max(0, bestWeight*best[i]+(1-bestWeight)*centroid[i]))
	}
	return scores
}

// Shortlist returns up to k business scenarios with a positive score, plus
// every forced ID (scenarios already in play) with its real score even when
// it falls outside the top k, sorted by score desc and then by ID. Text with
// no catalog evidence yields only the forced IDs, so the model reads the full
// catalog instead of an arbitrary slice of it.
func (r *Retriever) Shortlist(text string, k int, forced ...string) []ScoredScenario {
	scores := r.scores(text)
	byScore := func(a, b ScoredScenario) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), strings.Compare(a.ScenarioID, b.ScenarioID))
	}
	all := make([]ScoredScenario, len(r.ids))
	for i, id := range r.ids {
		all[i] = ScoredScenario{ScenarioID: id, Score: scores[i]}
	}
	slices.SortFunc(all, byScore)
	out := []ScoredScenario{}
	seen := map[string]bool{}
	for _, s := range all {
		if len(out) >= k || s.Score <= 0 {
			break
		}
		seen[s.ScenarioID] = true
		out = append(out, s)
	}
	for _, id := range forced {
		if i, ok := r.index[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, ScoredScenario{ScenarioID: id, Score: scores[i]})
		}
	}
	slices.SortFunc(out, byScore)
	return out
}
