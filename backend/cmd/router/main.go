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
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "voice_router.db"
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
	// Opening migrates idempotently, so a fresh checkout needs no separate step.
	repo, err := router.OpenSQLite(startup, dbPath, catalog)
	if err != nil {
		log.Fatal("Cannot open SQLite database: ", err)
	}
	defer repo.Close()
	if err := repo.Ready(startup); err != nil {
		log.Fatal("SQLite store is not ready: ", err)
	}
	cancelStartup()
	fastModel := os.Getenv("OPENAI_FAST_MODEL")
	if fastModel == "" {
		// Measured: nano took 2.5-3 s on the small prompt, mini 1.6-2.6 s.
		fastModel = "gpt-4.1-mini"
	}
	openai := router.NewOpenAI(key, model, catalog)
	openai.FastModel = fastModel
	engine := router.NewEngine(catalog, openai, repo)
	engine.Policy.FallbackModel = os.Getenv("OPENAI_FALLBACK_MODEL")
	if engine.Policy.FallbackModel == "" {
		// L1 retries on a different model than L0.
		engine.Policy.FallbackModel = "gpt-4.1-nano"
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
	log.Printf("Layer 2 classifier listening on %s; model=%s; fast=%s; fallback=%s; backend=synthetic; store=sqlite; db=%s", addr, model, fastModel, engine.Policy.FallbackModel, dbPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
