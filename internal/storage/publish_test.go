package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/publish"
	"github.com/yarlson/dyne/internal/session"
)

func TestPublicationRepositoryRetainsIntentProgressAndResult(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	ctx := context.Background()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	require.NoError(t, database.Sessions().Create(ctx,
		session.Record{Name: "review", IntentID: "session-intent", CreatedAt: now},
		session.Task{ID: "review", State: session.TaskCompleted, CreatedAt: now},
	))
	record := publish.Record{
		Session: "review", IntentID: "publish-intent",
		Request:    publish.Request{Session: "review", Branch: "yar/review", Timeout: time.Minute},
		Repository: "https://github.com/lokalise/ratchet-test-service", State: publish.StateReady,
		CreatedAt: now, UpdatedAt: now,
	}

	created, err := database.Publications().Create(ctx, record)
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)

	created.State = publish.StateCompleted
	created.CommitSHA = "9a4484441215661904e02a807adf5034d13f5bbe"
	created.PullRequestNumber = 17
	created.PullRequestURL = "https://github.com/lokalise/ratchet-test-service/pull/17"
	updated, err := database.Publications().Update(ctx, created)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)

	stored, err := database.Publications().Get(ctx, "review")
	require.NoError(t, err)
	assert.Equal(t, publish.StateCompleted, stored.State)
	assert.Equal(t, 17, stored.PullRequestNumber)
}

func TestPublicationRepositoryRejectsAnotherIntentForSession(t *testing.T) {
	database := openTestDatabase(t)
	defer func() { require.NoError(t, database.Close()) }()
	ctx := context.Background()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	require.NoError(t, database.Sessions().Create(ctx,
		session.Record{Name: "review", IntentID: "session-intent", CreatedAt: now},
		session.Task{ID: "review", State: session.TaskCompleted, CreatedAt: now},
	))
	repository := database.Publications()
	_, err := repository.Create(ctx, publish.Record{Session: "review", IntentID: "first"})
	require.NoError(t, err)

	_, err = repository.Create(ctx, publish.Record{Session: "review", IntentID: "second"})
	assert.ErrorIs(t, err, publish.ErrConflict)
}
