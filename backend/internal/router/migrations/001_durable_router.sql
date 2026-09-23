-- JSON lives in TEXT columns checked with json_valid. Timestamps are ISO-8601
-- UTC text so Go parses them as RFC 3339.
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    state TEXT NOT NULL CHECK(json_valid(state)),
    version INTEGER NOT NULL DEFAULT 0 CHECK(version>=0),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE turns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    request_id TEXT NOT NULL,
    input_fingerprint TEXT NOT NULL,
    input TEXT NOT NULL CHECK(json_valid(input)),
    phase TEXT NOT NULL,
    checkpoint TEXT NOT NULL CHECK(json_valid(checkpoint)),
    final_output TEXT CHECK(final_output IS NULL OR json_valid(final_output)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE(session_id, request_id),
    CHECK ((phase='finished') = (final_output IS NOT NULL))
);
CREATE INDEX turns_history ON turns(session_id, id DESC) WHERE phase='finished';
CREATE UNIQUE INDEX one_active_turn_per_session ON turns(session_id) WHERE phase <> 'finished';

CREATE TABLE turn_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    data TEXT NOT NULL CHECK(json_valid(data)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    FOREIGN KEY(session_id, request_id) REFERENCES turns(session_id, request_id)
);
CREATE INDEX turn_events_order ON turn_events(session_id, request_id, id);

CREATE TABLE intent_reviews (
    session_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    proposal_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(revision>0),
    target TEXT NOT NULL CHECK(target IN ('user','operator')),
    status TEXT NOT NULL,
    proposal TEXT NOT NULL CHECK(json_valid(proposal)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY(session_id, request_id, proposal_id, revision),
    FOREIGN KEY(session_id, request_id) REFERENCES turns(session_id, request_id)
);

CREATE TABLE tool_executions (
    session_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    name TEXT NOT NULL,
    arguments_fingerprint TEXT NOT NULL,
    arguments TEXT NOT NULL CHECK(json_valid(arguments)),
    result TEXT NOT NULL CHECK(json_valid(result)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY(session_id, request_id, operation_id),
    FOREIGN KEY(session_id, request_id) REFERENCES turns(session_id, request_id)
);

CREATE TABLE mock_backend_state (
    id INTEGER PRIMARY KEY CHECK(id=1),
    data TEXT NOT NULL CHECK(json_valid(data)),
    sequence INTEGER NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
