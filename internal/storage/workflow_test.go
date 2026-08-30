package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/session"
	"github.com/yarlson/dyne/internal/workflow"
)

func TestRepositoryCreatesWorkflowRunAndSnapshotsAtomically(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Workflows()
	ctx := context.Background()

	created, err := repository.Create(ctx, workflow.Run{
		Version: "v1", Name: "delivery-123", State: workflow.StatePending,
	}, map[string]session.Definition{
		"reviewer": {Agent: "reviewer", Instructions: "review"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)

	definition, err := repository.SessionDefinition(ctx, "delivery-123", "reviewer")
	require.NoError(t, err)
	assert.Equal(t, "review", definition.Instructions)
}

func TestRepositoryRejectsStaleRunUpdate(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Workflows()
	ctx := context.Background()
	created, err := repository.Create(ctx, workflow.Run{
		Version: "v1", Name: "delivery-123", State: workflow.StatePending,
	}, nil)
	require.NoError(t, err)

	updated := created
	updated.State = workflow.StateRunning
	updated, err = repository.Update(ctx, updated)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)

	created.State = workflow.StateFailed
	_, err = repository.Update(ctx, created)
	assert.ErrorIs(t, err, workflow.ErrConcurrentUpdate)

	current, err := repository.Run(ctx, "delivery-123")
	require.NoError(t, err)
	assert.Equal(t, workflow.StateRunning, current.State)
}

func TestRepositoryDeletesRunAndSnapshotsTogether(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Workflows()
	ctx := context.Background()
	_, err := repository.Create(ctx, workflow.Run{Version: "v1", Name: "delivery-123"}, map[string]session.Definition{
		"reviewer": {Agent: "reviewer"},
	})
	require.NoError(t, err)

	require.NoError(t, repository.Delete(ctx, "delivery-123"))
	_, err = repository.Run(ctx, "delivery-123")
	assert.ErrorIs(t, err, workflow.ErrRunNotFound)
	_, err = repository.SessionDefinition(ctx, "delivery-123", "reviewer")
	assert.ErrorIs(t, err, workflow.ErrRunNotFound)
}

func TestRepositoryReadsEstablishedAgentSnapshotShape(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Workflows()
	ctx := context.Background()
	_, err := repository.Create(ctx, workflow.Run{Version: "v1", Name: "delivery-123"}, nil)
	require.NoError(t, err)
	_, err = database.database.ExecContext(ctx, `
		INSERT INTO workflow_agent_snapshots (run_name, agent_name, contents)
		VALUES (?, ?, ?)
	`, "delivery-123", "reviewer", []byte(`{"Name":"reviewer","Storage":"ephemeral","Instructions":"review","Timeout":3600000000000}`))
	require.NoError(t, err)

	definition, err := repository.SessionDefinition(ctx, "delivery-123", "reviewer")
	require.NoError(t, err)
	assert.Equal(t, session.Definition{
		Agent: "reviewer", Storage: session.StorageEphemeral, Instructions: "review", Timeout: time.Hour,
	}, definition)
}

func TestOpenRejectsUnsupportedURL(t *testing.T) {
	_, err := Open(context.Background(), "mysql://localhost/dyne")
	require.ErrorContains(t, err, "unsupported database URL")
}

func TestOpenProtectsLocalFileAndReappliesNoSchemaChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.db")
	repository, err := Open(context.Background(), "sqlite:"+path)
	require.NoError(t, err)
	require.NoError(t, repository.Close())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	repository, err = Open(context.Background(), "sqlite:"+path)
	require.NoError(t, err)
	require.NoError(t, repository.Close())
}

func openTestDatabase(t *testing.T) *Database {
	t.Helper()
	database, err := Open(context.Background(), "sqlite::memory:")
	require.NoError(t, err)

	return database
}
