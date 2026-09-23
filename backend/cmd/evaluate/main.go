// Evaluate replays the dataset through the real turn loop: Engine.Process with
// the OpenAI model and an in-memory repository, one session per utterance,
// dialog or probe. It reports routing accuracy, statuses, latency per stage
// and an irreversible-action safety check. Modes single, dialogs and probes
// make paid API calls; retrieval is offline.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"voice-router/internal/router"
)

type options struct {
	mode, dataset, input, probes, out, predictions, env, ids string
	limit, concurrency, k                                    int
	scorer, quiet                                            bool
	turnTimeout, fullTimeout, fastTimeout                    time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.mode, "mode", "single", "single | dialogs | probes | retrieval")
	flag.IntVar(&o.limit, "limit", 0, "maximum sessions (utterances, dialogs or probes); 0 = all")
	flag.StringVar(&o.ids, "ids", "", "comma-separated case IDs to run, e.g. U001,U083 or D03 or P05")
	flag.IntVar(&o.concurrency, "concurrency", 4, "sessions processed in parallel")
	flag.StringVar(&o.dataset, "dataset", "../voice_router_dataset", "dataset directory")
	flag.StringVar(&o.input, "input", "", "dev utterances file (default <dataset>/dev_utterances.json)")
	flag.StringVar(&o.probes, "probes", "cmd/evaluate/testdata/probes.json", "probe set for -mode probes")
	flag.StringVar(&o.out, "out", "", "write the per-turn report (summary + one row per turn) as JSON here")
	flag.StringVar(&o.predictions, "predictions", "predictions.json", "single mode: predictions file for evaluate.py")
	flag.BoolVar(&o.scorer, "scorer", true, "single mode: run <dataset>/evaluate.py on the predictions")
	flag.StringVar(&o.env, "env", ".env", "dotenv file for OPENAI_* variables; variables already set win")
	flag.DurationVar(&o.turnTimeout, "turn-timeout", 90*time.Second, "context timeout per turn")
	flag.DurationVar(&o.fullTimeout, "full-timeout", 0, "override Policy.FullTimeout (0 = engine default)")
	flag.DurationVar(&o.fastTimeout, "fast-timeout", 0, "override Policy.FastTimeout (0 = engine default)")
	flag.IntVar(&o.k, "k", 8, "retrieval mode: shortlist size")
	flag.BoolVar(&o.quiet, "quiet", false, "suppress per-turn progress on stderr")
	flag.Parse()
	if o.input == "" {
		o.input = filepath.Join(o.dataset, "dev_utterances.json")
	}
	catalog, err := router.LoadCatalog()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch o.mode {
	case "retrieval":
		dev, err := loadDev(o.input)
		if err != nil {
			log.Fatal(err)
		}
		dev = selectDev(dev, o.ids, o.limit)
		rows, groups := runRetrieval(catalog, dev, o.k)
		printRetrieval(os.Stdout, rows, groups, o.k)
		writeReport(o.out, "retrieval", map[string]any{"k": o.k, "input": o.input}, groups, rows)
	case "single", "dialogs", "probes":
		runModel(ctx, o, catalog)
	default:
		log.Fatalf("unknown -mode %q", o.mode)
	}
}

func runModel(ctx context.Context, o options, catalog *router.Catalog) {
	var cases []sessionCase
	var dev []devUtterance
	switch o.mode {
	case "single":
		all, err := loadDev(o.input)
		if err != nil {
			log.Fatal(err)
		}
		dev = selectDev(all, o.ids, o.limit)
		cases = devCases(dev)
	case "dialogs":
		all, err := loadDialogs(filepath.Join(o.dataset, "dialogs_sample.json"), catalog)
		if err != nil {
			log.Fatal(err)
		}
		cases = selectCases(all, o.ids, o.limit)
	case "probes":
		all, err := loadProbes(o.probes)
		if err != nil {
			log.Fatal(err)
		}
		cases = selectCases(all, o.ids, o.limit)
	}
	if len(cases) == 0 {
		log.Fatal("no cases selected")
	}
	if err := router.LoadDotEnv(o.env); err != nil {
		log.Fatal(err)
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		log.Fatal("Set OPENAI_API_KEY (or pass -env); this mode makes paid API calls")
	}
	model := envOr("OPENAI_MODEL", "gpt-4.1-mini")
	fast := envOr("OPENAI_FAST_MODEL", "gpt-4.1-nano")
	openai := router.NewOpenAI(key, model, catalog)
	openai.FastModel = fast
	engine := router.NewEngine(catalog, openai, router.NewMemoryRepository(catalog))
	engine.Policy.FallbackModel = envOr("OPENAI_FALLBACK_MODEL", fast)
	if o.fullTimeout > 0 {
		engine.Policy.FullTimeout = o.fullTimeout
	}
	if o.fastTimeout > 0 {
		engine.Policy.FastTimeout = o.fastTimeout
	}
	turns := 0
	for _, c := range cases {
		turns += len(c.Turns)
	}
	p := engine.Policy
	config := map[string]any{"model": model, "fast_model": fast, "fallback_model": p.FallbackModel, "concurrency": o.concurrency, "policy": map[string]any{"shortlist_size": p.ShortlistSize, "fast_candidates": p.FastCandidates, "fast_margin": p.FastMargin, "fast_min_score": p.FastMinScore, "execute": p.Execute, "handoff": p.Handoff, "l2_margin": p.L2Margin, "fast_timeout": p.FastTimeout.String(), "full_timeout": p.FullTimeout.String()}, "max_steps": engine.MaxSteps, "turn_budget": engine.Timeout.String(), "sessions": len(cases), "turns": turns}
	fmt.Printf("evaluate mode=%s sessions=%d turns=%d model=%s fast=%s fallback=%s concurrency=%d\n", o.mode, len(cases), turns, model, fast, p.FallbackModel, o.concurrency)
	fmt.Printf("policy: shortlist=%d fast_candidates=%d fast_margin=%.2f fast_min=%.2f exec<=%.2f handoff>%.2f fast_timeout=%s full_timeout=%s turn_budget=%s\n", p.ShortlistSize, p.FastCandidates, p.FastMargin, p.FastMinScore, p.Execute, p.Handoff, p.FastTimeout, p.FullTimeout, engine.Timeout)
	r := &runner{base: engine, catalog: catalog, mode: o.mode, timeout: o.turnTimeout}
	if !o.quiet {
		r.progress = os.Stderr
	}
	start := time.Now()
	rows := r.runAll(ctx, cases, o.concurrency)
	fmt.Printf("elapsed %s\n", time.Since(start).Round(time.Second))
	s := summarize(o.mode, rows)
	printSummary(os.Stdout, s, rows)
	writeReport(o.out, o.mode, config, s, rows)
	if o.mode == "single" {
		writePredictions(o, dev, rows)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func selectDev(dev []devUtterance, ids string, limit int) []devUtterance {
	cases := selectCases(devCases(dev), ids, limit)
	keep := map[string]bool{}
	for _, c := range cases {
		keep[c.ID] = true
	}
	out := []devUtterance{}
	for _, u := range dev {
		if keep[u.ID] {
			out = append(out, u)
		}
	}
	return out
}

func writeReport(path, mode string, config, summary, rows any) {
	if path == "" {
		return
	}
	b, err := json.MarshalIndent(map[string]any{"mode": mode, "generated_at": time.Now().UTC().Format(time.RFC3339), "config": config, "summary": summary, "rows": rows}, "", "  ")
	if err == nil {
		err = os.WriteFile(path, b, 0o644)
	}
	if err != nil {
		log.Printf("write report: %v", err)
		return
	}
	fmt.Printf("report: %s\n", path)
}

// writePredictions writes the reference scorer's input and runs evaluate.py.
// A partial run is scored against the matching subset of the dev set, so
// skipped utterances do not count as errors.
func writePredictions(o options, dev []devUtterance, rows []row) {
	preds := map[string][]string{}
	for _, r := range rows {
		preds[r.ID] = r.Got
	}
	b, _ := json.MarshalIndent(preds, "", "  ")
	if err := os.WriteFile(o.predictions, b, 0o644); err != nil {
		log.Printf("write predictions: %v", err)
		return
	}
	fmt.Printf("predictions: %s\n", o.predictions)
	if !o.scorer {
		return
	}
	devPath := o.input
	if all, err := loadDev(o.input); err == nil && len(all) != len(dev) {
		devPath = strings.TrimSuffix(o.predictions, ".json") + ".dev_subset.json"
		b, _ := json.MarshalIndent(map[string]any{"utterances": dev}, "", "  ")
		if err := os.WriteFile(devPath, b, 0o644); err != nil {
			log.Printf("write dev subset: %v", err)
			return
		}
	}
	fmt.Printf("\n=== evaluate.py %s %s ===\n", o.predictions, devPath)
	cmd := exec.Command("python3", filepath.Join(o.dataset, "evaluate.py"), o.predictions, devPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("evaluate.py: %v", err)
	}
}
