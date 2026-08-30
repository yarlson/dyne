package agent

import (
	"context"
	"encoding/json"
)

const (
	// DefaultNamespace is the namespace used when a caller does not specify one.
	DefaultNamespace = "coding-agents"
	// DefaultImage is the coding-session image used when a caller does not specify one.
	DefaultImage = "coding-agent:local"
)

// Storage controls whether a session retains state after a task finishes.
type Storage string

const (
	// StorageEphemeral runs one disposable task with Pod-owned storage.
	StorageEphemeral Storage = "ephemeral"
	// StoragePersistent retains session state on one claim.
	StoragePersistent Storage = "persistent"
)

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

// AgentSkill contains one instruction-only Codex skill.
type AgentSkill struct {
	// Name identifies the skill in Codex.
	Name string
	// Contents is the complete SKILL.md file.
	Contents string
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
