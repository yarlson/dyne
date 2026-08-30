package storage

import (
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yarlson/dyne/internal/session"
	"github.com/yarlson/dyne/internal/storage/sql"
	"github.com/yarlson/dyne/internal/workflow"
)

// WorkflowRepository owns durable workflow runs and immutable agent snapshots.
type WorkflowRepository struct {
	database *stdsql.DB
	queries  *sql.Queries
}

type sessionSnapshot struct {
	Name         string
	Storage      session.Storage
	Instructions string
	Skills       []session.Skill
	SetupCommand string
	CloneDepth   int
	StorageSize  string
	Timeout      time.Duration
}

// Workflows returns the workflow aggregate repository.
func (d *Database) Workflows() *WorkflowRepository {
	return &WorkflowRepository{database: d.database, queries: d.queries}
}

// Create atomically inserts one run and all resolved agent definitions.
func (r *WorkflowRepository) Create(
	ctx context.Context, run workflow.Run, definitions map[string]session.Definition,
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

	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}

	slices.Sort(names)
	for _, name := range names {
		definition, err := encodeSessionDefinition(definitions[name])
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
func (r *WorkflowRepository) Update(ctx context.Context, run workflow.Run) (workflow.Run, error) {
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
func (r *WorkflowRepository) Run(ctx context.Context, name string) (workflow.Run, error) {
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
func (r *WorkflowRepository) Runs(ctx context.Context) ([]workflow.Run, error) {
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

// SessionDefinition returns one resolved immutable session definition.
func (r *WorkflowRepository) SessionDefinition(ctx context.Context, run, agentName string) (session.Definition, error) {
	contents, err := r.queries.GetAgentSnapshot(ctx, sql.GetAgentSnapshotParams{
		RunName: run, AgentName: agentName,
	})
	if errors.Is(err, stdsql.ErrNoRows) {
		return session.Definition{}, workflow.ErrRunNotFound
	}

	if err != nil {
		return session.Definition{}, fmt.Errorf("get workflow run %s agent %s snapshot: %w", run, agentName, err)
	}

	definition, err := decodeSessionDefinition(contents)
	if err != nil {
		return session.Definition{}, fmt.Errorf("decode workflow run %s agent %s snapshot: %w", run, agentName, err)
	}

	if definition.Agent != agentName {
		return session.Definition{}, fmt.Errorf("workflow run %s agent %s snapshot has an invalid name", run, agentName)
	}

	return definition, nil
}

func encodeSessionDefinition(definition session.Definition) ([]byte, error) {
	return json.Marshal(sessionSnapshot{
		Name: definition.Agent, Storage: definition.Storage, Instructions: definition.Instructions,
		Skills: definition.Skills, SetupCommand: definition.SetupCommand, CloneDepth: definition.CloneDepth,
		StorageSize: definition.StorageSize, Timeout: definition.Timeout,
	})
}

func decodeSessionDefinition(contents []byte) (session.Definition, error) {
	var snapshot sessionSnapshot
	if err := json.Unmarshal(contents, &snapshot); err != nil {
		return session.Definition{}, err
	}

	return session.Definition{
		Agent: snapshot.Name, Storage: snapshot.Storage, Instructions: snapshot.Instructions,
		Skills: snapshot.Skills, SetupCommand: snapshot.SetupCommand, CloneDepth: snapshot.CloneDepth,
		StorageSize: snapshot.StorageSize, Timeout: snapshot.Timeout,
	}, nil
}

// Delete removes one run and its snapshots in one database change.
func (r *WorkflowRepository) Delete(ctx context.Context, name string) error {
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
