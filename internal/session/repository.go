package session

import (
	"context"
	"errors"
)

var (
	// ErrNotFound means the requested durable session or task does not exist.
	ErrNotFound = errors.New("session not found")
	// ErrConflict means a durable session already owns the requested identity.
	ErrConflict = errors.New("session conflict")
	// ErrActiveTask means the session already has unfinished work.
	ErrActiveTask = errors.New("session has an active task")
)

// Repository owns durable sessions, tasks, results, and deletion progress.
type Repository interface {
	Create(context.Context, Record, Task) error
	Get(context.Context, string) (Record, error)
	AddTask(context.Context, string, Task) error
	LatestTask(context.Context, string) (Task, error)
	UpdateTask(context.Context, string, Task) error
	BeginDeletion(context.Context, string, bool) error
	FinishDeletion(context.Context, string, bool) error
	Deleting(context.Context) ([]Record, error)
}
