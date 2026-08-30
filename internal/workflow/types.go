package workflow

import (
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/yarlson/dyne/internal/session"
)

// ErrorKind classifies a workflow operation failure for entrypoints.
type ErrorKind string

const (
	// ErrorInvalid identifies an invalid workflow request.
	ErrorInvalid ErrorKind = "invalid"
	// ErrorNotFound identifies a workflow or run that does not exist.
	ErrorNotFound ErrorKind = "not_found"
	// ErrorConflict identifies state that prevents the requested transition.
	ErrorConflict ErrorKind = "conflict"
	// ErrorUnavailable identifies a storage or session dependency failure.
	ErrorUnavailable ErrorKind = "unavailable"
)

type operationError struct {
	kind    ErrorKind
	message string
	cause   error
}

func (e *operationError) Error() string { return e.message }
func (e *operationError) Unwrap() error { return e.cause }

// ErrorKindOf returns the stable classification of a workflow operation error.
func ErrorKindOf(err error) ErrorKind {
	var target *operationError
	if errors.As(err, &target) {
		return target.kind
	}

	return ""
}

// State is the durable lifecycle state of a workflow run.
type State string

const (
	// StatePending means no workflow step has started.
	StatePending State = "pending"
	// StateRunning means the workflow has active or schedulable work.
	StateRunning State = "running"
	// StateCompleted means every required step completed.
	StateCompleted State = "completed"
	// StateBlocked means at least one step reported a blocker.
	StateBlocked State = "blocked"
	// StateFailed means at least one step failed.
	StateFailed State = "failed"
	// StateCanceled means cancellation stopped the run.
	StateCanceled State = "canceled"
	// StateDeleting means durable cleanup is in progress.
	StateDeleting State = "deleting"
)

// StepState is the durable lifecycle state of one workflow step.
type StepState string

const (
	// StepPending waits for dependencies and capacity.
	StepPending StepState = "pending"
	// StepStarting has durable intent but may have an ambiguous dispatch outcome.
	StepStarting StepState = "starting"
	// StepRunning has an accepted agent session.
	StepRunning StepState = "running"
	// StepCompleted produced its required result.
	StepCompleted StepState = "completed"
	// StepBlocked reported that it cannot complete.
	StepBlocked StepState = "blocked"
	// StepFailed terminated without a valid completed or blocked result.
	StepFailed StepState = "failed"
	// StepCanceled was stopped by run cancellation.
	StepCanceled StepState = "canceled"
	// StepSkipped depends on a step that did not complete.
	StepSkipped StepState = "skipped"
)

// StartRequest contains client-owned inputs for one workflow run.
type StartRequest struct {
	Workflow   string
	Name       string
	Repository string
	Ref        string
	Prompt     string
}

// Run is the durable, client-visible state of one workflow execution.
type Run struct {
	Version         string          `json:"version"`
	Name            string          `json:"name"`
	Workflow        string          `json:"workflow"`
	Description     string          `json:"description"`
	Repository      string          `json:"repository"`
	Ref             string          `json:"ref"`
	Prompt          string          `json:"prompt"`
	Intent          string          `json:"intent"`
	MaxParallelism  int             `json:"max_parallelism"`
	State           State           `json:"state"`
	CancelRequested bool            `json:"cancel_requested"`
	Steps           map[string]Step `json:"steps"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	Revision        int64           `json:"-"`
}

// Step is the durable state and result of one isolated agent session.
type Step struct {
	Name        string          `json:"name"`
	Agent       string          `json:"agent"`
	Prompt      string          `json:"prompt"`
	After       []string        `json:"after,omitempty"`
	Publishable bool            `json:"publishable,omitempty"`
	Session     string          `json:"session"`
	State       StepState       `json:"state"`
	Summary     string          `json:"summary,omitempty"`
	Blocker     string          `json:"blocker,omitempty"`
	Output      json.RawMessage `json:"output,omitempty"`
	Cleaned     bool            `json:"cleaned,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
}

// Artifacts contains the explicit outputs and publishable session for a run.
type Artifacts struct {
	Name               string                     `json:"name"`
	Outputs            map[string]json.RawMessage `json:"outputs"`
	PublishableSession string                     `json:"publishable_session,omitempty"`
}

// Definition is one validated immutable workflow graph.
type Definition struct {
	Name           string
	Description    string
	MaxParallelism int
	Steps          map[string]StepDefinition
}

// StepDefinition is one isolated agent session in a workflow graph.
type StepDefinition struct {
	Name              string
	Agent             string
	Prompt            string
	After             []string
	Publishable       bool
	SessionDefinition session.Definition
}

// Summary contains workflow metadata safe to return to clients.
type Summary struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	MaxParallelism int    `json:"max_parallelism"`
	Steps          int    `json:"steps"`
}

// Catalog provides immutable configured workflows.
type Catalog interface {
	List() []Summary
	Find(string) (Definition, bool)
}

// CloneDefinition returns a definition that callers may safely modify.
func CloneDefinition(definition Definition) Definition {
	clone := definition
	clone.Steps = make(map[string]StepDefinition, len(definition.Steps))
	for name, step := range definition.Steps {
		step.After = slices.Clone(step.After)
		step.SessionDefinition.Skills = slices.Clone(step.SessionDefinition.Skills)
		clone.Steps[name] = step
	}

	return clone
}
