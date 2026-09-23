package router

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// engineDatabase opens a migrated store in a fresh file; the returned path
// reopens the same database to simulate a restart.
func engineDatabase(t *testing.T) (*Catalog, *SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.db")
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenSQLite(context.Background(), path, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return c, p, path
}
func TestSQLiteReviewAndBusinessStateSurviveRestart(t *testing.T) {
	c, p, path := engineDatabase(t)
	m := &fakeModel{decisions: []Decision{decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "durable@mail.example"})}}
	e := NewEngine(c, m, p)
	in := input("initial", "Изменить почту")
	in.ReviewMode = "operator"
	first, err := e.Process(context.Background(), in)
	if err != nil || first.Status != "awaiting_intent_confirmation" {
		t.Fatal(first, err)
	}
	p.Close()
	restarted, err := OpenSQLite(context.Background(), path, c)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	m2 := &fakeModel{decisions: []Decision{decision("SC29", Values{})}}
	e2 := NewEngine(c, m2, restarted)
	confirm, err := e2.Review(context.Background(), in.SessionID, feedback(first, "review", "approved"), "operator")
	if err != nil || confirm.Status != "awaiting_confirmation" || m2.calls != 0 {
		t.Fatal(confirm, err, m2.calls)
	}
	final, err := e2.Process(context.Background(), input("consent", "Да"))
	if err != nil || !actionExecuted(final, "update_contact") {
		t.Fatal(final, err)
	}
	replay, err := e2.Process(context.Background(), input("consent", "Да"))
	if err != nil || !equalJSON(final, replay) || m2.calls != 0 {
		t.Fatal("completed replay differs", err)
	}
	// A new call observes the database mutation, not the fixture seed.
	verify := NewEngine(c, &fakeModel{decisions: []Decision{decision("SC25", Values{"phone": "+77010000003"})}}, restarted)
	v := input("verify", "Проверьте полис")
	v.SessionID = "second-call"
	out, err := verify.Process(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range out.Trace.Actions {
		if a.Name == "find_client" && a.Result["email"] == "durable@mail.example" {
			found = true
		}
	}
	if !found {
		t.Fatal("mock mutation lost across sessions")
	}
}

type failAfterToolRepo struct {
	Repository
	name   string
	failed bool
}
type failAfterToolLease struct {
	Lease
	owner *failAfterToolRepo
}

func (r *failAfterToolRepo) Lock(ctx context.Context, id string) (Lease, error) {
	l, err := r.Repository.Lock(ctx, id)
	if err != nil {
		return nil, err
	}
	return &failAfterToolLease{Lease: l, owner: r}, nil
}
func (l *failAfterToolLease) Tool(ctx context.Context, s *Session, r *Run, op, name string, args Values, apply func(Values)) error {
	err := l.Lease.Tool(ctx, s, r, op, name, args, apply)
	if err == nil && name == l.owner.name && !l.owner.failed {
		l.owner.failed = true
		return ErrDatabase
	}
	return err
}
func TestSQLiteResumeAfterCommittedMutation(t *testing.T) {
	c, p, _ := engineDatabase(t)
	faults := &failAfterToolRepo{Repository: p, name: "update_contact"}
	m := &fakeModel{decisions: []Decision{decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "once@mail.example"}), decision("SC29", Values{})}}
	e := NewEngine(c, m, faults)
	if out, err := e.Process(context.Background(), input("one", "Сменить почту")); err != nil || out.Status != "awaiting_confirmation" {
		t.Fatal(out, err)
	}
	if _, err := e.Process(context.Background(), input("two", "Да")); !errors.Is(err, ErrDatabase) {
		t.Fatal("fault not injected", err)
	}
	saved, err := p.GetTurn(context.Background(), "call-1", "two")
	if err != nil || saved.Phase != "executing" || saved.FrameReady != true {
		t.Fatal(saved, err)
	}
	model := &fakeModel{routeErr: errors.New("routing must not repeat")}
	restarted := NewEngine(c, model, p)
	out, err := restarted.Process(context.Background(), input("two", "Да"))
	if err != nil || out.Status != "completed" || model.calls != 0 {
		t.Fatal(out, err)
	}
	count := 0
	for _, a := range out.Trace.Actions {
		if a.Name == "update_contact" && a.Mode == "execute" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("mutation repeated", count)
	}
}
func TestSQLiteInputCheckpointResumes(t *testing.T) {
	c, p, _ := engineDatabase(t)
	ctx := context.Background()
	in := input("one", "Офис в Алматы")
	in.ReviewMode = "auto"
	lease, err := p.Lock(ctx, in.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lease.Begin(ctx, in); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	e := NewEngine(c, &fakeModel{decisions: []Decision{decision("SC33", Values{"city": "Almaty"})}}, p)
	out, err := e.Process(ctx, in)
	if err != nil || out.Status != "completed" || out.Trace.Turn != 1 {
		t.Fatal(out, err)
	}
}

// SQLite serves one process, so ownership is an in-process gate: a second
// owner waits for Release or gives up with ErrBusy when its context ends.
func TestSQLiteLockIsExclusivePerSession(t *testing.T) {
	_, p, _ := engineDatabase(t)
	l, err := p.Lock(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if second, err := p.Lock(ctx, "shared"); err == nil {
		second.Release()
		t.Fatal("two owners of the same session")
	} else if !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	unrelated, err := p.Lock(context.Background(), "unrelated")
	if err != nil {
		t.Fatal("unrelated session blocked", err)
	}
	unrelated.Release()
	type result struct {
		lease Lease
		err   error
	}
	waiter := make(chan result, 1)
	go func() {
		next, err := p.Lock(context.Background(), "shared")
		waiter <- result{next, err}
	}()
	select {
	case <-waiter:
		t.Fatal("waiter acquired a held session")
	case <-time.After(50 * time.Millisecond):
	}
	l.Release()
	select {
	case got := <-waiter:
		if got.err != nil {
			t.Fatal(got.err)
		}
		got.lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("release did not wake the waiter")
	}
	if l.Context().Err() == nil {
		t.Fatal("released lease context still live")
	}
	if _, err := l.Load(context.Background()); !errors.Is(err, ErrDatabase) {
		t.Fatal("released lease accepted work", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.gates) != 0 {
		t.Fatal("session gates leaked", len(p.gates))
	}
}

type stopAfterProposalModel struct{ fakeModel }

func (m *stopAfterProposalModel) Route(ctx context.Context, in Input, s Session, opts RouteOptions) (Decision, error) {
	d, err := m.fakeModel.Route(ctx, in, s, opts)
	if err != nil {
		return Decision{}, err
	}
	if err := recordRoute(ctx, "model_proposal", Values{"decision": d}); err != nil {
		return Decision{}, err
	}
	// Simulate losing the worker after the provider result was committed.
	return Decision{}, ErrDatabase
}

func TestSQLiteCompletedProposalResumesWithoutModelCall(t *testing.T) {
	c, p, _ := engineDatabase(t)
	m := &stopAfterProposalModel{fakeModel: fakeModel{decisions: []Decision{decision("SC33", Values{"city": "Almaty"})}}}
	in := input("proposal", "Адрес офиса")
	e := NewEngine(c, m, p)
	if _, err := e.Process(context.Background(), in); !errors.Is(err, ErrDatabase) {
		t.Fatal("failed checkpoint incorrectly returned success", err)
	}
	saved, err := p.GetTurn(context.Background(), in.SessionID, in.RequestID)
	if err != nil || saved.Phase != "validating" || saved.Attempts != 1 || saved.InFlightAt != nil {
		t.Fatal("completed provider result not resumable", saved, err)
	}
	m2 := &fakeModel{routeErr: errors.New("must reuse saved proposal")}
	restarted := NewEngine(c, m2, p)
	out, err := restarted.Process(context.Background(), in)
	if err != nil || out.Status != "completed" || m2.calls != 0 || !actionExecuted(out, "get_offices") {
		t.Fatal("resume repeated or lost the model proposal", out, err, m2.calls)
	}
}

func TestSQLiteConcurrentDuplicateRunsOnce(t *testing.T) {
	c, p, _ := engineDatabase(t)
	var ds []Decision
	for i := 0; i < 12; i++ {
		ds = append(ds, decision("SC33", Values{"city": "Almaty"}))
	}
	m := &fakeModel{decisions: ds}
	e := NewEngine(c, m, p)
	var wg sync.WaitGroup
	outs, errs := make([]Output, 12), make([]error, 12)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = e.Process(context.Background(), input("same", "office"))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		if !equalJSON(outs[i], outs[0]) {
			t.Fatal("duplicate returned a different answer")
		}
	}
	if m.calls != 1 {
		t.Fatal("concurrent duplicate processed more than once", m.calls)
	}
}

func TestSQLiteSeparateSessionsAreIsolated(t *testing.T) {
	c, p, _ := engineDatabase(t)
	var ds []Decision
	for i := 0; i < 10; i++ {
		ds = append(ds, decision("SC33", Values{"city": "Almaty"}))
	}
	m := &fakeModel{decisions: ds}
	e := NewEngine(c, m, p)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := input("one", "office")
			in.SessionID = fmt.Sprintf("session-%d", i)
			if _, err := e.Process(context.Background(), in); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m.calls != 10 {
		t.Fatal(m.calls)
	}
	for i := 0; i < 10; i++ {
		s, err := p.Get(context.Background(), fmt.Sprintf("session-%d", i))
		if err != nil || len(s.Turns) != 1 || s.Turns[0].Output.Status != "completed" {
			t.Fatal(i, s.Turns, err)
		}
	}
}

func TestSQLiteMemoryDatabase(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenSQLite(context.Background(), ":memory:", c)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	e := NewEngine(c, &fakeModel{decisions: []Decision{decision("SC33", Values{"city": "Almaty"})}}, p)
	out, err := e.Process(context.Background(), input("one", "office"))
	if err != nil || out.Status != "completed" {
		t.Fatal(out, err)
	}
	s, err := p.Get(context.Background(), "call-1")
	if err != nil || len(s.Turns) != 1 || p.Ready(context.Background()) != nil {
		t.Fatal("in-memory database lost state", s, err)
	}
}
