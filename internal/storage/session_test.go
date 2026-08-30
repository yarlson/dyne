package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/session"
)

func TestRepositoryCreatesSessionAndInitialTaskAtomically(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Sessions()
	createdAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	record := session.Record{
		Name: "review", IntentID: "intent-123", RuntimeScope: "coding-agents", Image: "coding-agent:test",
		Definition: session.Definition{
			Agent: "implementer", Storage: session.StoragePersistent, Instructions: "Make the change.",
			Skills: []session.Skill{{Name: "testing", Contents: "test behavior"}}, SetupCommand: "make tools",
			CloneDepth: 1, StorageSize: "10Gi", Timeout: time.Hour,
		},
		Repository: "https://github.com/lokalise/ratchet-test-service", InitialRef: "main", CreatedAt: createdAt,
	}
	task := session.Task{
		ID: "review", Prompt: "fix the link", Timeout: time.Hour, ResultKind: session.ResultKindPullRequest,
		State: session.TaskPending, CreatedAt: createdAt,
	}

	require.NoError(t, repository.Create(context.Background(), record, task))

	stored, err := repository.Get(context.Background(), "review")
	require.NoError(t, err)
	assert.Equal(t, record, stored)
	latest, err := repository.LatestTask(context.Background(), "review")
	require.NoError(t, err)
	assert.Equal(t, task, latest)
}

func TestRepositoryRejectsDuplicateSessionWithoutWaitingForAnotherConnection(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Sessions()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	record := session.Record{Name: "review", IntentID: "first", CreatedAt: now}
	task := session.Task{ID: "review", State: session.TaskPending, CreatedAt: now}
	require.NoError(t, repository.Create(ctx, record, task))

	record.IntentID = "second"
	err := repository.Create(ctx, record, task)
	assert.ErrorIs(t, err, session.ErrConflict)
}

func TestRepositoryRejectsSecondActiveSessionTask(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Sessions()
	ctx := context.Background()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repository.Create(ctx,
		session.Record{Name: "review", IntentID: "intent-123", CreatedAt: now},
		session.Task{ID: "review", State: session.TaskRunning, CreatedAt: now},
	))

	err := repository.AddTask(ctx, "review", session.Task{ID: "review-next", State: session.TaskPending, CreatedAt: now.Add(time.Minute)})
	assert.ErrorIs(t, err, session.ErrActiveTask)
}

func TestRepositoryRetainsCompletedTaskArtifacts(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Sessions()
	ctx := context.Background()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repository.Create(ctx,
		session.Record{Name: "review", IntentID: "intent-123", CreatedAt: now},
		session.Task{ID: "review", State: session.TaskPending, CreatedAt: now},
	))
	finishedAt := now.Add(time.Minute)
	task := session.Task{
		ID: "review", State: session.TaskCompleted, CreatedAt: now, FinishedAt: &finishedAt,
		Artifacts: session.Artifacts{
			Outcome:     []byte(`{"status":"completed","summary":"fixed","blocker":""}`),
			PullRequest: []byte(`{"title":"Fix link","body":"Updates the README."}`),
		},
	}
	require.NoError(t, repository.UpdateTask(ctx, "review", task))

	stored, err := repository.LatestTask(ctx, "review")
	require.NoError(t, err)
	assert.Equal(t, task, stored)
}

func TestRepositoryPersistsDeletionUntilCleanupFinishes(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	repository := database.Sessions()
	ctx := context.Background()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repository.Create(ctx,
		session.Record{Name: "review", IntentID: "intent-123", Definition: session.Definition{Storage: session.StoragePersistent}, CreatedAt: now},
		session.Task{ID: "review", State: session.TaskCompleted, CreatedAt: now},
	))
	require.NoError(t, repository.BeginDeletion(ctx, "review", true))

	deleting, err := repository.Deleting(ctx)
	require.NoError(t, err)
	require.Len(t, deleting, 1)
	assert.Equal(t, &session.Deletion{Storage: true}, deleting[0].Deletion)

	require.NoError(t, repository.FinishDeletion(ctx, "review", true))
	_, err = repository.Get(ctx, "review")
	assert.ErrorIs(t, err, session.ErrNotFound)
}
