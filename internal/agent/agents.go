package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yarlson/dyne/internal/session"
)

// AgentDefinition is one validated reusable agent configuration.
type AgentDefinition struct {
	Name         string
	Description  string
	Storage      Storage
	Instructions string
	Skills       []AgentSkill
	SetupCommand string
	CloneDepth   int
	StorageSize  string
	Timeout      time.Duration
}

// SessionDefinition returns the immutable session configuration owned by this agent.
func (definition AgentDefinition) SessionDefinition() session.Definition {
	return session.Definition{
		Agent: definition.Name, Storage: definition.Storage, Instructions: definition.Instructions,
		Skills: slices.Clone(definition.Skills), SetupCommand: definition.SetupCommand,
		CloneDepth: definition.CloneDepth, StorageSize: definition.StorageSize, Timeout: definition.Timeout,
	}
}

// AgentSummary contains agent metadata that is safe to return to clients.
type AgentSummary struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Storage     Storage  `json:"storage"`
	Skills      []string `json:"skills,omitempty"`
}

// AgentCatalog provides immutable configured agents.
type AgentCatalog interface {
	List() []AgentSummary
	Find(string) (AgentDefinition, bool)
}

// StartRequest contains client-owned inputs for a new configured-agent session.
type StartRequest struct {
	Agent        string
	Name         string
	Repository   string
	InitialRef   string
	Prompt       string
	Timeout      time.Duration
	ResultKind   ResultKind
	WorkflowRun  string
	WorkflowStep string
}

// StartResult identifies the accepted initial task.
type StartResult = session.StartResult

// ErrorKind classifies an agent operation failure for entrypoints.
type ErrorKind string

const (
	// ErrorInvalid identifies a request that cannot describe a valid operation.
	ErrorInvalid ErrorKind = "invalid"
	// ErrorNotFound identifies a requested product entity that does not exist.
	ErrorNotFound ErrorKind = "not_found"
	// ErrorConflict identifies a session identity owned by different durable intent.
	ErrorConflict ErrorKind = "conflict"
	// ErrorUnavailable identifies an external dependency failure.
	ErrorUnavailable ErrorKind = "unavailable"
)

type operationError struct {
	kind    ErrorKind
	message string
	cause   error
}

func (e *operationError) Error() string { return e.message }
func (e *operationError) Unwrap() error { return e.cause }

// ErrorKindOf returns the stable classification of an agent operation error.
func ErrorKindOf(err error) ErrorKind {
	var target *operationError
	if errors.As(err, &target) {
		return target.kind
	}

	return ""
}

type sessionStarter interface {
	Start(context.Context, session.Definition, session.StartRequest) (session.StartResult, error)
}

// Control resolves configured agents into session starts.
type Control struct {
	sessions sessionStarter
	catalog  AgentCatalog
}

// New creates configured-agent control with an explicit session dependency.
func New(sessions *session.Control, catalog AgentCatalog) (*Control, error) {
	return newControl(sessions, catalog)
}

func newControl(sessions sessionStarter, catalog AgentCatalog) (*Control, error) {
	if sessions == nil {
		return nil, errors.New("session control is required")
	}

	return &Control{sessions: sessions, catalog: catalog}, nil
}

// Agents returns safe configured agent metadata.
func (c *Control) Agents() []AgentSummary {
	if c.catalog == nil {
		return []AgentSummary{}
	}

	return c.catalog.List()
}

// Start creates a session from one configured agent.
func (c *Control) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	definition, found := c.findAgent(request.Agent)
	if !found {
		return StartResult{}, &operationError{
			kind: ErrorNotFound, message: fmt.Sprintf("agent %s is not configured", request.Agent),
		}
	}

	result, err := c.sessions.Start(ctx, definition.SessionDefinition(), session.StartRequest{
		Name: request.Name, Repository: request.Repository, InitialRef: request.InitialRef,
		Prompt: request.Prompt, Timeout: request.Timeout, ResultKind: request.ResultKind,
		WorkflowRun: request.WorkflowRun, WorkflowStep: request.WorkflowStep,
	})
	if err != nil {
		kind := ErrorUnavailable
		message := "start session failed"
		switch {
		case session.ErrorKindOf(err) == session.ErrorInvalid:
			kind = ErrorInvalid
			message = err.Error()
		case session.ErrorKindOf(err) == session.ErrorConflict, errors.Is(err, session.ErrConflict):
			kind = ErrorConflict
			message = err.Error()
		}

		return StartResult{}, &operationError{kind: kind, message: message, cause: err}
	}

	return result, nil
}

func (c *Control) findAgent(name string) (AgentDefinition, bool) {
	if c.catalog == nil {
		return AgentDefinition{}, false
	}

	return c.catalog.Find(name)
}
