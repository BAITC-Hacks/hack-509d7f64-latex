package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"
	"voice-router/internal/router"
)

func main() {
	if err := router.LoadDotEnv(".env"); err != nil {
		log.Fatal(err)
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		log.Fatal("Set OPENAI_API_KEY before starting the classifier")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("Set DATABASE_URL before starting the classifier")
	}
	apiToken, operatorToken := os.Getenv("API_TOKEN"), os.Getenv("OPERATOR_API_TOKEN")
	if operatorToken != "" && operatorToken == apiToken {
		log.Fatal("OPERATOR_API_TOKEN must differ from API_TOKEN")
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4.1-mini"
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	catalog, err := router.LoadCatalog()
	if err != nil {
		log.Fatal(err)
	}
	startup, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()
	repo, err := router.OpenPostgres(startup, databaseURL, catalog)
	if err != nil {
		log.Fatal("Cannot connect to PostgreSQL: ", err)
	}
	defer repo.Close()
	if err := repo.Ready(startup); err != nil {
		log.Fatal("PostgreSQL is not ready; run go run ./cmd/migrate: ", err)
	}
	cancelStartup()
	fastModel := os.Getenv("OPENAI_FAST_MODEL")
	if fastModel == "" {
		fastModel = "gpt-4.1-nano"
	}
	openai := router.NewOpenAI(key, model, catalog)
	openai.FastModel = fastModel
	engine := router.NewEngine(catalog, openai, repo)
	engine.Policy.FallbackModel = os.Getenv("OPENAI_FALLBACK_MODEL")
	if engine.Policy.FallbackModel == "" {
		engine.Policy.FallbackModel = fastModel
	}
	server := &http.Server{Addr: addr, Handler: router.Handler(engine, apiToken, operatorToken), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 70 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Layer 2 classifier listening on %s; model=%s; fast=%s; fallback=%s; backend=synthetic; store=postgresql", addr, model, fastModel, engine.Policy.FallbackModel)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
