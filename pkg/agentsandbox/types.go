package agentsandbox

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/yarlson/airlock/internal/sessionmanifest"
)

const (
	// DefaultNamespace is the namespace used when a caller does not specify one.
	DefaultNamespace = sessionmanifest.DefaultNamespace
	// DefaultImage is the coding-session image used when a caller does not specify one.
	DefaultImage = sessionmanifest.DefaultImage
)

// Storage controls whether a session retains state after a task finishes.
type Storage string

const (
	// StorageEphemeral runs one disposable task with Pod-owned storage.
	StorageEphemeral Storage = "ephemeral"
	// StoragePersistent retains session state on one claim.
	StoragePersistent Storage = "persistent"
)

// Streams connects session output and interactive commands to a caller.
type Streams struct {
	// Input supplies interactive command input.
	Input io.Reader
	// Output receives command output and progress.
	Output io.Writer
	// ErrorOutput receives command error output.
	ErrorOutput io.Writer
}

// Connection selects how the control plane authenticates to its Kubernetes cluster.
type Connection struct {
	// KubeconfigPath selects an explicit kubeconfig file.
	KubeconfigPath string
	// ContextName selects a context from kubeconfig.
	ContextName string
	// EKSCluster selects an Amazon EKS cluster by name.
	EKSCluster string
	// AWSRegion overrides the AWS SDK region for EKS discovery.
	AWSRegion string
	// AWSRoleARN is assumed before EKS discovery and authentication.
	AWSRoleARN string
}

// RepositoryTokenProvider returns short-lived credentials for clone and publish workloads.
type RepositoryTokenProvider interface {
	InstallationToken(context.Context) (string, error)
}

// Target identifies one coding session.
type Target struct {
	// Namespace owns the session.
	Namespace string
	// Name identifies the session within its namespace.
	Name string
}

// AgentSkill contains one instruction-only Codex skill.
type AgentSkill struct {
	// Name identifies the skill in Codex.
	Name string
	// Contents is the complete SKILL.md file.
	Contents string
}

// StartRequest defines a new coding session.
type StartRequest struct {
	// Target identifies the session.
	Target Target
	// Image runs setup and coding-agent commands.
	Image string
	// Storage selects whether the session state is temporary or persistent.
	Storage Storage
	// Repository is cloned into the workspace; an empty value initializes a repository.
	Repository string
	// InitialRef is the Git branch or tag cloned for the session.
	InitialRef string
	// SetupCommand runs after the workspace is prepared and before agent work begins.
	SetupCommand string
	// Prompt is the initial task for an explore or update session.
	Prompt string
	// AgentName identifies the reusable agent template used to start the session.
	AgentName string
	// Instructions are additional Codex developer instructions.
	Instructions string
	// Skills are the instruction-only Codex skills available to the agent.
	Skills []AgentSkill
	// CloneDepth limits fetched Git history; zero fetches full history.
	CloneDepth int
	// StorageSize is the requested size of each persistent workspace claim.
	StorageSize string
	// Timeout bounds a bounded session.
	Timeout time.Duration
}

// ContinueRequest defines one Job against a retained session.
type ContinueRequest struct {
	// Target identifies the persistent session.
	Target Target
	// TaskID distinguishes this task from earlier session Jobs.
	TaskID string
	// Prompt is the next task given to the coding agent.
	Prompt string
	// Timeout bounds the task Job.
	Timeout time.Duration
}

// Status describes the resources owned by one coding session.
type Status struct {
	// Resources lists session resources in display order.
	Resources []ResourceStatus
}

// Artifacts contains the latest task's validated result files.
type Artifacts struct {
	// Outcome is the task's completed, blocked, or failed result.
	Outcome json.RawMessage `json:"outcome"`
	// PullRequest is the proposed pull request metadata when work completed.
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
}

// ResourceStatus describes the readiness and state of one session resource.
type ResourceStatus struct {
	// Kind identifies the resource type.
	Kind string `json:"kind"`
	// Name identifies the resource.
	Name string `json:"name"`
	// Ready reports current readiness against the desired count.
	Ready string `json:"ready"`
	// State reports the resource lifecycle state.
	State string `json:"state"`
}

// LogRequest selects the session logs to stream.
type LogRequest struct {
	// Target identifies the session.
	Target Target
	// Follow keeps the log stream open for new output.
	Follow bool
}

// PublishRequest defines one idempotent session publication.
type PublishRequest struct {
	// Target identifies the source session.
	Target Target
	// Branch is the new remote branch that receives the changes.
	Branch string
	// BaseBranch is the pull request target and defaults to the session's initial ref.
	BaseBranch string
	// CommitMessage is the message used for the workspace commit.
	CommitMessage string
	// Draft controls whether GitHub creates a draft pull request.
	Draft bool
	// Timeout bounds the publishing operation.
	Timeout time.Duration
}

// PublishResult identifies the branch, commit, and pull request created by publishing.
type PublishResult struct {
	// PullRequestNumber is the repository-local pull request number.
	PullRequestNumber int `json:"pull_request_number"`
	// PullRequestURL is the pull request's GitHub web URL.
	PullRequestURL string `json:"pull_request_url"`
	// Branch is the remote branch containing the published changes.
	Branch string `json:"branch"`
	// CommitSHA is the published commit, or empty when an existing pull request is recovered.
	CommitSHA string `json:"commit_sha"`
}
