CREATE TABLE publications (
    session_name TEXT PRIMARY KEY REFERENCES sessions(name) ON DELETE CASCADE,
    contents BYTEA NOT NULL,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
