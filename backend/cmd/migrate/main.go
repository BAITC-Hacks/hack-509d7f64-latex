// migrate prepares the router's SQLite database and reports what it holds. It
// applies the embedded migrations: 001 schema, 002 synthetic backend seeded
// from voice_router_dataset/mock_backend.json, 003 read-only mock_* views. The
// router applies the same migrations at startup; this command prepares a file
// ahead of it and resets demo data.
//
//	go run ./cmd/migrate              # apply pending migrations; idempotent, keeps all data
//	go run ./cmd/migrate -reset-data  # also re-seed the mock backend; sessions and turns stay
//	go run ./cmd/migrate -reset-all   # also delete sessions, turns, events, reviews, receipts
//
// The file is -db, else $DB_PATH, else voice_router.db. Afterwards it prints
// the applied migrations and the row count of each mock_* view next to the
// dataset's count.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"text/tabwriter"
	"time"

	"voice-router/internal/router"
)

func main() {
	if err := router.LoadDotEnv(".env"); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		cancel()
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbDefault := os.Getenv("DB_PATH")
	if dbDefault == "" {
		dbDefault = "voice_router.db"
	}
	dbPath := flags.String("db", dbDefault, "SQLite database file (default $DB_PATH, else voice_router.db)")
	resetData := flags.Bool("reset-data", false, "re-seed the mock backend (clients, policies, claims, payments, ...) from the embedded dataset and restart generated IDs; sessions and turns stay")
	resetAll := flags.Bool("reset-all", false, "like -reset-data, and also delete every session, turn, turn event, intent review and tool receipt (stop the router first)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}
	catalog, err := router.LoadCatalog()
	if err != nil {
		return err
	}
	// Opening applies pending migrations; the seed step only inserts into an
	// empty mock_backend_state, so this never overwrites tool mutations.
	db, err := router.OpenSQLite(ctx, *dbPath, catalog)
	if err != nil {
		return err
	}
	defer db.Close()
	action := "migrated: pending migrations applied, existing data kept"
	switch {
	case *resetAll:
		err, action = db.ResetAll(ctx), "reset all: sessions and turns deleted, mock backend re-seeded from the dataset"
	case *resetData:
		err, action = db.ResetMockBackend(ctx), "reset data: mock backend re-seeded from the dataset; sessions and turns kept"
	}
	if err != nil {
		return err
	}
	if err = db.Ready(ctx); err != nil {
		return err
	}
	return report(ctx, db, *dbPath, action, stdout)
}

func report(ctx context.Context, db *router.SQLite, dbPath, action string, out io.Writer) error {
	applied, err := db.AppliedMigrations(ctx)
	if err != nil {
		return err
	}
	st, err := db.MockBackendStatus(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "database\t%s\n", dbPath)
	fmt.Fprintf(w, "action\t%s\n", action)
	for _, m := range applied {
		fmt.Fprintf(w, "migration %03d\t%s\tapplied %s\n", m.Version, m.Name, m.AppliedAt)
	}
	seeded := "seeded " + st.SeededAt + ", sha256 " + st.SeedSHA256
	switch {
	case st.SeedSHA256 == "":
		seeded = "seeded before migration 002 (no digest recorded); -reset-data re-seeds"
	case st.SeedSHA256 != st.DatasetSHA256:
		seeded += " (differs from the embedded dataset " + st.DatasetSHA256 + "; -reset-data re-seeds)"
	}
	matches := "no, changed by tools since seeding"
	if st.MatchesDataset {
		matches = "yes"
	}
	fmt.Fprintf(w, "mock backend\t%s\n", seeded)
	fmt.Fprintf(w, "\tsequence %d, updated %s, equals dataset: %s\n", st.Sequence, st.UpdatedAt, matches)
	if err = w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "view\tcollection\trows\tdataset")
	for _, c := range st.Counts {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", c.View, c.Collection, c.Rows, c.Dataset)
	}
	return w.Flush()
}
