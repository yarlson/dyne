package storage

import (
	"context"
	stdsql "database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yarlson/dyne/internal/session"
	"github.com/yarlson/dyne/internal/storage/sql"
)

// SessionRepository owns durable sessions, tasks, results, and deletion progress.
type SessionRepository struct {
	database *stdsql.DB
	queries  *sql.Queries
}

// Sessions returns the session aggregate repository.
func (d *Database) Sessions() *SessionRepository {
	return &SessionRepository{database: d.database, queries: d.queries}
}

// Create atomically inserts one session and its initial task.
func (r *SessionRepository) Create(ctx context.Context, record session.Record, task session.Task) error {
	definition, err := encodeSessionDefinition(record.Definition)
	if err != nil {
		return fmt.Errorf("encode session %s definition: %w", record.Name, err)
	}

	transaction, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session %s creation: %w", record.Name, err)
	}

	defer func() { _ = transaction.Rollback() }()

	queries := r.queries.WithTx(transaction)
	created, err := queries.CreateSession(ctx, sql.CreateSessionParams{
		Name: record.Name, IntentID: record.IntentID, RuntimeScope: record.RuntimeScope,
		Image: record.Image, Definition: definition, Repository: record.Repository,
		InitialRef: record.InitialRef, WorkflowRun: record.WorkflowRun, WorkflowStep: record.WorkflowStep,
		CreatedAt: record.CreatedAt.UnixNano(),
	})
	if err != nil {
		return fmt.Errorf("create session %s: %w", record.Name, err)
	}

	if created == 0 {
		return session.ErrConflict
	}

	if err := queries.CreateSessionTask(ctx, sessionTaskParams(record.Name, task)); err != nil {
		return fmt.Errorf("create session %s initial task: %w", record.Name, err)
	}

	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit session %s creation: %w", record.Name, err)
	}

	return nil
}

// Get returns one durable session definition.
func (r *SessionRepository) Get(ctx context.Context, name string) (session.Record, error) {
	row, err := r.queries.GetSession(ctx, name)
	if errors.Is(err, stdsql.ErrNoRows) {
		return session.Record{}, session.ErrNotFound
	}

	if err != nil {
		return session.Record{}, fmt.Errorf("get session %s: %w", name, err)
	}

	return decodeSession(row)
}

// AddTask inserts another task when the session has no unfinished task.
func (r *SessionRepository) AddTask(ctx context.Context, name string, task session.Task) error {
	active, err := r.queries.CountActiveSessionTasks(ctx, name)
	if err != nil {
		return fmt.Errorf("check session %s active tasks: %w", name, err)
	}

	if active > 0 {
		return session.ErrActiveTask
	}

	if _, err := r.queries.GetSession(ctx, name); errors.Is(err, stdsql.ErrNoRows) {
		return session.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get session %s before adding task: %w", name, err)
	}

	if err := r.queries.CreateSessionTask(ctx, sessionTaskParams(name, task)); err != nil {
		active, activeErr := r.queries.CountActiveSessionTasks(ctx, name)
		if activeErr == nil && active > 0 {
			return session.ErrActiveTask
		}

		return fmt.Errorf("create session %s task %s: %w", name, task.ID, err)
	}

	return nil
}

// LatestTask returns the newest durable task for one session.
func (r *SessionRepository) LatestTask(ctx context.Context, name string) (session.Task, error) {
	row, err := r.queries.GetLatestSessionTask(ctx, name)
	if errors.Is(err, stdsql.ErrNoRows) {
		return session.Task{}, session.ErrNotFound
	}

	if err != nil {
		return session.Task{}, fmt.Errorf("get session %s latest task: %w", name, err)
	}

	return decodeSessionTask(row), nil
}

// UpdateTask persists an observed task state and validated result.
func (r *SessionRepository) UpdateTask(ctx context.Context, name string, task session.Task) error {
	updated, err := r.queries.UpdateSessionTask(ctx, sql.UpdateSessionTaskParams{
		SessionName: name, TaskID: task.ID, State: string(task.State),
		Outcome: task.Artifacts.Outcome, PullRequest: task.Artifacts.PullRequest,
		WorkflowOutput: task.Artifacts.WorkflowOutput, Failure: task.Failure,
		FinishedAt: nullableTime(task.FinishedAt),
	})
	if err != nil {
		return fmt.Errorf("update session %s task %s: %w", name, task.ID, err)
	}

	if updated == 0 {
		return session.ErrNotFound
	}

	return nil
}

// BeginDeletion records cleanup intent before runtime resources are removed.
func (r *SessionRepository) BeginDeletion(ctx context.Context, name string, deleteStorage bool) error {
	updated, err := r.queries.SetSessionDeletion(ctx, sql.SetSessionDeletionParams{
		Name: name, DeletionStorage: stdsql.NullBool{Bool: deleteStorage, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("record session %s deletion: %w", name, err)
	}

	if updated > 0 {
		return nil
	}

	record, getErr := r.Get(ctx, name)
	if errors.Is(getErr, session.ErrNotFound) {
		return session.ErrNotFound
	}

	if getErr != nil {
		return getErr
	}

	if record.Deletion != nil && record.Deletion.Storage != deleteStorage {
		return session.ErrConflict
	}

	return nil
}

// FinishDeletion removes destroyed sessions or clears completed compute cleanup.
func (r *SessionRepository) FinishDeletion(ctx context.Context, name string, removeRecord bool) error {
	var (
		updated int64
		err     error
	)
	if removeRecord {
		updated, err = r.queries.DeleteSession(ctx, name)
	} else {
		updated, err = r.queries.ClearSessionDeletion(ctx, name)
	}

	if err != nil {
		return fmt.Errorf("finish session %s deletion: %w", name, err)
	}

	if updated == 0 {
		return session.ErrNotFound
	}

	return nil
}

// Deleting returns sessions with unfinished runtime cleanup.
func (r *SessionRepository) Deleting(ctx context.Context) ([]session.Record, error) {
	rows, err := r.queries.ListDeletingSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list deleting sessions: %w", err)
	}

	records := make([]session.Record, len(rows))
	for i := range rows {
		records[i], err = decodeSession(rows[i])
		if err != nil {
			return nil, err
		}
	}

	return records, nil
}

func sessionTaskParams(name string, task session.Task) sql.CreateSessionTaskParams {
	return sql.CreateSessionTaskParams{
		SessionName: name, TaskID: task.ID, Prompt: task.Prompt,
		TimeoutNanoseconds: int64(task.Timeout), ResultKind: string(task.ResultKind), State: string(task.State),
		Outcome: task.Artifacts.Outcome, PullRequest: task.Artifacts.PullRequest,
		WorkflowOutput: task.Artifacts.WorkflowOutput, Failure: task.Failure,
		CreatedAt: task.CreatedAt.UnixNano(), FinishedAt: nullableTime(task.FinishedAt),
	}
}

func decodeSession(row sql.Session) (session.Record, error) {
	definition, err := decodeSessionDefinition(row.Definition)
	if err != nil {
		return session.Record{}, fmt.Errorf("decode session %s definition: %w", row.Name, err)
	}

	var deletion *session.Deletion
	if row.DeletionStorage.Valid {
		deletion = &session.Deletion{Storage: row.DeletionStorage.Bool}
	}

	return session.Record{
		Name: row.Name, IntentID: row.IntentID, RuntimeScope: row.RuntimeScope, Image: row.Image,
		Definition: definition, Repository: row.Repository, InitialRef: row.InitialRef,
		WorkflowRun: row.WorkflowRun, WorkflowStep: row.WorkflowStep,
		Deletion: deletion, CreatedAt: time.Unix(0, row.CreatedAt).UTC(),
	}, nil
}

func decodeSessionTask(row sql.SessionTask) session.Task {
	var finishedAt *time.Time
	if row.FinishedAt.Valid {
		value := time.Unix(0, row.FinishedAt.Int64).UTC()
		finishedAt = &value
	}

	return session.Task{
		ID: row.TaskID, Prompt: row.Prompt, Timeout: time.Duration(row.TimeoutNanoseconds),
		ResultKind: session.ResultKind(row.ResultKind), State: session.TaskState(row.State),
		Artifacts: session.Artifacts{
			Outcome: row.Outcome, PullRequest: row.PullRequest, WorkflowOutput: row.WorkflowOutput,
		},
		Failure: row.Failure, CreatedAt: time.Unix(0, row.CreatedAt).UTC(), FinishedAt: finishedAt,
	}
}

func nullableTime(value *time.Time) stdsql.NullInt64 {
	if value == nil {
		return stdsql.NullInt64{}
	}

	return stdsql.NullInt64{Int64: value.UnixNano(), Valid: true}
}
