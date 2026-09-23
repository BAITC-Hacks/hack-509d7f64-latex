package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"voice-router/internal/router"
)

type count struct{ rows, dataset int }

// migrate runs the command and parses its view table: view -> collection counts.
func migrate(t *testing.T, args ...string) (string, map[string]count) {
	t.Helper()
	var out bytes.Buffer
	if err := run(context.Background(), args, &out, io.Discard); err != nil {
		t.Fatal(args, err)
	}
	counts := map[string]count{}
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || !strings.HasPrefix(f[0], "mock_") {
			continue
		}
		rows, err1 := strconv.Atoi(f[2])
		dataset, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			t.Fatal("bad count line", line)
		}
		counts[f[0]] = count{rows, dataset}
	}
	for _, view := range []string{"mock_clients", "mock_policies", "mock_claims", "mock_payments"} {
		if c, ok := counts[view]; !ok || c.dataset == 0 {
			t.Fatalf("no %s count in output:\n%s", view, out.String())
		}
	}
	return out.String(), counts
}

func matchesDataset(t *testing.T, counts map[string]count) {
	t.Helper()
	for view, c := range counts {
		if c.rows != c.dataset {
			t.Fatalf("%s has %d rows, dataset %d", view, c.rows, c.dataset)
		}
	}
}

// createPolicy issues one policy through the store, as the router would, in
// a new session.
func createPolicy(t *testing.T, path, session string) {
	t.Helper()
	ctx := context.Background()
	catalog, err := router.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	db, err := router.OpenSQLite(ctx, path, catalog)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lease, err := db.Lock(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	s, err := lease.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := lease.Begin(ctx, router.Input{SessionID: session, RequestID: "buy", Text: "ОГПО", Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	var result router.Values
	if err = lease.Tool(ctx, &s, &r, "op-buy", "create_policy", router.Values{"product_type": "ogpo", "phone": "+77010000001", "price": 30400}, func(v router.Values) { result = v }); err != nil {
		t.Fatal(err)
	}
	if result["policy_number"] != "SQ-OGPO-900001" {
		t.Fatal("create_policy", result)
	}
}

func sessionExists(t *testing.T, path string) bool {
	t.Helper()
	ctx := context.Background()
	catalog, err := router.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	db, err := router.OpenSQLite(ctx, path, catalog)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Get(ctx, "demo")
	if err != nil && !errors.Is(err, router.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func TestMigrateSeedsKeepsMutationsAndResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.db")
	out, counts := migrate(t, "-db", path)
	matchesDataset(t, counts)
	for _, want := range []string{"migration 001", "migration 002  seed_mock_backend", "migration 003  mock_backend_views", "equals dataset: yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}

	createPolicy(t, path, "demo")
	out, counts = migrate(t, "-db", path)
	if c := counts["mock_policies"]; c.rows != c.dataset+1 || !strings.Contains(out, "equals dataset: no") {
		t.Fatalf("re-running migrate lost or hid the created policy:\n%s", out)
	}

	out, counts = migrate(t, "-db", path, "-reset-data")
	matchesDataset(t, counts)
	if !strings.Contains(out, "equals dataset: yes") || !strings.Contains(out, "sequence 900000") {
		t.Fatalf("-reset-data did not restore the dataset:\n%s", out)
	}
	if !sessionExists(t, path) {
		t.Fatal("-reset-data deleted sessions")
	}

	createPolicy(t, path, "demo-2") // the counter restarted: SQ-OGPO-900001 again
	_, counts = migrate(t, "-db", path, "-reset-all")
	matchesDataset(t, counts)
	if sessionExists(t, path) {
		t.Fatal("-reset-all kept sessions")
	}
}

func TestMigrateRejectsBadArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.db")
	if err := run(context.Background(), []string{"-db", path, "extra"}, io.Discard, io.Discard); err == nil {
		t.Fatal("positional argument accepted")
	}
	if err := run(context.Background(), []string{"-reset"}, io.Discard, io.Discard); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if err := run(context.Background(), []string{"-h"}, io.Discard, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatal("-h", err)
	}
}
