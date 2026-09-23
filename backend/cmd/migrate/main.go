// migrate applies the embedded database schema and seeds synthetic records
// once. The router does the same at startup; this prepares a file ahead of it.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"voice-router/internal/router"
)

func main() {
	if err := router.LoadDotEnv(".env"); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalog, err := router.LoadCatalog()
	if err != nil {
		log.Fatal(err)
	}
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "voice_router.db"
	}
	db, err := router.OpenSQLite(ctx, dbPath, catalog)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err = db.Ready(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("SQLite migrations and seed are ready in %s", dbPath)
}
