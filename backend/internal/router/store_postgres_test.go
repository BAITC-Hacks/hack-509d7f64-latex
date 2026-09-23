package router

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPostgresStoreInputIdentityAndHistoryProjection(t *testing.T) {
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
	if err != nil || saved.TurnCount != 12 || len(saved.Turns) != 10 || saved.Turns[0].Input.RequestID != "turn-2" {
		t.Fatal(saved, err)
	}
	var historyStored bool
	if err = p.pool.QueryRow(ctx, `SELECT state ? 'turns' OR state ? 'routing_context' FROM sessions WHERE id='history'`).Scan(&historyStored); err != nil || historyStored {
		t.Fatal("session duplicates conversation history", err)
	}
	var count int
	if err = p.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id='history'`).Scan(&count); err != nil || count != 12 {
		t.Fatal("long-term history missing", count, err)
	}
}

func TestPostgresToolRollbackReplayAndSeedOnce(t *testing.T) {
	c, p, _ := engineDatabase(t)
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
	if err = p.pool.QueryRow(ctx, `SELECT count(*) FROM tool_executions`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("rolled-back receipt remained", receipts, err)
	}
	if err = l.Tool(ctx, &s, &r, "op-1", "update_contact", args, apply); err != nil {
		t.Fatal(err)
	}
	if err = l.Tool(ctx, &s, &r, "op-1", "update_contact", args, apply); err != nil {
		t.Fatal("receipt replay failed", err)
	}
	changed := clone(args)
	changed["new_value"] = "different@example.com"
	if err = l.Tool(ctx, &s, &r, "op-1", "update_contact", changed, apply); !errors.Is(err, ErrConflict) {
		t.Fatal("operation ID reused with changed arguments", err)
	}
	if err = p.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var data Values
	if err = p.pool.QueryRow(ctx, `SELECT data FROM mock_backend_state WHERE id=1`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	backend := &Backend{c: c, data: data}
	if found := backend.find("clients", "client_id", client["client_id"]); found["email"] != "persisted@example.com" {
		t.Fatal("migration overwrote mutation", found)
	}
	if err = p.pool.QueryRow(ctx, `SELECT count(*) FROM tool_executions`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("receipt duplicated", receipts, err)
	}
	saved, err := p.GetTurn(ctx, s.ID, r.Input.RequestID)
	if err != nil || saved.Steps != 2 {
		t.Fatal("receipt application not checkpointed", saved, err)
	}
}

func TestPostgresLeaseLossCancelsProcessing(t *testing.T) {
	_, p, _ := engineDatabase(t)
	ctx := context.Background()
	lease, err := p.Lock(ctx, "watchdog")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	l := lease.(*postgresLease)
	var pid uint32
	l.mu.Lock()
	pid = l.conn.Conn().PgConn().PID()
	l.mu.Unlock()
	var terminated bool
	if err = p.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatal(terminated, err)
	}
	select {
	case <-lease.Context().Done():
	case <-time.After(6 * time.Second):
		t.Fatal("lost session lock did not cancel model context")
	}
	if _, err = lease.Load(ctx); !errors.Is(err, ErrDatabase) {
		t.Fatal("lost lease accepted work", err)
	}
	replacement, err := p.Lock(ctx, "watchdog")
	if err != nil {
		t.Fatal("replacement lease unavailable", err)
	}
	replacement.Release()
}
