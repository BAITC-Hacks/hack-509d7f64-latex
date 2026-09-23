package router

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// mockSequenceStart is the synthetic ID counter of a freshly seeded backend;
// Backend.next increments it before use, so the first created record is 900001.
const mockSequenceStart = 900000

// mockBackendViews pairs the typed views of migration 003 with the dataset
// arrays they unnest, in display order.
var mockBackendViews = []struct{ view, collection string }{
	{"mock_clients", "clients"},
	{"mock_policies", "policies"},
	{"mock_claims", "claims"},
	{"mock_payments", "payments"},
}

// seedJSON is the canonical text of the embedded mock_backend.json (Go's
// json.Marshal of Catalog.Seed: sorted keys, no whitespace) and its SHA-256.
// The tools write state back through the same marshaller, so the stored
// document equals this text exactly until a tool changes business data.
func (c *Catalog) seedJSON() (string, string, error) {
	b, err := json.Marshal(c.Seed)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:]), nil
}

// seedArgs binds the named parameters that migrations may use; see
// migrations/002_seed_mock_backend.sql.
func (p *SQLite) seedArgs(now time.Time) ([]any, error) {
	seed, sum, err := p.catalog.seedJSON()
	if err != nil {
		return nil, err
	}
	return []any{
		sql.Named("mock_backend", seed),
		sql.Named("mock_backend_sha256", sum),
		sql.Named("mock_sequence", mockSequenceStart),
		sql.Named("now", stamp(now)),
	}, nil
}

// reseedSQL is migration 002's insert turned into an overwrite.
const reseedSQL = `INSERT INTO mock_backend_state(id, data, sequence, seed_sha256, seeded_at, updated_at)
VALUES (1, :mock_backend, :mock_sequence, :mock_backend_sha256, :now, :now)
ON CONFLICT(id) DO UPDATE SET data=excluded.data, sequence=excluded.sequence, seed_sha256=excluded.seed_sha256, seeded_at=excluded.seeded_at, updated_at=excluded.updated_at`

// ResetMockBackend replaces the synthetic backend with the embedded dataset
// and restarts the ID counter, so a demo can be repeated with the same
// records and generated numbers. Sessions, turns and tool receipts are kept.
// A running router picks the reset up on its next tool call.
func (p *SQLite) ResetMockBackend(ctx context.Context) error {
	return p.reset(ctx, false)
}

// ResetAll re-seeds the synthetic backend like ResetMockBackend and also
// deletes every session, turn, turn event, intent review and tool receipt.
// Stop the router first: an in-flight turn would fail with a conflict.
func (p *SQLite) ResetAll(ctx context.Context) error {
	return p.reset(ctx, true)
}

func (p *SQLite) reset(ctx context.Context, sessions bool) error {
	args, err := p.seedArgs(time.Now())
	if err != nil {
		return err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	if sessions {
		// Children first: the foreign keys point at turns and sessions.
		for _, table := range []string{"turn_events", "intent_reviews", "tool_executions", "turns", "sessions"} {
			if _, err = tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
				return databaseError(err)
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name IN ('turns','turn_events')`); err != nil {
			return databaseError(err)
		}
	}
	if _, err = tx.ExecContext(ctx, reseedSQL, args...); err != nil {
		return databaseError(err)
	}
	return databaseError(tx.Commit())
}

// MockBackendCount is the size of one collection of the synthetic backend.
type MockBackendCount struct {
	View       string // mock_clients, ...; mock_records for other collections
	Collection string // array in mock_backend.json / mock_backend_state.data
	Rows       int    // rows the view returns now
	Dataset    int    // elements in the embedded dataset
}

// MockBackendStatus describes the stored synthetic backend.
type MockBackendStatus struct {
	// Counts lists the typed views, then any other array (bookings, handoffs,
	// callbacks, ...) as counted by mock_records.
	Counts   []MockBackendCount
	Sequence int
	// SeedSHA256 and SeededAt are recorded by migration 002 or a reset; both
	// are empty for a database seeded by the pre-002 startup code.
	SeedSHA256, SeededAt, UpdatedAt string
	// DatasetSHA256 is the digest of the dataset embedded in this binary.
	DatasetSHA256 string
	// MatchesDataset is true while the stored document equals the embedded
	// dataset exactly, i.e. no tool has changed business data since seeding.
	MatchesDataset bool
}

func (p *SQLite) MockBackendStatus(ctx context.Context) (MockBackendStatus, error) {
	seed, sum, err := p.catalog.seedJSON()
	if err != nil {
		return MockBackendStatus{}, err
	}
	st := MockBackendStatus{DatasetSHA256: sum}
	// One read transaction, so the counts and the document are one snapshot.
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return st, databaseError(err)
	}
	defer tx.Rollback()
	var data string
	var seedSum, seededAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT data, sequence, seed_sha256, seeded_at, updated_at FROM mock_backend_state WHERE id=1`).Scan(&data, &st.Sequence, &seedSum, &seededAt, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("%w: mock backend is not seeded", ErrNotFound)
	}
	if err != nil {
		return st, databaseError(err)
	}
	st.SeedSHA256, st.SeededAt, st.MatchesDataset = seedSum.String, seededAt.String, data == seed
	typed := map[string]bool{}
	for _, v := range mockBackendViews {
		c := MockBackendCount{View: v.view, Collection: v.collection, Dataset: len(list(p.catalog.Seed[v.collection]))}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+v.view).Scan(&c.Rows); err != nil {
			return st, databaseError(err)
		}
		st.Counts, typed[v.collection] = append(st.Counts, c), true
	}
	rows, err := tx.QueryContext(ctx, `SELECT collection, count(*) FROM mock_records GROUP BY collection ORDER BY collection`)
	if err != nil {
		return st, databaseError(err)
	}
	defer rows.Close()
	for rows.Next() {
		c := MockBackendCount{View: "mock_records"}
		if err = rows.Scan(&c.Collection, &c.Rows); err != nil {
			return st, databaseError(err)
		}
		if !typed[c.Collection] {
			c.Dataset = len(list(p.catalog.Seed[c.Collection]))
			st.Counts = append(st.Counts, c)
		}
	}
	if err = rows.Err(); err != nil {
		return st, databaseError(err)
	}
	rows.Close()
	return st, databaseError(tx.Commit())
}

// AppliedMigration is one row of router_schema_migrations.
type AppliedMigration struct {
	Version   int
	Name      string // label of the embedded file; "" if this binary lacks it
	AppliedAt string
}

func (p *SQLite) AppliedMigrations(ctx context.Context) ([]AppliedMigration, error) {
	ms, err := migrations()
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	for _, m := range ms {
		names[m.version] = m.name
	}
	rows, err := p.db.QueryContext(ctx, `SELECT version, applied_at FROM router_schema_migrations ORDER BY version`)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []AppliedMigration
	for rows.Next() {
		var m AppliedMigration
		if err = rows.Scan(&m.Version, &m.AppliedAt); err != nil {
			return nil, databaseError(err)
		}
		m.Name = names[m.Version]
		out = append(out, m)
	}
	return out, databaseError(rows.Err())
}
