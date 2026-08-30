package workflow

import (
	"context"
	"errors"

	"github.com/yarlson/dyne/internal/agent"
)

var (
	// ErrRunNotFound means the requested durable workflow run does not exist.
	ErrRunNotFound = errors.New("workflow run not found")
	// ErrConcurrentUpdate means the run changed after the caller read it.
	ErrConcurrentUpdate = errors.New("workflow run changed")
)

// Repository owns durable workflow runs and their resolved agent definitions.
type Repository interface {
	Create(context.Context, Run, map[string]agent.AgentDefinition) (Run, error)
	Update(context.Context, Run) (Run, error)
	Run(context.Context, string) (Run, error)
	Runs(context.Context) ([]Run, error)
	Agent(context.Context, string, string) (agent.AgentDefinition, error)
	Delete(context.Context, string) error
}
