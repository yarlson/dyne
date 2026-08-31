package workload

import (
	"errors"
	"time"
)

// Storage selects temporary or retained files for one task.
type Storage string

const (
	// StorageEphemeral uses Pod-owned temporary files.
	StorageEphemeral Storage = "ephemeral"
	// StoragePersistent uses a retained session volume.
	StoragePersistent Storage = "persistent"
)

// ResultKind selects the artifact expected from a successful task execution.
type ResultKind string

const (
	// ResultKindPullRequest expects pull request metadata.
	ResultKindPullRequest ResultKind = "pull-request"
	// ResultKindWorkflowOutput expects a workflow output object.
	ResultKindWorkflowOutput ResultKind = "workflow-output"
)

// Skill is one instruction file projected into a coding task.
type Skill struct {
	Name     string
	Contents string
}

// TaskRequest is the complete disposable projection of one coding task.
type TaskRequest struct {
	SessionName          string
	TaskName             string
	Image                string
	Storage              Storage
	Repository           string
	InitialRef           string
	SetupCommand         string
	Prompt               string
	AgentName            string
	Instructions         string
	Skills               []Skill
	CloneDepth           int
	StorageSize          string
	Timeout              time.Duration
	ResultKind           ResultKind
	WorkflowRun          string
	WorkflowStep         string
	Resume               bool
	RepositoryCredential string
}

// TaskPhase is the observed phase of one disposable task execution.
type TaskPhase string

const (
	// TaskPending means the runtime has accepted the task but has not started it.
	TaskPending TaskPhase = "pending"
	// TaskRunning means the task is executing.
	TaskRunning TaskPhase = "running"
	// TaskSucceeded means the task process exited successfully.
	TaskSucceeded TaskPhase = "succeeded"
	// TaskFailed means the task process failed.
	TaskFailed TaskPhase = "failed"
	// TaskCanceled means the runtime canceled the task.
	TaskCanceled TaskPhase = "canceled"
)

// TaskArtifacts contains raw result files reported by one task execution.
type TaskArtifacts struct {
	Outcome        []byte
	PullRequest    []byte
	WorkflowOutput []byte
}

// TaskObservation is the runtime evidence currently available for one task.
type TaskObservation struct {
	Phase     TaskPhase
	Artifacts TaskArtifacts
	Failure   string
}

// PublishRequest is the complete disposable projection of one publisher execution.
type PublishRequest struct {
	Session              string
	IntentID             string
	Image                string
	Repository           string
	RepositoryCredential string
	BaseRef              string
	Branch               string
	CommitMessage        string
	AuthorName           string
	AuthorEmail          string
	Timeout              time.Duration
}

// PublishResult identifies the branch and commit produced by a publisher execution.
type PublishResult struct {
	Branch    string
	CommitSHA string
}

// ErrExecutionFailed means a disposable publisher execution reached a terminal failure.
var ErrExecutionFailed = errors.New("publisher execution failed")
