package router

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Postgres owns durable state. A Backend is constructed only inside a tool
// transaction, from the locked database row; it is never shared across turns.
type Postgres struct {
	pool          *pgxpool.Pool
	catalog       *Catalog
	lockNamespace string
}

func OpenPostgres(ctx context.Context, databaseURL string, catalog *Catalog) (*Postgres, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if catalog == nil {
		return nil, fmt.Errorf("catalog is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if config.MaxConns < 16 {
		config.MaxConns = 16
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, databaseError(err)
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, databaseError(err)
	}
	var schema string
	if err = pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		pool.Close()
		return nil, databaseError(err)
	}
	return &Postgres{pool: pool, catalog: catalog, lockNamespace: schema}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryKey("voice-router:"+p.lockNamespace+":migrations")); err != nil {
		return databaseError(err)
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS router_schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return databaseError(err)
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM router_schema_migrations WHERE version = 1)`).Scan(&exists); err != nil {
		return databaseError(err)
	}
	if !exists {
		migration, readErr := migrationFiles.ReadFile("migrations/001_durable_router.sql")
		if readErr != nil {
			return readErr
		}
		if _, err = tx.Exec(ctx, string(migration)); err != nil {
			return databaseError(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO router_schema_migrations(version) VALUES(1)`); err != nil {
			return databaseError(err)
		}
	}
	seed, err := json.Marshal(p.catalog.Seed)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO mock_backend_state(id, data, sequence) VALUES(1, $1, 900000) ON CONFLICT(id) DO NOTHING`, seed); err != nil {
		return databaseError(err)
	}
	return databaseError(tx.Commit(ctx))
}

func (p *Postgres) Ready(ctx context.Context) error {
	var ready bool
	err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM router_schema_migrations WHERE version=1) AND EXISTS(SELECT 1 FROM mock_backend_state WHERE id=1)`).Scan(&ready)
	if err != nil {
		return databaseError(err)
	}
	if !ready {
		return fmt.Errorf("%w: migrations or seed not applied", ErrDatabase)
	}
	return nil
}

func advisoryKey(id string) int64 {
	sum := sha256.Sum256([]byte(id))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

type postgresLease struct {
	owner    *Postgres
	conn     *pgxpool.Conn
	id       string
	key      int64
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	released bool
	lost     bool
}

func (p *Postgres) Lock(ctx context.Context, id string) (Lease, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	key := advisoryKey("voice-router:" + p.lockNamespace + ":session:" + id)
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		closePooledConnection(conn)
		return nil, databaseError(err)
	}
	if !locked {
		conn.Release()
		return nil, ErrBusy
	}
	initial, err := sessionJSON(initialSession(id))
	if err == nil {
		_, err = conn.Exec(ctx, `INSERT INTO sessions(id,state) VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, id, initial)
	}
	if err != nil {
		closePooledConnection(conn)
		return nil, databaseError(err)
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &postgresLease{owner: p, conn: conn, id: id, key: key, ctx: lctx, cancel: cancel, done: make(chan struct{})}
	go l.watch()
	return l, nil
}

func (l *postgresLease) Context() context.Context { return l.ctx }

func (l *postgresLease) watch() {
	defer close(l.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			if !l.mu.TryLock() {
				continue
			}
			if l.released || l.lost {
				l.mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
			err := l.conn.Ping(ctx)
			cancel()
			if err != nil {
				l.lost = true
				l.cancel()
			}
			l.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (l *postgresLease) check() error {
	if l.released || l.lost || l.conn.Conn().IsClosed() {
		l.lost = true
		l.cancel()
		return fmt.Errorf("%w: session lease lost", ErrDatabase)
	}
	if err := l.ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (l *postgresLease) Release() {
	l.cancel()
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var unlocked bool
	err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, l.key).Scan(&unlocked)
	if err != nil || !unlocked {
		slog.Warn("discarding PostgreSQL lease connection", "unlock_error", err)
		closePooledConnection(l.conn)
		return
	}
	l.conn.Release()
}

func closePooledConnection(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Hijack().Close(ctx)
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func initialSession(id string) Session {
	return Session{ID: id, Language: "ru", Identity: Values{}, Stack: []*Frame{}, Queue: []*Frame{}, Turns: []Turn{}}
}

func (l *postgresLease) Load(ctx context.Context) (Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return Session{}, err
	}
	data, err := sessionJSON(initialSession(l.id))
	if err != nil {
		return Session{}, err
	}
	if _, err = l.conn.Exec(ctx, `INSERT INTO sessions(id, state) VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, l.id, data); err != nil {
		return Session{}, databaseError(err)
	}
	return loadSession(ctx, l.conn, l.id)
}

func (p *Postgres) Get(ctx context.Context, id string) (Session, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Session{}, databaseError(err)
	}
	defer tx.Rollback(context.Background())
	s, err := loadSession(ctx, tx, id)
	if err != nil {
		return Session{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Session{}, databaseError(err)
	}
	return s, nil
}

func loadSession(ctx context.Context, q queryer, id string) (Session, error) {
	var data []byte
	var version int64
	var updated time.Time
	err := q.QueryRow(ctx, `SELECT state, version, updated_at FROM sessions WHERE id=$1`, id).Scan(&data, &version, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, databaseError(err)
	}
	var s Session
	if err = json.Unmarshal(data, &s); err != nil {
		return Session{}, databaseError(err)
	}
	s.Version, s.UpdatedAt, s.Turns = version, updated, []Turn{}
	rows, err := q.Query(ctx, `SELECT input, final_output FROM (SELECT id,input,final_output FROM turns WHERE session_id=$1 AND phase='finished' ORDER BY id DESC LIMIT 10) recent ORDER BY id`, id)
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

func (l *postgresLease) Begin(ctx context.Context, in Input) (Run, bool, error) {
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
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return Run{}, false, databaseError(err)
	}
	defer tx.Rollback(context.Background())
	var previousHash string
	var data []byte
	err = tx.QueryRow(ctx, `SELECT input_fingerprint, checkpoint FROM turns WHERE session_id=$1 AND request_id=$2`, l.id, in.RequestID).Scan(&previousHash, &data)
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
	if !errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, databaseError(err)
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE session_id=$1 AND phase <> 'finished')`, l.id).Scan(&busy); err != nil {
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
	if _, err = tx.Exec(ctx, `INSERT INTO turns(session_id, request_id, input_fingerprint, input, phase, checkpoint) VALUES($1,$2,$3,$4,$5,$6)`, l.id, in.RequestID, hash, input, run.Phase, data); err != nil {
		return Run{}, false, databaseError(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO turn_events(session_id, request_id, kind, data) VALUES($1,$2,'received',$3)`, l.id, in.RequestID, input); err != nil {
		return Run{}, false, databaseError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Run{}, false, databaseError(err)
	}
	return run, false, nil
}

func getRun(ctx context.Context, q queryer, sessionID, requestID string) (Run, error) {
	var data []byte
	err := q.QueryRow(ctx, `SELECT checkpoint FROM turns WHERE session_id=$1 AND request_id=$2`, sessionID, requestID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
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
func (p *Postgres) GetTurn(ctx context.Context, sid, rid string) (Run, error) {
	return getRun(ctx, p.pool, sid, rid)
}
func (l *postgresLease) GetRun(ctx context.Context, rid string) (Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return Run{}, err
	}
	return getRun(ctx, l.conn, l.id, rid)
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

// saveTx checks the caller's checkpoint version before updating any durable
// state. Only the transaction's caller publishes the incremented version.
func (l *postgresLease) saveTx(ctx context.Context, tx pgx.Tx, s *Session, r *Run, event *Event) (Session, error) {
	if s.ID != l.id || r.Input.SessionID != l.id {
		return Session{}, ErrConflict
	}
	next := *s
	next.Version++
	next.UpdatedAt = time.Now().UTC()
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
		final = b
	}
	tag, err := tx.Exec(ctx, `UPDATE sessions SET state=$2, version=$3, updated_at=$4 WHERE id=$1 AND version=$5`, l.id, snapshot, next.Version, next.UpdatedAt, s.Version)
	if err != nil {
		return Session{}, databaseError(err)
	}
	if tag.RowsAffected() != 1 {
		return Session{}, ErrConflict
	}
	tag, err = tx.Exec(ctx, `UPDATE turns SET checkpoint=$3, phase=$4, final_output=$5, updated_at=now() WHERE session_id=$1 AND request_id=$2 AND input_fingerprint=$6`, l.id, r.Input.RequestID, checkpoint, r.Phase, final, hash)
	if err != nil {
		return Session{}, databaseError(err)
	}
	if tag.RowsAffected() != 1 {
		return Session{}, ErrConflict
	}
	if r.Review != nil {
		review, marshalErr := json.Marshal(r.Review)
		if marshalErr != nil {
			return Session{}, marshalErr
		}
		_, err = tx.Exec(ctx, `INSERT INTO intent_reviews(session_id,request_id,proposal_id,revision,target,status,proposal) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(session_id,request_id,proposal_id,revision) DO UPDATE SET status=excluded.status,proposal=excluded.proposal,updated_at=now()`, l.id, r.Input.RequestID, r.Review.ProposalID, r.Review.Revision, r.Review.Target, r.Review.Status, review)
		if err != nil {
			return Session{}, databaseError(err)
		}
	}
	if event != nil {
		data, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			return Session{}, marshalErr
		}
		if _, err = tx.Exec(ctx, `INSERT INTO turn_events(session_id,request_id,kind,data) VALUES($1,$2,$3,$4)`, l.id, r.Input.RequestID, event.Kind, data); err != nil {
			return Session{}, databaseError(err)
		}
	}
	return next, nil
}

func (l *postgresLease) Save(ctx context.Context, s *Session, r *Run, event *Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return err
	}
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(context.Background())
	next, err := l.saveTx(ctx, tx, s, r, event)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return databaseError(err)
	}
	s.Version, s.UpdatedAt = next.Version, next.UpdatedAt
	return nil
}

func (l *postgresLease) Tool(ctx context.Context, s *Session, r *Run, operationID, name string, args Values, apply func(Values)) error {
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
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(context.Background())
	var result Values
	var previousHash string
	var data []byte
	err = tx.QueryRow(ctx, `SELECT arguments_fingerprint,result FROM tool_executions WHERE session_id=$1 AND request_id=$2 AND operation_id=$3`, l.id, r.Input.RequestID, operationID).Scan(&previousHash, &data)
	if err == nil {
		if previousHash != hash {
			return ErrConflict
		}
		if err = json.Unmarshal(data, &result); err != nil {
			return databaseError(err)
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		var seq int
		if err = tx.QueryRow(ctx, `SELECT data,sequence FROM mock_backend_state WHERE id=1 FOR UPDATE`).Scan(&data, &seq); err != nil {
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
		if _, err = tx.Exec(ctx, `UPDATE mock_backend_state SET data=$1,sequence=$2,updated_at=now() WHERE id=1`, state, backend.seq); err != nil {
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
		if _, err = tx.Exec(ctx, `INSERT INTO tool_executions(session_id,request_id,operation_id,name,arguments_fingerprint,arguments,result) VALUES($1,$2,$3,$4,$5,$6,$7)`, l.id, r.Input.RequestID, operationID, name, hash, arguments, data); err != nil {
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
	if err = tx.Commit(ctx); err != nil {
		return databaseError(err)
	}
	s.Version, s.UpdatedAt = next.Version, next.UpdatedAt
	committed = true
	return nil
}

func databaseError(err error) error {
	if err == nil {
		return nil
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return fmt.Errorf("%w: %w", ErrDatabase, err)
}

var _ Repository = (*Postgres)(nil)
var _ Lease = (*postgresLease)(nil)
