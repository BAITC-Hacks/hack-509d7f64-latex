package router

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func engineDatabase(t *testing.T) (*Catalog, *Postgres, string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("engine_test_%x", suffix)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(ctx)
	})
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenPostgres(ctx, parsed.String(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return c, p, parsed.String()
}
func TestPostgresReviewAndBusinessStateSurviveRestart(t *testing.T) {
	c, p, dsn := engineDatabase(t)
	m := &fakeModel{decisions: []Decision{decision("SC29", Values{"phone": "+77010000003", "contact_field": "email", "new_value": "durable@mail.example"})}}
	e := NewEngine(c, m, p)
	in := input("initial", "Изменить почту")
	in.ReviewMode = "operator"
	first, err := e.Process(context.Background(), in)
	if err != nil || first.Status != "awaiting_intent_confirmation" {
		t.Fatal(first, err)
	}
	p.Close()
	restarted, err := OpenPostgres(context.Background(), dsn, c)
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
	if err != nil || !equalJSON(final, replay) || m2.calls != 1 {
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
func TestPostgresResumeAfterCommittedMutation(t *testing.T) {
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
func TestPostgresInputCheckpointResumes(t *testing.T) {
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
func TestPostgresLockAcrossInstances(t *testing.T) {
	c, p, dsn := engineDatabase(t)
	other, err := OpenPostgres(context.Background(), dsn, c)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	l, err := p.Lock(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if second, err := other.Lock(ctx, "shared"); err == nil {
		second.Release()
		t.Fatal("two processes own same session")
	}
	unrelated, err := other.Lock(ctx, "unrelated")
	if err != nil {
		t.Fatal("unrelated session blocked", err)
	}
	unrelated.Release()
}

type stopAfterProposalModel struct{ fakeModel }

func (m *stopAfterProposalModel) Route(ctx context.Context, in Input, s Session) (Decision, error) {
	d, err := m.fakeModel.Route(ctx, in, s)
	if err != nil {
		return Decision{}, err
	}
	if err := recordRoute(ctx, "model_proposal", Values{"decision": d}); err != nil {
		return Decision{}, err
	}
	// Simulate losing the worker after the provider result was committed.
	return Decision{}, ErrDatabase
}

func TestPostgresCompletedProposalResumesWithoutModelCall(t *testing.T) {
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
