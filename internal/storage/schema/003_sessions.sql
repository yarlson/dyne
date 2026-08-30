CREATE TABLE sessions (
    name TEXT PRIMARY KEY,
    intent_id TEXT NOT NULL,
    runtime_scope TEXT NOT NULL,
    image TEXT NOT NULL,
    definition BYTEA NOT NULL,
    repository TEXT NOT NULL,
    initial_ref TEXT NOT NULL,
    workflow_run TEXT NOT NULL,
    workflow_step TEXT NOT NULL,
    deletion_storage BOOLEAN,
    created_at BIGINT NOT NULL
);

CREATE TABLE session_tasks (
    session_name TEXT NOT NULL REFERENCES sessions(name) ON DELETE CASCADE,
    task_id TEXT NOT NULL,
    prompt TEXT NOT NULL,
    timeout_nanoseconds BIGINT NOT NULL,
    result_kind TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'running', 'completed', 'blocked', 'failed', 'canceled')),
    outcome BYTEA,
    pull_request BYTEA,
    workflow_output BYTEA,
    failure TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    finished_at BIGINT,
    PRIMARY KEY (session_name, task_id)
);

CREATE UNIQUE INDEX session_tasks_one_active
ON session_tasks(session_name)
WHERE state IN ('pending', 'running');
