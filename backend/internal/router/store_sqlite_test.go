package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSQLiteStoreInputIdentityAndHistoryProjection(t *testing.T) {
	_, p, _ := engineDatabase(t)
	ctx := context.Background()
	l, err := p.Lock(ctx, "history")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	s, err := l.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		in := Input{SessionID: "history", RequestID: fmt.Sprintf("turn-%d", i), Text: " exact input ", Language: "ru", ReviewMode: "auto"}
		r, resumed, err := l.Begin(ctx, in)
		if err != nil || resumed {
			t.Fatal(resumed, err)
		}
		changed := in
		changed.Text = "exact input"
		if _, _, err = l.Begin(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatal("changed text accepted", err)
		}
		next := in
		next.RequestID = "concurrent"
		if _, _, err = l.Begin(ctx, next); !errors.Is(err, ErrBusy) {
			t.Fatal("second unfinished request accepted", err)
		}
		r.Phase = "finished"
		r.Output.Answer = fmt.Sprintf("answer %d", i)
		r.Output.Status = "completed"
		s.TurnCount++
		s.Turns = append(s.Turns, Turn{Input: in, Output: &r.Output})
		s.RoutingContext = []Values{{"temporary": true}}
		if err = l.Save(ctx, &s, &r, &Event{Kind: "answer", Data: Values{"number": i}}); err != nil {
			t.Fatal(err)
		}
		if replay, resumed, err := l.Begin(ctx, in); err != nil || !resumed || replay.Output.Answer != r.Output.Answer {
			t.Fatal("replay", replay, resumed, err)
		}
	}
	saved, err := p.Get(ctx, "history")
	if err != nil || saved.TurnCount != 12 || len(saved.Turns) != 10 || saved.Turns[0].Input.RequestID != "turn-2" || saved.Turns[9].Output.Answer != "answer 11" {
		t.Fatal(saved, err)
	}
	if saved.Version != s.Version || !saved.UpdatedAt.Equal(s.UpdatedAt) {
		t.Fatal("stored version or timestamp differs from the saved copy", saved.Version, s.Version, saved.UpdatedAt, s.UpdatedAt)
	}
	loaded, err := l.Load(ctx)
	if err != nil || !equalJSON(loaded, saved) {
		t.Fatal("lease and read projections differ", err)
	}
	var historyStored bool
	if err = p.db.QueryRow(`SELECT json_type(state,'$.turns') IS NOT NULL OR json_type(state,'$.routing_context') IS NOT NULL FROM sessions WHERE id='history'`).Scan(&historyStored); err != nil || historyStored {
		t.Fatal("session duplicates conversation history", err)
	}
	var count int
	if err = p.db.QueryRow(`SELECT count(*) FROM turns WHERE session_id='history'`).Scan(&count); err != nil || count != 12 {
		t.Fatal("long-term history missing", count, err)
	}
	if err = p.db.QueryRow(`SELECT count(*) FROM turn_events WHERE session_id='history'`).Scan(&count); err != nil || count != 24 {
		t.Fatal("received and answer events missing", count, err)
	}
	if _, err = p.Get(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = p.GetTurn(ctx, "history", "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	stale := saved
	stale.Version--
	r, _ := p.GetTurn(ctx, "history", "turn-11")
	if err = l.Save(ctx, &stale, &r, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("stale session version accepted", err)
	}
}

func TestSQLiteToolRollbackReplayAndSeedOnce(t *testing.T) {
	c, p, path := engineDatabase(t)
	ctx := context.Background()
	l, err := p.Lock(ctx, "atomic-tool")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	s, err := l.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := l.Begin(ctx, Input{SessionID: s.ID, RequestID: "one", Text: "update", Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	stale := clone(s)
	r.Phase = "executing"
	if err = l.Save(ctx, &s, &r, nil); err != nil {
		t.Fatal(err)
	}
	client := asMap(list(c.Seed["clients"])[0])
	args := Values{"client_id": client["client_id"], "contact_field": "email", "new_value": "persisted@example.com"}
	apply := func(result Values) {
		if errorCode(result) != "" {
			t.Fatalf("tool rejected test arguments: %v", result)
		}
		r.Steps++
	}
	if err = l.Tool(ctx, &stale, &r, "op-1", "update_contact", args, apply); !errors.Is(err, ErrConflict) {
		t.Fatal("stale checkpoint accepted", err)
	}
	if r.Steps != 0 {
		t.Fatal("rolled-back callback leaked", r.Steps)
	}
	var receipts int
	if err = p.db.QueryRow(`SELECT count(*) FROM tool_executions`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("rolled-back receipt remained", receipts, err)
	}
	if email := storedClient(t, p, c, client["client_id"])["email"]; email == "persisted@example.com" {
		t.Fatal("rolled-back mutation remained")
	}
	if err = l.Tool(ctx, &s, &r, "op-1", "update_contact", args, apply); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err = p.db.QueryRow(`SELECT updated_at FROM mock_backend_state`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = l.Tool(ctx, &s, &r, "op-1", "update_contact", args, apply); err != nil {
		t.Fatal("receipt replay failed", err)
	}
	if err = p.db.QueryRow(`SELECT updated_at FROM mock_backend_state`).Scan(&after); err != nil || before != after {
		t.Fatal("replay executed the mutation again", before, after, err)
	}
	changed := clone(args)
	changed["new_value"] = "different@example.com"
	if err = l.Tool(ctx, &s, &r, "op-1", "update_contact", changed, apply); !errors.Is(err, ErrConflict) {
		t.Fatal("operation ID reused with changed arguments", err)
	}
	if err = p.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if found := storedClient(t, p, c, client["client_id"]); found["email"] != "persisted@example.com" {
		t.Fatal("migration overwrote mutation", found)
	}
	if err = p.db.QueryRow(`SELECT count(*) FROM tool_executions`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("receipt duplicated", receipts, err)
	}
	saved, err := p.GetTurn(ctx, s.ID, r.Input.RequestID)
	if err != nil || saved.Steps != 2 {
		t.Fatal("receipt application not checkpointed", saved, err)
	}
	l.Release()
	p.Close()
	reopened, err := OpenSQLite(ctx, path, c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if found := storedClient(t, reopened, c, client["client_id"]); found["email"] != "persisted@example.com" {
		t.Fatal("restart reseeded business state", found)
	}
}

func storedClient(t *testing.T, p *SQLite, c *Catalog, id any) Values {
	t.Helper()
	var raw []byte
	if err := p.db.QueryRow(`SELECT data FROM mock_backend_state WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var data Values
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return (&Backend{c: c, data: data}).find("clients", "client_id", id)
}

// Replaces the PostgreSQL lease-loss test: an in-process lease can only be
// lost when the store closes, and that must stop in-flight work.
func TestSQLiteCloseCancelsLeases(t *testing.T) {
	_, p, _ := engineDatabase(t)
	ctx := context.Background()
	lease, err := p.Lock(ctx, "watchdog")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	p.Close()
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("closing the store did not cancel the lease context")
	}
	if _, err = lease.Load(ctx); !errors.Is(err, ErrDatabase) {
		t.Fatal("lease accepted work after close", err)
	}
	if _, err = p.Lock(ctx, "other"); !errors.Is(err, ErrDatabase) {
		t.Fatal("closed store granted a lease", err)
	}
	if err = p.Ready(ctx); !errors.Is(err, ErrDatabase) {
		t.Fatal("closed store reported ready", err)
	}
}

func TestSQLitePragmasAndConcurrentMigration(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shared.db")
	stores := make([]*SQLite, 4)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = OpenSQLite(context.Background(), path, c)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		defer stores[i].Close()
	}
	p := stores[0]
	var mode string
	var fk, timeout, synchronous int
	if err = p.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatal(mode, err)
	}
	if err = p.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatal(fk, err)
	}
	if err = p.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatal(timeout, err)
	}
	if err = p.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 1 {
		t.Fatal("synchronous is not NORMAL", synchronous, err)
	}
	var versions int
	if err = p.db.QueryRow(`SELECT count(*) FROM router_schema_migrations`).Scan(&versions); err != nil || versions != 1 {
		t.Fatal("migration applied more than once", versions, err)
	}
	if _, err = p.db.Exec(`INSERT INTO turns(session_id, request_id, input_fingerprint, input, phase, checkpoint) VALUES('missing','r','h','{}','received','{}')`); !errors.Is(databaseError(err), ErrDatabase) {
		t.Fatal("foreign key not enforced", err)
	}
}
