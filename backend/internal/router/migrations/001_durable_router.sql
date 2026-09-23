CREATE TABLE sessions (
    id text PRIMARY KEY,
    state jsonb NOT NULL,
    version bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE turns (
    id bigserial PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions(id),
    request_id text NOT NULL,
    input_fingerprint text NOT NULL,
    input jsonb NOT NULL,
    phase text NOT NULL,
    checkpoint jsonb NOT NULL,
    final_output jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(session_id, request_id),
    CHECK ((phase='finished') = (final_output IS NOT NULL))
);
CREATE INDEX turns_history ON turns(session_id,id DESC) WHERE phase='finished';
CREATE UNIQUE INDEX one_active_turn_per_session ON turns(session_id) WHERE phase <> 'finished';

CREATE TABLE turn_events (
    id bigserial PRIMARY KEY,
    session_id text NOT NULL,
    request_id text NOT NULL,
    kind text NOT NULL,
    data jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY(session_id,request_id) REFERENCES turns(session_id,request_id)
);
CREATE INDEX turn_events_order ON turn_events(session_id,request_id,id);

CREATE TABLE intent_reviews (
    session_id text NOT NULL,
    request_id text NOT NULL,
    proposal_id text NOT NULL,
    revision integer NOT NULL CHECK(revision>0),
    target text NOT NULL CHECK(target IN ('user','operator')),
    status text NOT NULL,
    proposal jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(session_id,request_id,proposal_id,revision),
    FOREIGN KEY(session_id,request_id) REFERENCES turns(session_id,request_id)
);

CREATE TABLE tool_executions (
    session_id text NOT NULL,
    request_id text NOT NULL,
    operation_id text NOT NULL,
    name text NOT NULL,
    arguments_fingerprint text NOT NULL,
    arguments jsonb NOT NULL,
    result jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(session_id,request_id,operation_id),
    FOREIGN KEY(session_id,request_id) REFERENCES turns(session_id,request_id)
);

CREATE TABLE mock_backend_state (
    id integer PRIMARY KEY CHECK(id=1),
    data jsonb NOT NULL,
    sequence bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
