// migrate applies the embedded database schema and seeds synthetic records once.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"voice-router/internal/router"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalog, err := router.LoadCatalog()
	if err != nil {
		log.Fatal(err)
	}
	db, err := router.OpenPostgres(ctx, os.Getenv("DATABASE_URL"), catalog)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		log.Fatal(err)
	}
	log.Print("PostgreSQL migrations and seed are ready")
}
