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
