-- Seeds the synthetic business backend (clients, policies, claims, payments,
-- meta, defaults) from the dataset embedded in the binary:
-- voice_router_dataset/mock_backend.json. The migrator binds these parameters:
--   :mock_backend         the dataset as canonical JSON (Go json.Marshal of
--                         Catalog.Seed: object keys sorted, no whitespace)
--   :mock_backend_sha256  hex SHA-256 of exactly that text
--   :mock_sequence        first value of the synthetic ID counter (900000);
--                         tools allocate SQ-...-900001, CL-900002, ...
--   :now                  migration time, RFC 3339 UTC
--
-- The whole backend stays one JSON document in mock_backend_state.data, which
-- the tools read and rewrite inside their own transaction (store.go Tool).
-- ON CONFLICT DO NOTHING makes the step safe on databases seeded by the
-- pre-002 startup code: their state, including every mutation, is kept and
-- seed_sha256 stays NULL. The router applies this once on first start; a
-- restart never reseeds. `go run ./cmd/migrate -reset-data` re-seeds on
-- purpose with the same parameters.
ALTER TABLE mock_backend_state ADD COLUMN seed_sha256 TEXT;
ALTER TABLE mock_backend_state ADD COLUMN seeded_at TEXT;

INSERT INTO mock_backend_state(id, data, sequence, seed_sha256, seeded_at, updated_at)
VALUES (1, :mock_backend, :mock_sequence, :mock_backend_sha256, :now, :now)
ON CONFLICT(id) DO NOTHING;
