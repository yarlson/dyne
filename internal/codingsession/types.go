package codingsession

import (
	"io"
	"time"

	"coding-agent-k8s/internal/sessionmanifest"
)

const (
	// DefaultNamespace is the namespace used when a caller does not specify one.
	DefaultNamespace = sessionmanifest.DefaultNamespace
	// DefaultImage is the coding-session image used when a caller does not specify one.
	DefaultImage = sessionmanifest.DefaultImage
)

// Mode controls a session's execution and storage lifecycle.
type Mode string

const (
	// ModeExplore runs one bounded task without retaining its workspace.
	ModeExplore Mode = "explore"
	// ModeUpdate runs one bounded task and retains its workspace.
	ModeUpdate Mode = "update"
	// ModeLong runs a resumable session that accepts tasks separately.
	ModeLong Mode = "long"
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

// Target identifies one coding session.
type Target struct {
	// Namespace owns the session.
	Namespace string
	// Name identifies the session within its namespace.
	Name string
}

// BootstrapRequest defines the shared environment and credentials for coding sessions.
type BootstrapRequest struct {
	// Namespace is the namespace prepared for coding sessions.
	Namespace string
	// CodexAuthJSON contains an existing Codex CLI login; nil omits the login.
	CodexAuthJSON []byte
	// CodexAPIKey contains the API key used by Codex.
	CodexAPIKey string
	// GitHubToken contains the token used to clone private repositories and publish changes.
	GitHubToken string
}

// StartRequest defines a new coding session.
type StartRequest struct {
	// Target identifies the session.
	Target Target
	// Image runs setup and coding-agent commands.
	Image string
	// Mode selects the session lifecycle and storage behavior.
	Mode Mode
	// Repository is cloned into the workspace; an empty value initializes a repository.
	Repository string
	// InitialRef is the Git branch or tag cloned for the session.
	InitialRef string
	// SetupCommand runs after the workspace is prepared and before agent work begins.
	SetupCommand string
	// Prompt is the initial task for an explore or update session.
	Prompt string
	// CloneDepth limits fetched Git history; zero fetches full history.
	CloneDepth int
	// StorageSize is the requested size of each persistent workspace claim.
	StorageSize string
	// Timeout bounds a bounded session.
	Timeout time.Duration
}

// Status describes the resources owned by one coding session.
type Status struct {
	// Resources lists session resources in display order.
	Resources []ResourceStatus
}

// ResourceStatus describes the readiness and state of one session resource.
type ResourceStatus struct {
	// Kind identifies the resource type.
	Kind string
	// Name identifies the resource.
	Name string
	// Ready reports current readiness against the desired count.
	Ready string
	// State reports the resource lifecycle state.
	State string
}

// LogRequest selects the session logs to stream.
type LogRequest struct {
	// Target identifies the session.
	Target Target
	// Follow keeps the log stream open for new output.
	Follow bool
}

// TaskRequest defines one task submitted to an existing session.
type TaskRequest struct {
	// Target identifies the session.
	Target Target
	// Prompt is the task given to the coding agent.
	Prompt string
	// ResumeLast continues the most recent Codex thread.
	ResumeLast bool
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
	// Title is the pull request title.
	Title string
	// Body is the pull request description.
	Body string
	// Draft controls whether GitHub creates a draft pull request.
	Draft bool
	// Timeout bounds the publishing operation.
	Timeout time.Duration
}

// PublishResult identifies the branch, commit, and pull request created by publishing.
type PublishResult struct {
	// PullRequestNumber is the repository-local pull request number.
	PullRequestNumber int
	// PullRequestURL is the pull request's GitHub web URL.
	PullRequestURL string
	// Branch is the remote branch containing the published changes.
	Branch string
	// CommitSHA is the published commit, or empty when an existing pull request is recovered.
	CommitSHA string
}
