package workflow

import (
	"context"
	"errors"

	"github.com/yarlson/dyne/internal/session"
)

var (
	// ErrRunNotFound means the requested durable workflow run does not exist.
	ErrRunNotFound = errors.New("workflow run not found")
	// ErrConcurrentUpdate means the run changed after the caller read it.
	ErrConcurrentUpdate = errors.New("workflow run changed")
)

// Repository owns durable workflow runs and their resolved agent definitions.
type Repository interface {
	Create(context.Context, Run, map[string]session.Definition) (Run, error)
	Update(context.Context, Run) (Run, error)
	Run(context.Context, string) (Run, error)
	Runs(context.Context) ([]Run, error)
	SessionDefinition(context.Context, string, string) (session.Definition, error)
	Delete(context.Context, string) error
}
