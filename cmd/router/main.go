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
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		log.Fatal("Set OPENAI_API_KEY before starting the classifier")
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
	engine := router.NewEngine(catalog, router.NewOpenAI(key, model, catalog))
	server := &http.Server{Addr: addr, Handler: router.Handler(engine, os.Getenv("API_TOKEN")), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 70 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Layer 2 classifier listening on %s; model=%s; backend=synthetic; store=in-memory", addr, model)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
