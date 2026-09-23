// Evaluate routes the dataset through the real LLM, without executing tools.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
	"voice-router/internal/router"
)

func main() {
	input := flag.String("input", "../voice_router_dataset/dev_utterances.json", "development utterances")
	output := flag.String("output", "predictions.json", "reference scorer output")
	limit := flag.Int("limit", 0, "maximum utterances (0 = all)")
	flag.Parse()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		log.Fatal("Set OPENAI_API_KEY; evaluation makes paid API calls")
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4.1-mini"
	}
	c, err := router.LoadCatalog()
	if err != nil {
		log.Fatal(err)
	}
	m := router.NewOpenAI(key, model, c)
	data, err := os.ReadFile(*input)
	if err != nil {
		log.Fatal(err)
	}
	var set struct {
		Utterances []struct {
			ID       string   `json:"id"`
			Text     string   `json:"text"`
			Lang     string   `json:"lang"`
			Expected []string `json:"expected"`
		} `json:"utterances"`
	}
	if err := json.Unmarshal(data, &set); err != nil {
		log.Fatal(err)
	}
	predictions := map[string][]string{}
	correct := 0
	var elapsed time.Duration
	for i, u := range set.Utterances {
		if *limit > 0 && i >= *limit {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		start := time.Now()
		d, err := m.Route(ctx, router.Input{SessionID: u.ID, RequestID: u.ID, Text: u.Text, Language: u.Lang}, router.Session{Language: "ru", Identity: router.Values{}}, router.RouteOptions{})
		elapsed += time.Since(start)
		cancel()
		if err != nil {
			log.Fatalf("%s: %v", u.ID, err)
		}
		ids := []string{}
		for _, s := range d.Scenarios {
			ids = append(ids, s.ScenarioID)
		}
		predictions[u.ID] = ids
		if len(ids) > 0 && len(u.Expected) > 0 && ids[0] == u.Expected[0] {
			correct++
		}
		fmt.Printf("%s %v\n", u.ID, ids)
	}
	if len(predictions) == 0 {
		log.Fatal("No utterances found")
	}
	b, _ := json.MarshalIndent(predictions, "", "  ")
	if err := os.WriteFile(*output, b, 0600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Primary accuracy: %d/%d (%.1f%%); mean router latency: %s\n", correct, len(predictions), 100*float64(correct)/float64(len(predictions)), elapsed/time.Duration(len(predictions)))
}
