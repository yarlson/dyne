package session

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/yarlson/dyne/internal/workload"
)

// Storage controls whether a session retains files after a task finishes.
type Storage string

const (
	// StorageEphemeral runs one disposable task with Pod-owned storage.
	StorageEphemeral Storage = "ephemeral"
	// StoragePersistent retains session files on one claim.
	StoragePersistent Storage = "persistent"
)

// ResultKind selects the artifact required when a session task completes.
type ResultKind string

const (
	// ResultKindPullRequest requires pull request metadata for completed work.
	ResultKindPullRequest ResultKind = "pull-request"
	// ResultKindWorkflowOutput requires JSON output for a dependent workflow step.
	ResultKindWorkflowOutput ResultKind = "workflow-output"
)

// TaskState is the durable lifecycle state of one bounded coding task.
type TaskState string

const (
	// TaskPending means the task intent is durable but execution has not been observed.
	TaskPending TaskState = "pending"
	// TaskRunning means the runtime reports an active execution.
	TaskRunning TaskState = "running"
	// TaskCompleted means the agent completed the requested work.
	TaskCompleted TaskState = "completed"
	// TaskBlocked means the agent stopped with a documented blocker.
	TaskBlocked TaskState = "blocked"
	// TaskFailed means the runtime or agent failed.
	TaskFailed TaskState = "failed"
	// TaskCanceled means the task was deleted before completion.
	TaskCanceled TaskState = "canceled"
)

// Skill contains one instruction-only Codex skill.
type Skill struct {
	Name     string
	Contents string
}

// Definition contains immutable configuration for one session.
type Definition struct {
	Agent        string
	Storage      Storage
	Instructions string
	Skills       []Skill
	SetupCommand string
	CloneDepth   int
	StorageSize  string
	Timeout      time.Duration
}

// StartRequest contains instance inputs for a new session.
type StartRequest struct {
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
type StartResult struct {
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	TaskID string `json:"task_id"`
}

// ContinueRequest contains client inputs for another session task.
type ContinueRequest struct {
	Name    string
	Prompt  string
	Timeout time.Duration
}

// TaskResult identifies an accepted continuation task.
type TaskResult struct {
	Name   string `json:"name"`
	TaskID string `json:"task_id"`
}

// Status describes the latest durable task for one session.
type Status struct {
	Name   string    `json:"name"`
	TaskID string    `json:"task_id"`
	State  TaskState `json:"state"`
}

// Artifacts contains one task's validated result files.
type Artifacts struct {
	Outcome        json.RawMessage `json:"outcome"`
	PullRequest    json.RawMessage `json:"pull_request,omitempty"`
	WorkflowOutput json.RawMessage `json:"workflow_output,omitempty"`
}

// Record is the durable identity and immutable definition of one session.
type Record struct {
	Name         string
	IntentID     string
	RuntimeScope string
	Image        string
	Definition   Definition
	Repository   string
	InitialRef   string
	WorkflowRun  string
	WorkflowStep string
	Deletion     *Deletion
	CreatedAt    time.Time
}

// Deletion records cleanup that must finish before the session record changes.
type Deletion struct {
	Storage bool
}

// Task is one durable bounded execution within a session.
type Task struct {
	ID         string
	Prompt     string
	Timeout    time.Duration
	ResultKind ResultKind
	State      TaskState
	Artifacts  Artifacts
	Failure    string
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// PublicationSource is the durable eligible source for one publish operation.
type PublicationSource struct {
	Repository  string
	InitialRef  string
	Image       string
	PullRequest json.RawMessage
}

// RepositoryTokenProvider returns short-lived repository credentials.
type RepositoryTokenProvider interface {
	InstallationToken(context.Context) (string, error)
}

// Runtime executes disposable session operations in one concrete environment.
type Runtime interface {
	Scope() string
	Start(context.Context, workload.TaskRequest) error
	Observe(context.Context, string, string) (workload.TaskObservation, error)
	WriteLogs(context.Context, string, string, bool, io.Writer) error
	Delete(context.Context, string, bool) error
}
