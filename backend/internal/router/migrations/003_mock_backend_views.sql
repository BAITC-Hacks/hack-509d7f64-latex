-- Read-only views over the synthetic backend that 002 seeds. The backend is one
-- JSON document (mock_backend_state.data, id=1), and each view unnests one of
-- its arrays with json_each. The views therefore always show the live state,
-- including records that tools created or changed. SQLite views cannot be
-- written: data changes only through the tools (store.go Tool), or through a
-- deliberate re-seed with `go run ./cmd/migrate -reset-data`.
--
--   mock_clients   one row per data.clients[]  (find_client, update_contact)
--   mock_policies  one row per data.policies[] (get_policy, get_policies,
--                  create/renew/update/cancel_policy). status mirrors
--                  Backend.status: the stored status when set (pending_payment,
--                  cancelled), otherwise not_yet_active/expired/active from the
--                  start and end dates against data.meta.as_of_date.
--   mock_claims    one row per data.claims[]   (get_claim, create_claim)
--   mock_payments  one row per data.payments[] (check_payment)
--   mock_records   one row per element of every array in the document
--                  (collection, position, record), including collections the
--                  tools add at runtime: bookings, handoffs, callbacks, ...
-- Each typed view also returns the whole element as `record` (JSON text), for
-- fields that are not broken out into columns.
--
-- Inspect with the sqlite3 shell, e.g. from backend/:
--   sqlite3 -readonly -header -column voice_router.db 'SELECT client_id, full_name, phone FROM mock_clients'
--   sqlite3 -readonly -header -column voice_router.db 'SELECT policy_number, client_id, product, status FROM mock_policies'
--   sqlite3 -readonly voice_router.db 'SELECT collection, count(*) FROM mock_records GROUP BY 1'
-- `go run ./cmd/migrate` prints the row count of each view.

CREATE VIEW mock_clients AS
SELECT
    json_extract(c.value, '$.client_id') AS client_id,
    json_extract(c.value, '$.full_name') AS full_name,
    json_extract(c.value, '$.phone') AS phone,
    json_extract(c.value, '$.iin') AS iin,
    json_extract(c.value, '$.city') AS city,
    json_extract(c.value, '$.email') AS email,
    json_extract(c.value, '$.address') AS address,
    json_extract(c.value, '$.bm_class') AS bm_class,
    json_extract(c.value, '$.preferred_language') AS preferred_language,
    c.value AS record
FROM mock_backend_state AS s, json_each(s.data, '$.clients') AS c
WHERE s.id = 1;

CREATE VIEW mock_policies AS
SELECT
    json_extract(p.value, '$.policy_number') AS policy_number,
    json_extract(p.value, '$.client_id') AS client_id,
    json_extract(p.value, '$.product') AS product,
    CASE
        WHEN coalesce(json_extract(p.value, '$.status'), '') <> '' THEN json_extract(p.value, '$.status')
        WHEN coalesce(json_extract(p.value, '$.start_date'), '') > json_extract(s.data, '$.meta.as_of_date') THEN 'not_yet_active'
        WHEN coalesce(json_extract(p.value, '$.end_date'), '') < json_extract(s.data, '$.meta.as_of_date') THEN 'expired'
        ELSE 'active'
    END AS status,
    json_extract(p.value, '$.start_date') AS start_date,
    json_extract(p.value, '$.end_date') AS end_date,
    json_extract(p.value, '$.premium') AS premium,
    json_extract(p.value, '$.details.vehicle_plate') AS vehicle_plate,
    json_extract(p.value, '$.details') AS details,
    p.value AS record
FROM mock_backend_state AS s, json_each(s.data, '$.policies') AS p
WHERE s.id = 1;

CREATE VIEW mock_claims AS
SELECT
    json_extract(c.value, '$.claim_number') AS claim_number,
    json_extract(c.value, '$.client_id') AS client_id,
    json_extract(c.value, '$.policy_number') AS policy_number,
    json_extract(c.value, '$.claim_type') AS claim_type,
    json_extract(c.value, '$.incident_date') AS incident_date,
    json_extract(c.value, '$.status') AS status,
    json_extract(c.value, '$.approved_amount') AS approved_amount,
    json_extract(c.value, '$.next_step') AS next_step,
    c.value AS record
FROM mock_backend_state AS s, json_each(s.data, '$.claims') AS c
WHERE s.id = 1;

CREATE VIEW mock_payments AS
SELECT
    json_extract(p.value, '$.payment_id') AS payment_id,
    json_extract(p.value, '$.client_id') AS client_id,
    json_extract(p.value, '$.policy_number') AS policy_number,
    json_extract(p.value, '$.date') AS payment_date,
    json_extract(p.value, '$.amount') AS amount,
    json_extract(p.value, '$.product') AS product,
    json_extract(p.value, '$.status') AS status,
    json_extract(p.value, '$.note') AS note,
    p.value AS record
FROM mock_backend_state AS s, json_each(s.data, '$.payments') AS p
WHERE s.id = 1;

CREATE VIEW mock_records AS
SELECT
    t.key AS collection,
    r.key AS position,
    r.value AS record
FROM mock_backend_state AS s, json_each(s.data) AS t, json_each(t.value) AS r
WHERE s.id = 1 AND t.type = 'array';
