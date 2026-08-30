package agent

import "github.com/yarlson/dyne/internal/session"

// Storage controls whether a session retains state after a task finishes.
type Storage = session.Storage

const (
	// StorageEphemeral runs one disposable task with Pod-owned storage.
	StorageEphemeral = session.StorageEphemeral
	// StoragePersistent retains session state on one claim.
	StoragePersistent = session.StoragePersistent
)

// ResultKind selects the artifact required when an agent completes.
type ResultKind = session.ResultKind

const (
	// ResultKindPullRequest requires pull request metadata for completed work.
	ResultKindPullRequest = session.ResultKindPullRequest
	// ResultKindWorkflowOutput requires JSON output for a dependent workflow step.
	ResultKindWorkflowOutput = session.ResultKindWorkflowOutput
)

// AgentSkill contains one instruction-only Codex skill.
type AgentSkill = session.Skill
