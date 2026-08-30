package storage

import (
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/yarlson/dyne/internal/agent"
	"github.com/yarlson/dyne/internal/workflow"
	"github.com/yarlson/dyne/internal/workflow/storage/sql"
)

// Repository owns durable workflow runs and immutable agent snapshots.
type Repository struct {
	database *stdsql.DB
	queries  *sql.Queries
}

// Close releases the database connection.
func (r *Repository) Close() error {
	return r.database.Close()
}

// Create atomically inserts one run and all resolved agent definitions.
func (r *Repository) Create(
	ctx context.Context, run workflow.Run, agents map[string]agent.AgentDefinition,
) (workflow.Run, error) {
	contents, err := json.Marshal(run)
	if err != nil {
		return workflow.Run{}, fmt.Errorf("encode workflow run %s: %w", run.Name, err)
	}

	transaction, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return workflow.Run{}, fmt.Errorf("begin workflow run creation: %w", err)
	}

	defer func() { _ = transaction.Rollback() }()

	queries := r.queries.WithTx(transaction)
	created, err := queries.CreateRun(ctx, sql.CreateRunParams{Name: run.Name, Contents: contents})
	if err != nil {
		return workflow.Run{}, fmt.Errorf("create workflow run %s: %w", run.Name, err)
	}

	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}

	slices.Sort(names)
	for _, name := range names {
		definition, err := json.Marshal(agents[name])
		if err != nil {
			return workflow.Run{}, fmt.Errorf("encode workflow run %s agent %s: %w", run.Name, name, err)
		}

		if err := queries.CreateAgentSnapshot(ctx, sql.CreateAgentSnapshotParams{
			RunName: run.Name, AgentName: name, Contents: definition,
		}); err != nil {
			return workflow.Run{}, fmt.Errorf("create workflow run %s agent %s snapshot: %w", run.Name, name, err)
		}
	}

	if err := transaction.Commit(); err != nil {
		return workflow.Run{}, fmt.Errorf("commit workflow run %s creation: %w", run.Name, err)
	}

	run.Revision = created.Version

	return run, nil
}

// Update replaces a run only when its optimistic version is current.
func (r *Repository) Update(ctx context.Context, run workflow.Run) (workflow.Run, error) {
	contents, err := json.Marshal(run)
	if err != nil {
		return workflow.Run{}, fmt.Errorf("encode workflow run %s: %w", run.Name, err)
	}

	updated, err := r.queries.UpdateRun(ctx, sql.UpdateRunParams{
		Name: run.Name, Contents: contents, Version: run.Revision,
	})
	if errors.Is(err, stdsql.ErrNoRows) {
		return workflow.Run{}, workflow.ErrConcurrentUpdate
	}

	if err != nil {
		return workflow.Run{}, fmt.Errorf("update workflow run %s: %w", run.Name, err)
	}

	run.Revision = updated.Version

	return run, nil
}

// Run returns one durable workflow run.
func (r *Repository) Run(ctx context.Context, name string) (workflow.Run, error) {
	value, err := r.queries.GetRun(ctx, name)
	if errors.Is(err, stdsql.ErrNoRows) {
		return workflow.Run{}, workflow.ErrRunNotFound
	}

	if err != nil {
		return workflow.Run{}, fmt.Errorf("get workflow run %s: %w", name, err)
	}

	return decodeRun(value)
}

// Runs returns every durable workflow run in name order.
func (r *Repository) Runs(ctx context.Context) ([]workflow.Run, error) {
	rows, err := r.queries.ListRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}

	result := make([]workflow.Run, len(rows))
	for i := range rows {
		result[i], err = decodeRun(rows[i])
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

// Agent returns one resolved immutable agent definition.
func (r *Repository) Agent(ctx context.Context, run, agentName string) (agent.AgentDefinition, error) {
	contents, err := r.queries.GetAgentSnapshot(ctx, sql.GetAgentSnapshotParams{
		RunName: run, AgentName: agentName,
	})
	if errors.Is(err, stdsql.ErrNoRows) {
		return agent.AgentDefinition{}, workflow.ErrRunNotFound
	}

	if err != nil {
		return agent.AgentDefinition{}, fmt.Errorf("get workflow run %s agent %s snapshot: %w", run, agentName, err)
	}

	var definition agent.AgentDefinition
	if err := json.Unmarshal(contents, &definition); err != nil {
		return agent.AgentDefinition{}, fmt.Errorf("decode workflow run %s agent %s snapshot: %w", run, agentName, err)
	}

	if definition.Name != agentName {
		return agent.AgentDefinition{}, fmt.Errorf("workflow run %s agent %s snapshot has an invalid name", run, agentName)
	}

	return definition, nil
}

// Delete removes one run and its snapshots in one database change.
func (r *Repository) Delete(ctx context.Context, name string) error {
	deleted, err := r.queries.DeleteRun(ctx, name)
	if err != nil {
		return fmt.Errorf("delete workflow run %s: %w", name, err)
	}

	if deleted == 0 {
		return workflow.ErrRunNotFound
	}

	return nil
}

func decodeRun(row sql.WorkflowRun) (workflow.Run, error) {
	var run workflow.Run
	if err := json.Unmarshal(row.Contents, &run); err != nil {
		return workflow.Run{}, fmt.Errorf("decode workflow run %s: %w", row.Name, err)
	}

	if run.Version != "v1" || run.Name != row.Name {
		return workflow.Run{}, fmt.Errorf("workflow run %s has an invalid durable definition", row.Name)
	}

	run.Revision = row.Version

	return run, nil
}
