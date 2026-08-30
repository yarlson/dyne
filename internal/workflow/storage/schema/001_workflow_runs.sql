CREATE TABLE workflow_runs (
    name TEXT PRIMARY KEY,
    contents BYTEA NOT NULL,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
