package router

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"voice-router/voice_router_dataset"
)

// rawDataset reads the embedded mock_backend.json itself rather than through
// the Catalog, so the expectations do not come from the code under test.
func rawDataset(t *testing.T) map[string][]Values {
	t.Helper()
	b, err := dataset.Files.ReadFile("mock_backend.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string][]Values{}
	for name, v := range doc {
		var rows []Values
		if json.Unmarshal(v, &rows) == nil {
			out[name] = rows
		}
	}
	for _, name := range []string{"clients", "policies", "claims", "payments"} {
		if len(out[name]) == 0 {
			t.Fatalf("dataset has no %s", name)
		}
	}
	return out
}

func viewRows(t *testing.T, p *SQLite, query string, args ...any) int {
	t.Helper()
	var n int
	if err := p.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(query, err)
	}
	return n
}

func appliedVersions(t *testing.T, p *SQLite) []int {
	t.Helper()
	applied, err := p.AppliedMigrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, m := range applied {
		if m.Name == "" {
			t.Fatal("applied migration has no embedded file", m)
		}
		out = append(out, m.Version)
	}
	return out
}

// toolCall runs one catalog action through Lease.Tool, the transaction the
// engine uses, in a session of its own, and returns the action's result.
func toolCall(t *testing.T, p *SQLite, requestID, name string, args Values) Values {
	t.Helper()
	ctx := context.Background()
	l, err := p.Lock(ctx, "tool-"+requestID)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	s, err := l.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := l.Begin(ctx, Input{SessionID: s.ID, RequestID: requestID, Text: name, Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	var result Values
	if err = l.Tool(ctx, &s, &r, "op-"+requestID, name, args, func(v Values) { result = v }); err != nil {
		t.Fatal(name, err)
	}
	return result
}

func TestSeedMigrationFreshDatabaseMatchesDataset(t *testing.T) {
	c, p, _ := engineDatabase(t)
	ds := rawDataset(t)
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	want := []int{}
	for _, m := range ms {
		want = append(want, m.version)
	}
	if got := appliedVersions(t, p); !slices.Equal(got, want) || !slices.Contains(got, 2) || !slices.Contains(got, 3) {
		t.Fatal("seed and view migrations not recorded", got, want)
	}
	for _, v := range mockBackendViews {
		if got, want := viewRows(t, p, `SELECT count(*) FROM `+v.view), len(ds[v.collection]); got != want {
			t.Fatalf("%s has %d rows, dataset %s has %d", v.view, got, v.collection, want)
		}
		if got := viewRows(t, p, `SELECT count(*) FROM mock_records WHERE collection=?`, v.collection); got != len(ds[v.collection]) {
			t.Fatalf("mock_records has %d %s", got, v.collection)
		}
		// Each row carries the dataset element unchanged and in order.
		rows, err := p.db.Query(`SELECT record FROM ` + v.view)
		if err != nil {
			t.Fatal(err)
		}
		i := 0
		for rows.Next() {
			var raw []byte
			var record Values
			if err = rows.Scan(&raw); err != nil || json.Unmarshal(raw, &record) != nil {
				t.Fatal(v.view, err)
			}
			if !equalJSON(record, ds[v.collection][i]) {
				t.Fatalf("%s row %d differs from the dataset: %s", v.view, i, raw)
			}
			i++
		}
		rows.Close()
	}
	var seedSum, data string
	var sequence int
	if err = p.db.QueryRow(`SELECT seed_sha256, data, sequence FROM mock_backend_state WHERE id=1`).Scan(&seedSum, &data, &sequence); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(data))
	if seedSum != hex.EncodeToString(sum[:]) || sequence != mockSequenceStart {
		t.Fatal("seed digest or sequence not recorded", seedSum, sequence)
	}
	st, err := p.MockBackendStatus(context.Background())
	if err != nil || !st.MatchesDataset || st.SeedSHA256 != st.DatasetSHA256 || st.SeededAt == "" || len(st.Counts) != len(mockBackendViews) {
		t.Fatal("status of a fresh seed", st, err)
	}
	for _, n := range st.Counts {
		if n.Rows != n.Dataset || n.Dataset != len(ds[n.Collection]) {
			t.Fatal("status count differs from the dataset", n)
		}
	}
	// The view's status column follows the rule the tools apply.
	b := NewBackend(c)
	rows, err := p.db.Query(`SELECT policy_number, status FROM mock_policies`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	statuses := map[string]bool{}
	for rows.Next() {
		var number, status string
		if err = rows.Scan(&number, &status); err != nil {
			t.Fatal(err)
		}
		if want := b.status(b.find("policies", "policy_number", number)); status != want {
			t.Fatalf("mock_policies says %s is %s, Backend.status says %s", number, status, want)
		}
		statuses[status] = true
	}
	if !statuses["active"] || !statuses["expired"] {
		t.Fatal("dataset statuses not derived", statuses)
	}
	if _, err = p.db.Exec(`DELETE FROM mock_clients`); err == nil {
		t.Fatal("mock_clients view accepted a write")
	}
}

func TestSeededToolsWorkThroughStore(t *testing.T) {
	_, p, _ := engineDatabase(t)
	ds := rawDataset(t)
	for i, client := range ds["clients"] {
		found := toolCall(t, p, "find-phone-"+str(client["client_id"]), "find_client", Values{"phone": client["phone"]})
		if errorCode(found) != "" || !equalJSON(found, client) {
			t.Fatal("find_client by phone", client["client_id"], found)
		}
		if i == 0 {
			byIIN := toolCall(t, p, "find-iin", "find_client", Values{"iin": client["iin"]})
			if byIIN["client_id"] != client["client_id"] {
				t.Fatal("find_client by IIN", byIIN)
			}
		}
	}
	if missing := toolCall(t, p, "find-missing", "find_client", Values{"phone": "+77019999999"}); errorCode(missing) != "not_found" {
		t.Fatal("unknown phone found", missing)
	}
	for _, policy := range ds["policies"] {
		number, owner := str(policy["policy_number"]), str(policy["client_id"])
		var status string
		if err := p.db.QueryRow(`SELECT status FROM mock_policies WHERE policy_number=?`, number).Scan(&status); err != nil {
			t.Fatal(err)
		}
		got := toolCall(t, p, "get-"+number, "get_policy", Values{"policy_number": number, "client_id": owner})
		if errorCode(got) != "" || got["product"] != policy["product"] || got["end_date"] != policy["end_date"] || got["status"] != status {
			t.Fatal("get_policy", number, got)
		}
		if other := toolCall(t, p, "foreign-"+number, "get_policy", Values{"policy_number": number, "client_id": "C999"}); errorCode(other) != "not_found" {
			t.Fatal("policy returned to another client", other)
		}
	}
	for _, client := range ds["clients"] {
		id := str(client["client_id"])
		owned := viewRows(t, p, `SELECT count(*) FROM mock_policies WHERE client_id=?`, id)
		got := toolCall(t, p, "list-"+id, "get_policies", Values{"client_id": id})
		if owned == 0 {
			if errorCode(got) != "not_found" {
				t.Fatal("get_policies for a client without policies", id, got)
			}
			continue
		}
		policies := list(got["policies"])
		if errorCode(got) != "" || len(policies) != owned {
			t.Fatal("get_policies", id, owned, got)
		}
		for _, row := range policies {
			if asMap(row)["client_id"] != id || str(asMap(row)["status"]) == "" {
				t.Fatal("get_policies row", id, row)
			}
		}
	}
	st, err := p.MockBackendStatus(context.Background())
	if err != nil || !st.MatchesDataset {
		t.Fatal("read-only tools changed the stored dataset", st, err)
	}
}

func TestSeedSurvivesRestartAndResetRestoresDataset(t *testing.T) {
	c, p, path := engineDatabase(t)
	ctx := context.Background()
	ds := rawDataset(t)
	var seededAt string
	if err := p.db.QueryRow(`SELECT seeded_at FROM mock_backend_state`).Scan(&seededAt); err != nil {
		t.Fatal(err)
	}
	applied, err := p.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	phone := ds["clients"][0]["phone"]
	created := toolCall(t, p, "create", "create_policy", Values{"product_type": "ogpo", "phone": phone, "price": 30400})
	number := str(created["policy_number"])
	if number != "SQ-OGPO-900001" {
		t.Fatal("create_policy", created)
	}
	p.Close()

	restarted, err := OpenSQLite(ctx, path, c)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if again, err := restarted.AppliedMigrations(ctx); err != nil || !equalJSON(again, applied) {
		t.Fatal("restart re-applied migrations", again, applied, err)
	}
	var seededAgain string
	var sequence int
	if err = restarted.db.QueryRow(`SELECT seeded_at, sequence FROM mock_backend_state`).Scan(&seededAgain, &sequence); err != nil || seededAgain != seededAt || sequence != mockSequenceStart+1 {
		t.Fatal("restart reseeded", seededAt, seededAgain, sequence, err)
	}
	if got := viewRows(t, restarted, `SELECT count(*) FROM mock_policies`); got != len(ds["policies"])+1 {
		t.Fatal("created policy lost on restart", got)
	}
	if status := viewRows(t, restarted, `SELECT count(*) FROM mock_policies WHERE policy_number=? AND status='pending_payment'`, number); status != 1 {
		t.Fatal("created policy not visible in mock_policies")
	}
	if got := toolCall(t, restarted, "get-created", "get_policy", Values{"policy_number": number}); got["status"] != "pending_payment" {
		t.Fatal("get_policy after restart", got)
	}
	if st, err := restarted.MockBackendStatus(ctx); err != nil || st.MatchesDataset || st.Sequence != mockSequenceStart+1 {
		t.Fatal("status after a mutation", st, err)
	}
	turns := viewRows(t, restarted, `SELECT count(*) FROM turns`)

	if err = restarted.ResetMockBackend(ctx); err != nil {
		t.Fatal(err)
	}
	for _, v := range mockBackendViews {
		if got := viewRows(t, restarted, `SELECT count(*) FROM `+v.view); got != len(ds[v.collection]) {
			t.Fatalf("after -reset-data %s has %d rows", v.view, got)
		}
	}
	if got := toolCall(t, restarted, "get-after-reset", "get_policy", Values{"policy_number": number}); errorCode(got) != "not_found" {
		t.Fatal("reset kept the created policy", got)
	}
	st, err := restarted.MockBackendStatus(ctx)
	if err != nil || !st.MatchesDataset || st.Sequence != mockSequenceStart || st.SeededAt == seededAt {
		t.Fatal("status after reset", st, err)
	}
	if got := viewRows(t, restarted, `SELECT count(*) FROM turns`); got != turns+1 {
		t.Fatal("-reset-data touched turns", turns, got)
	}
	// Generated numbers repeat after a reset, so a demo script replays exactly.
	if again := toolCall(t, restarted, "create-again", "create_policy", Values{"product_type": "ogpo", "phone": phone, "price": 30400}); again["policy_number"] != number {
		t.Fatal("reset did not restart generated IDs", again)
	}

	if err = restarted.ResetAll(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "turns", "turn_events", "intent_reviews", "tool_executions"} {
		if got := viewRows(t, restarted, `SELECT count(*) FROM `+table); got != 0 {
			t.Fatalf("-reset-all left %d rows in %s", got, table)
		}
	}
	if st, err = restarted.MockBackendStatus(ctx); err != nil || !st.MatchesDataset || st.Sequence != mockSequenceStart {
		t.Fatal("status after reset all", st, err)
	}
	if err = restarted.Ready(ctx); err != nil {
		t.Fatal(err)
	}
}

// A database created before migration 002 existed was seeded by startup code
// and may hold tool mutations; upgrading must keep them.
func TestSeedMigrationKeepsLegacySeededState(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	legacy := clone(c.Seed)
	legacy["clients"] = append(list(legacy["clients"]), Values{"client_id": "C900003", "phone": "+77017770000"})
	asMap(list(legacy["clients"])[0])["email"] = "changed@mail.example"
	state, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE router_schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')))`,
		ms[0].sql,
		`INSERT INTO router_schema_migrations(version) VALUES(1)`,
	} {
		if _, err = raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = raw.Exec(`INSERT INTO mock_backend_state(id, data, sequence, updated_at) VALUES(1, ?, 900003, '2026-09-01T00:00:00Z')`, string(state)); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	p, err := OpenSQLite(context.Background(), path, c)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := appliedVersions(t, p); len(got) != len(ms) {
		t.Fatal("upgrade did not apply the new migrations", got)
	}
	var data string
	var sequence int
	var seedSum sql.NullString
	if err = p.db.QueryRow(`SELECT data, sequence, seed_sha256 FROM mock_backend_state`).Scan(&data, &sequence, &seedSum); err != nil {
		t.Fatal(err)
	}
	if data != string(state) || sequence != 900003 || seedSum.Valid {
		t.Fatal("upgrade overwrote the legacy state", sequence, seedSum)
	}
	if got := viewRows(t, p, `SELECT count(*) FROM mock_clients`); got != len(list(c.Seed["clients"]))+1 {
		t.Fatal("views do not show legacy records", got)
	}
	if email := toolCall(t, p, "legacy", "find_client", Values{"phone": asMap(list(c.Seed["clients"])[0])["phone"]})["email"]; email != "changed@mail.example" {
		t.Fatal("legacy mutation lost", email)
	}
	st, err := p.MockBackendStatus(context.Background())
	if err != nil || st.SeedSHA256 != "" || st.MatchesDataset {
		t.Fatal("legacy status", st, err)
	}
}
