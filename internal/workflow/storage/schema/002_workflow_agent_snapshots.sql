CREATE TABLE workflow_agent_snapshots (
    run_name TEXT NOT NULL REFERENCES workflow_runs(name) ON DELETE CASCADE,
    agent_name TEXT NOT NULL,
    contents BYTEA NOT NULL,
    PRIMARY KEY (run_name, agent_name)
);
