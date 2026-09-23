package router

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// SQLite owns durable state in one database file served by one router process.
// All statements share a single connection: writers never race each other for
// the file lock inside the process, and a ":memory:" database stays alive.
// busy_timeout covers other processes such as cmd/migrate. A Backend is
// constructed only inside a tool transaction, from the stored row; it is never
// shared across turns.
type SQLite struct {
	db      *sql.DB
	catalog *Catalog
	schema  int
	closed  context.Context
	close   context.CancelFunc
	mu      sync.Mutex
	gates   map[string]*sessionGate
}

type sessionGate struct {
	ch   chan struct{}
	refs int
}

// OpenSQLite opens (creating if needed) the database at dbPath and applies
// pending migrations, so an opened store is always ready. ":memory:" gives a
// private database that lives until Close.
func OpenSQLite(ctx context.Context, dbPath string, catalog *Catalog) (*SQLite, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("DB_PATH is required")
	}
	if strings.Contains(dbPath, "?") {
		return nil, fmt.Errorf("DB_PATH must not contain '?'")
	}
	if catalog == nil {
		return nil, fmt.Errorf("catalog is required")
	}
	ms, err := migrations()
	if err != nil {
		return nil, err
	}
	// _txlock=immediate takes the write lock at BEGIN, so a transaction that
	// reads and then writes cannot fail halfway with SQLITE_BUSY.
	q := url.Values{"_txlock": {"immediate"}, "_pragma": {"busy_timeout(5000)", "foreign_keys(ON)", "journal_mode(WAL)", "synchronous(NORMAL)"}}
	db, err := sql.Open("sqlite", dbPath+"?"+q.Encode())
	if err != nil {
		return nil, databaseError(err)
	}
	db.SetMaxOpenConns(1)
	closed, cancel := context.WithCancel(context.Background())
	p := &SQLite{db: db, catalog: catalog, schema: ms[len(ms)-1].version, closed: closed, close: cancel, gates: map[string]*sessionGate{}}
	if err = p.connect(ctx); err == nil {
		err = p.Migrate(ctx)
	}
	if err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// connect opens the connection. Switching a new file to WAL needs an exclusive
// lock that SQLite does not wait for via busy_timeout, so concurrent first
// opens (router and cmd/migrate) retry here for up to the same 5 s.
func (p *SQLite) connect(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := p.db.PingContext(ctx)
		var e *sqlite.Error
		if err == nil || !errors.As(err, &e) || e.Code()&0xff != sqlite3.SQLITE_BUSY || time.Now().After(deadline) {
			return databaseError(err)
		}
		select {
		case <-ctx.Done():
			return databaseError(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Close cancels every outstanding lease, so in-flight model calls stop instead
// of writing afterwards, then closes the database. It is idempotent.
func (p *SQLite) Close() {
	p.close()
	if err := p.db.Close(); err != nil {
		slog.Warn("closing SQLite store", "error", err)
	}
}

type migration struct {
	version int
	sql     string
}

// migrations returns the embedded files in name order; names start with a
// zero-padded version number.
func migrations() ([]migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, entry := range entries {
		name := entry.Name()
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", name, err)
		}
		b, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version, string(b)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no migrations embedded")
	}
	return out, nil
}

// Migrate applies pending migrations and seeds the synthetic backend once; a
// later call never overwrites mutated business records. BEGIN IMMEDIATE
// serializes concurrent migrators on the same file.
func (p *SQLite) Migrate(ctx context.Context) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS router_schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')))`); err != nil {
		return databaseError(err)
	}
	for _, m := range ms {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM router_schema_migrations WHERE version=?)`, m.version).Scan(&exists); err != nil {
			return databaseError(err)
		}
		if exists {
			continue
		}
		if _, err = tx.ExecContext(ctx, m.sql); err != nil {
			return databaseError(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO router_schema_migrations(version) VALUES(?)`, m.version); err != nil {
			return databaseError(err)
		}
	}
	seed, err := json.Marshal(p.catalog.Seed)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO mock_backend_state(id, data, sequence, updated_at) VALUES(1, ?, 900000, ?) ON CONFLICT(id) DO NOTHING`, string(seed), stamp(time.Now())); err != nil {
		return databaseError(err)
	}
	return databaseError(tx.Commit())
}

func (p *SQLite) Ready(ctx context.Context) error {
	var ready bool
	err := p.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM router_schema_migrations WHERE version=?) AND EXISTS(SELECT 1 FROM mock_backend_state WHERE id=1)`, p.schema).Scan(&ready)
	if err != nil {
		return databaseError(err)
	}
	if !ready {
		return fmt.Errorf("%w: migrations or seed not applied", ErrDatabase)
	}
	return nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

type sqliteLease struct {
	owner    *SQLite
	id       string
	gate     *sessionGate
	ctx      context.Context
	cancel   context.CancelFunc
	stop     func() bool
	mu       sync.Mutex
	released bool
}

// Lock gives exclusive ownership of a session within this process and waits
// for the current owner until ctx ends. A second process on the same file is
// not excluded here; Save's version check still rejects its stale writes.
func (p *SQLite) Lock(ctx context.Context, id string) (Lease, error) {
	p.mu.Lock()
	g := p.gates[id]
	if g == nil {
		g = &sessionGate{ch: make(chan struct{}, 1)}
		p.gates[id] = g
	}
	g.refs++
	p.mu.Unlock()
	select {
	case g.ch <- struct{}{}:
	case <-ctx.Done():
		p.unref(id, g)
		return nil, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
	initial, err := sessionJSON(initialSession(id))
	if err == nil {
		_, err = p.db.ExecContext(ctx, `INSERT INTO sessions(id, state, updated_at) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING`, id, string(initial), stamp(time.Now()))
	}
	if err != nil {
		<-g.ch
		p.unref(id, g)
		return nil, databaseError(err)
	}
	l := &sqliteLease{owner: p, id: id, gate: g}
	l.ctx, l.cancel = context.WithCancel(ctx)
	l.stop = context.AfterFunc(p.closed, l.cancel)
	return l, nil
}

func (p *SQLite) unref(id string, g *sessionGate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if g.refs--; g.refs == 0 {
		delete(p.gates, id)
	}
}

func (l *sqliteLease) Context() context.Context { return l.ctx }

func (l *sqliteLease) check() error {
	if l.released || l.owner.closed.Err() != nil {
		return fmt.Errorf("%w: session lease lost", ErrDatabase)
	}
	return l.ctx.Err()
}

func (l *sqliteLease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	l.stop()
	l.cancel()
	<-l.gate.ch
	l.owner.unref(l.id, l.gate)
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func initialSession(id string) Session {
	return Session{ID: id, Language: "ru", Identity: Values{}, Stack: []*Frame{}, Queue: []*Frame{}, Turns: []Turn{}}
}

func (l *sqliteLease) Load(ctx context.Context) (Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return Session{}, err
	}
	return loadSession(ctx, l.owner.db, l.id)
}

func (p *SQLite) Get(ctx context.Context, id string) (Session, error) {
	// A read transaction keeps the session row and its history consistent.
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Session{}, databaseError(err)
	}
	defer tx.Rollback()
	s, err := loadSession(ctx, tx, id)
	if err != nil {
		return Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, databaseError(err)
	}
	return s, nil
}

func loadSession(ctx context.Context, q queryer, id string) (Session, error) {
	var data []byte
	var version int64
	var updated string
	err := q.QueryRowContext(ctx, `SELECT state, version, updated_at FROM sessions WHERE id=?`, id).Scan(&data, &version, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, databaseError(err)
	}
	var s Session
	if err = json.Unmarshal(data, &s); err != nil {
		return Session{}, databaseError(err)
	}
	if s.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Session{}, databaseError(err)
	}
	s.Version, s.Turns = version, []Turn{}
	rows, err := q.QueryContext(ctx, `SELECT input, final_output FROM (SELECT id, input, final_output FROM turns WHERE session_id=? AND phase='finished' ORDER BY id DESC LIMIT 10) recent ORDER BY id`, id)
	if err != nil {
		return Session{}, databaseError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var input, output []byte
		if err = rows.Scan(&input, &output); err != nil {
			return Session{}, databaseError(err)
		}
		var turn Turn
		if err = json.Unmarshal(input, &turn.Input); err != nil {
			return Session{}, databaseError(err)
		}
		if err = json.Unmarshal(output, &turn.Output); err != nil {
			return Session{}, databaseError(err)
		}
		s.Turns = append(s.Turns, turn)
	}
	return s, databaseError(rows.Err())
}

func storeInputDigest(in Input) (string, error) {
	if in.ReviewMode == "" {
		in.ReviewMode = "auto"
	}
	return digest(in)
}
func digest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (l *sqliteLease) Begin(ctx context.Context, in Input) (Run, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return Run{}, false, err
	}
	if in.SessionID != l.id {
		return Run{}, false, ErrConflict
	}
	if in.ReviewMode == "" {
		in.ReviewMode = "auto"
	}
	hash, err := storeInputDigest(in)
	if err != nil {
		return Run{}, false, err
	}
	tx, err := l.owner.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, false, databaseError(err)
	}
	defer tx.Rollback()
	var previousHash string
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT input_fingerprint, checkpoint FROM turns WHERE session_id=? AND request_id=?`, l.id, in.RequestID).Scan(&previousHash, &data)
	if err == nil {
		if hash != previousHash {
			return Run{}, false, ErrConflict
		}
		var run Run
		if err = json.Unmarshal(data, &run); err != nil {
			return Run{}, false, databaseError(err)
		}
		return run, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, databaseError(err)
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE session_id=? AND phase <> 'finished')`, l.id).Scan(&busy); err != nil {
		return Run{}, false, databaseError(err)
	}
	if busy {
		return Run{}, false, ErrBusy
	}
	run := Run{Input: in, Phase: "received", Output: Output{SessionID: l.id, RequestID: in.RequestID}}
	input, err := json.Marshal(in)
	if err != nil {
		return Run{}, false, err
	}
	data, err = json.Marshal(run)
	if err != nil {
		return Run{}, false, err
	}
	now := stamp(time.Now())
	if _, err = tx.ExecContext(ctx, `INSERT INTO turns(session_id, request_id, input_fingerprint, input, phase, checkpoint, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`, l.id, in.RequestID, hash, string(input), run.Phase, string(data), now, now); err != nil {
		return Run{}, false, databaseError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO turn_events(session_id, request_id, kind, data, created_at) VALUES(?,?,'received',?,?)`, l.id, in.RequestID, string(input), now); err != nil {
		return Run{}, false, databaseError(err)
	}
	if err = tx.Commit(); err != nil {
		return Run{}, false, databaseError(err)
	}
	return run, false, nil
}

func getRun(ctx context.Context, q queryer, sessionID, requestID string) (Run, error) {
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT checkpoint FROM turns WHERE session_id=? AND request_id=?`, sessionID, requestID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, databaseError(err)
	}
	var run Run
	if err = json.Unmarshal(data, &run); err != nil {
		return Run{}, databaseError(err)
	}
	return run, nil
}
func (p *SQLite) GetTurn(ctx context.Context, sid, rid string) (Run, error) {
	return getRun(ctx, p.db, sid, rid)
}
func (l *sqliteLease) GetRun(ctx context.Context, rid string) (Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return Run{}, err
	}
	return getRun(ctx, l.owner.db, l.id, rid)
}

func sessionJSON(s Session) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	delete(fields, "turns")
	delete(fields, "routing_context")
	return json.Marshal(fields)
}

// oneRow requires a statement to have changed exactly one row; anything else
// means the caller's checkpoint is stale.
func oneRow(res sql.Result, err error) error {
	if err != nil {
		return databaseError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return databaseError(err)
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// saveTx checks the caller's checkpoint version before updating any durable
// state. Only the transaction's caller publishes the incremented version.
func (l *sqliteLease) saveTx(ctx context.Context, tx *sql.Tx, s *Session, r *Run, event *Event) (Session, error) {
	if s.ID != l.id || r.Input.SessionID != l.id {
		return Session{}, ErrConflict
	}
	next := *s
	next.Version++
	next.UpdatedAt = time.Now().UTC()
	now := stamp(next.UpdatedAt)
	snapshot, err := sessionJSON(next)
	if err != nil {
		return Session{}, err
	}
	checkpoint, err := json.Marshal(r)
	if err != nil {
		return Session{}, err
	}
	hash, err := storeInputDigest(r.Input)
	if err != nil {
		return Session{}, err
	}
	var final any
	if r.Phase == "finished" {
		b, marshalErr := json.Marshal(r.Output)
		if marshalErr != nil {
			return Session{}, marshalErr
		}
		final = string(b)
	}
	if err = oneRow(tx.ExecContext(ctx, `UPDATE sessions SET state=?, version=?, updated_at=? WHERE id=? AND version=?`, string(snapshot), next.Version, now, l.id, s.Version)); err != nil {
		return Session{}, err
	}
	if err = oneRow(tx.ExecContext(ctx, `UPDATE turns SET checkpoint=?, phase=?, final_output=?, updated_at=? WHERE session_id=? AND request_id=? AND input_fingerprint=?`, string(checkpoint), r.Phase, final, now, l.id, r.Input.RequestID, hash)); err != nil {
		return Session{}, err
	}
	if r.Review != nil {
		review, marshalErr := json.Marshal(r.Review)
		if marshalErr != nil {
			return Session{}, marshalErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO intent_reviews(session_id, request_id, proposal_id, revision, target, status, proposal, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(session_id, request_id, proposal_id, revision) DO UPDATE SET status=excluded.status, proposal=excluded.proposal, updated_at=excluded.updated_at`, l.id, r.Input.RequestID, r.Review.ProposalID, r.Review.Revision, r.Review.Target, r.Review.Status, string(review), now, now)
		if err != nil {
			return Session{}, databaseError(err)
		}
	}
	if event != nil {
		data, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			return Session{}, marshalErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO turn_events(session_id, request_id, kind, data, created_at) VALUES(?,?,?,?,?)`, l.id, r.Input.RequestID, event.Kind, string(data), now); err != nil {
			return Session{}, databaseError(err)
		}
	}
	return next, nil
}

func (l *sqliteLease) Save(ctx context.Context, s *Session, r *Run, event *Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return err
	}
	tx, err := l.owner.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	next, err := l.saveTx(ctx, tx, s, r, event)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	s.Version, s.UpdatedAt = next.Version, next.UpdatedAt
	return nil
}

func (l *sqliteLease) Tool(ctx context.Context, s *Session, r *Run, operationID, name string, args Values, apply func(Values)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return err
	}
	if s.ID != l.id || r.Input.SessionID != l.id {
		return ErrConflict
	}
	hash, err := digest(Values{"name": name, "arguments": args})
	if err != nil {
		return err
	}
	tx, err := l.owner.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	var result Values
	var previousHash string
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT arguments_fingerprint, result FROM tool_executions WHERE session_id=? AND request_id=? AND operation_id=?`, l.id, r.Input.RequestID, operationID).Scan(&previousHash, &data)
	if err == nil {
		if previousHash != hash {
			return ErrConflict
		}
		if err = json.Unmarshal(data, &result); err != nil {
			return databaseError(err)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		// BEGIN IMMEDIATE already holds the write lock, so the read-modify-write
		// of the business state cannot interleave with another tool call.
		var seq int
		if err = tx.QueryRowContext(ctx, `SELECT data, sequence FROM mock_backend_state WHERE id=1`).Scan(&data, &seq); err != nil {
			return databaseError(err)
		}
		var backendData Values
		if err = json.Unmarshal(data, &backendData); err != nil {
			return databaseError(err)
		}
		backend := &Backend{c: l.owner.catalog, data: backendData, seq: seq, receipts: map[string]Values{}}
		result = backend.Execute(name, args, operationID)
		state, marshalErr := json.Marshal(backend.data)
		if marshalErr != nil {
			return marshalErr
		}
		now := stamp(time.Now())
		if _, err = tx.ExecContext(ctx, `UPDATE mock_backend_state SET data=?, sequence=?, updated_at=? WHERE id=1`, string(state), backend.seq, now); err != nil {
			return databaseError(err)
		}
		data, err = json.Marshal(result)
		if err != nil {
			return err
		}
		arguments, marshalErr := json.Marshal(args)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tool_executions(session_id, request_id, operation_id, name, arguments_fingerprint, arguments, result, created_at) VALUES(?,?,?,?,?,?,?,?)`, l.id, r.Input.RequestID, operationID, name, hash, string(arguments), string(data), now); err != nil {
			return databaseError(err)
		}
	} else {
		return databaseError(err)
	}
	beforeSession, beforeRun := clone(*s), clone(*r)
	committed := false
	defer func() {
		if !committed {
			*s, *r = beforeSession, beforeRun
		}
	}()
	apply(clone(result))
	next, err := l.saveTx(ctx, tx, s, r, &Event{Kind: "tool", Data: Values{"operation_id": operationID, "name": name, "arguments": args, "result": result}})
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	s.Version, s.UpdatedAt = next.Version, next.UpdatedAt
	committed = true
	return nil
}

// databaseError maps unique-key violations to ErrConflict (a concurrent writer
// got there first) and everything else to ErrDatabase.
func databaseError(err error) error {
	if err == nil {
		return nil
	}
	var e *sqlite.Error
	if errors.As(err, &e) && (e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return fmt.Errorf("%w: %w", ErrDatabase, err)
}

var _ Repository = (*SQLite)(nil)
var _ Lease = (*sqliteLease)(nil)
