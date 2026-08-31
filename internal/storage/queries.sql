-- name: CreateRun :one
INSERT INTO workflow_runs (name, contents)
VALUES ($1, $2)
RETURNING name, contents, version;

-- name: GetRun :one
SELECT name, contents, version
FROM workflow_runs
WHERE name = $1;

-- name: ListRuns :many
SELECT name, contents, version
FROM workflow_runs
ORDER BY name;

-- name: UpdateRun :one
UPDATE workflow_runs
SET contents = $2, version = version + 1
WHERE name = $1 AND version = $3
RETURNING name, contents, version;

-- name: CreateAgentSnapshot :exec
INSERT INTO workflow_agent_snapshots (run_name, agent_name, contents)
VALUES ($1, $2, $3);

-- name: GetAgentSnapshot :one
SELECT contents
FROM workflow_agent_snapshots
WHERE run_name = $1 AND agent_name = $2;

-- name: DeleteRun :execrows
DELETE FROM workflow_runs
WHERE name = $1;

-- name: CreateSession :execrows
INSERT INTO sessions (
    name, intent_id, runtime_scope, image, definition, repository, initial_ref,
    workflow_run, workflow_step, deletion_storage, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULL, $10)
ON CONFLICT (name) DO NOTHING;

-- name: GetSession :one
SELECT name, intent_id, runtime_scope, image, definition, repository, initial_ref,
       workflow_run, workflow_step, deletion_storage, created_at
FROM sessions
WHERE name = $1;

-- name: ListDeletingSessions :many
SELECT name, intent_id, runtime_scope, image, definition, repository, initial_ref,
       workflow_run, workflow_step, deletion_storage, created_at
FROM sessions
WHERE deletion_storage IS NOT NULL
ORDER BY name;

-- name: CreateSessionTask :exec
INSERT INTO session_tasks (
    session_name, task_id, prompt, timeout_nanoseconds, result_kind, state,
    change_input, outcome, pull_request, workflow_output, change_artifact,
    failure, created_at, finished_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);

-- name: GetLatestSessionTask :one
SELECT session_name, task_id, prompt, timeout_nanoseconds, result_kind, state,
       outcome, pull_request, workflow_output, failure, created_at, finished_at,
       change_input, change_artifact
FROM session_tasks
WHERE session_name = $1
ORDER BY created_at DESC, task_id DESC
LIMIT 1;

-- name: CountActiveSessionTasks :one
SELECT COUNT(*)
FROM session_tasks
WHERE session_name = $1 AND state IN ('pending', 'running');

-- name: UpdateSessionTask :execrows
UPDATE session_tasks
SET state = $3,
    outcome = $4,
    pull_request = $5,
    workflow_output = $6,
    change_artifact = $7,
    failure = $8,
    finished_at = $9
WHERE session_name = $1 AND task_id = $2;

-- name: SetSessionDeletion :execrows
UPDATE sessions
SET deletion_storage = $2
WHERE name = $1 AND (deletion_storage IS NULL OR deletion_storage = $2);

-- name: ClearSessionDeletion :execrows
UPDATE sessions
SET deletion_storage = NULL
WHERE name = $1;

-- name: DeleteSession :execrows
DELETE FROM sessions
WHERE name = $1;

-- name: CreatePublication :one
INSERT INTO publications (session_name, contents)
VALUES ($1, $2)
RETURNING session_name, contents, version;

-- name: GetPublication :one
SELECT session_name, contents, version
FROM publications
WHERE session_name = $1;

-- name: UpdatePublication :one
UPDATE publications
SET contents = $2, version = version + 1
WHERE session_name = $1 AND version = $3
RETURNING session_name, contents, version;
