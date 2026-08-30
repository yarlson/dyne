package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
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

// Config contains the cluster connection and server-owned session defaults.
type Config struct {
	// Connection selects the Kubernetes cluster.
	Connection Connection
	// Namespace contains every session owned by this control plane.
	Namespace string
	// Image runs setup and coding-agent commands.
	Image string
	// TaskTimeout is the default continuation deadline.
	TaskTimeout time.Duration
}

// StartRequest contains the client-owned inputs for a new agent session.
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
type StartResult struct {
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	TaskID string `json:"task_id"`
}

// ContinueRequest contains the client-owned inputs for another session task.
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

// PublishRequest contains the client-owned inputs for publishing a session.
type PublishRequest struct {
	Name          string
	Branch        string
	BaseBranch    string
	CommitMessage string
	Draft         bool
	Timeout       time.Duration
}

// ErrorKind classifies an operation failure for entrypoints.
type ErrorKind string

const (
	// ErrorInvalid identifies a request that cannot describe a valid operation.
	ErrorInvalid ErrorKind = "invalid"
	// ErrorNotFound identifies a requested product entity that does not exist.
	ErrorNotFound ErrorKind = "not_found"
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

// ErrorKindOf returns the stable classification of an operation error.
func ErrorKindOf(err error) ErrorKind {
	var target *operationError
	if errors.As(err, &target) {
		return target.kind
	}

	return ""
}

type taskIDGenerator func() (string, error)

// Control performs configured agent and session operations for one namespace.
type Control struct {
	sessions  sessionOperations
	catalog   AgentCatalog
	config    Config
	newTaskID taskIDGenerator
}

// Connect returns agent control using the configured Kubernetes connection.
func Connect(
	ctx context.Context,
	config Config,
	output io.Writer,
	repositoryAuth RepositoryTokenProvider,
	catalog AgentCatalog,
) (*Control, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}

	sessions, err := connectSessions(ctx, config.Connection, output, repositoryAuth)
	if err != nil {
		return nil, err
	}

	return newControl(sessions, catalog, config, nil)
}

func newControl(sessions sessionOperations, catalog AgentCatalog, config Config, newTaskID taskIDGenerator) (*Control, error) {
	if sessions == nil {
		return nil, errors.New("session control is required")
	}

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	if newTaskID == nil {
		newTaskID = randomTaskID
	}

	return &Control{sessions: sessions, catalog: catalog, config: config, newTaskID: newTaskID}, nil
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
		return StartResult{}, newOperationError(ErrorNotFound, fmt.Sprintf("agent %s is not configured", request.Agent), nil)
	}

	return c.startDefinition(ctx, definition, request)
}

// StartDefinition creates a workflow-owned session from a snapshotted agent definition.
func (c *Control) StartDefinition(
	ctx context.Context,
	definition AgentDefinition,
	request StartRequest,
) (StartResult, error) {
	request.Agent = definition.Name

	return c.startDefinition(ctx, definition, request)
}

func (c *Control) startDefinition(ctx context.Context, definition AgentDefinition, request StartRequest) (StartResult, error) {
	if request.InitialRef == "" {
		request.InitialRef = "main"
	}

	if request.Timeout == 0 {
		request.Timeout = definition.Timeout
	}

	session := sessionStartRequest{
		target:       c.target(request.Name),
		image:        c.config.Image,
		storage:      definition.Storage,
		repository:   request.Repository,
		initialRef:   request.InitialRef,
		setupCommand: definition.SetupCommand,
		prompt:       request.Prompt,
		agentName:    definition.Name,
		instructions: definition.Instructions,
		skills:       definition.Skills,
		cloneDepth:   definition.CloneDepth,
		storageSize:  definition.StorageSize,
		timeout:      request.Timeout,
		resultKind:   request.ResultKind,
		workflowRun:  request.WorkflowRun,
		workflowStep: request.WorkflowStep,
	}
	if err := session.validate(); err != nil {
		return StartResult{}, newOperationError(ErrorInvalid, err.Error(), err)
	}

	if err := c.sessions.start(ctx, session); err != nil {
		return StartResult{}, newOperationError(ErrorUnavailable, "start session failed", err)
	}

	return StartResult{Agent: definition.Name, Name: request.Name, TaskID: request.Name}, nil
}

// Continue creates another bounded task for a persistent session.
func (c *Control) Continue(ctx context.Context, request ContinueRequest) (TaskResult, error) {
	if strings.TrimSpace(request.Name) == "" {
		return TaskResult{}, newOperationError(ErrorInvalid, "session name is required", nil)
	}

	if strings.TrimSpace(request.Prompt) == "" {
		return TaskResult{}, newOperationError(ErrorInvalid, "prompt is required", nil)
	}

	if request.Timeout == 0 {
		request.Timeout = c.config.TaskTimeout
	}

	if request.Timeout <= 0 {
		return TaskResult{}, newOperationError(ErrorInvalid, "timeout must be greater than zero", nil)
	}

	taskID, err := c.newTaskID()
	if err != nil {
		return TaskResult{}, newOperationError(ErrorUnavailable, "create task identity failed", err)
	}

	continuation := sessionContinueRequest{
		target: c.target(request.Name), taskID: taskID, prompt: request.Prompt, timeout: request.Timeout,
	}
	if err := c.sessions.continueTask(ctx, continuation); err != nil {
		return TaskResult{}, newOperationError(ErrorUnavailable, "continue session failed", err)
	}

	return TaskResult{Name: request.Name, TaskID: taskID}, nil
}

// Status returns the resources owned by one named session.
func (c *Control) Status(ctx context.Context, name string) (Status, error) {
	if err := validateSessionName(name); err != nil {
		return Status{}, err
	}

	status, err := c.sessions.status(ctx, c.target(name))
	if err != nil {
		return Status{}, newOperationError(ErrorUnavailable, "read session status failed", err)
	}

	return status, nil
}

// Artifacts returns the latest task artifacts for one named session.
func (c *Control) Artifacts(ctx context.Context, name string) (Artifacts, error) {
	if err := validateSessionName(name); err != nil {
		return Artifacts{}, err
	}

	artifacts, err := c.sessions.artifacts(ctx, c.target(name))
	if err != nil {
		return Artifacts{}, newOperationError(ErrorUnavailable, "read session artifacts failed", err)
	}

	return artifacts, nil
}

// WriteLogs writes logs from one named session's current attempt.
func (c *Control) WriteLogs(ctx context.Context, name string, follow bool, output io.Writer) error {
	if err := validateSessionName(name); err != nil {
		return err
	}

	if err := c.sessions.writeLogs(ctx, sessionLogRequest{target: c.target(name), follow: follow}, output); err != nil {
		return newOperationError(ErrorUnavailable, "stream session logs failed", err)
	}

	return nil
}

// Delete removes compute resources and retains persistent session state.
func (c *Control) Delete(ctx context.Context, name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}

	if err := c.sessions.delete(ctx, c.target(name)); err != nil {
		return newOperationError(ErrorUnavailable, "delete session failed", err)
	}

	return nil
}

// Destroy removes compute resources and persistent session state.
func (c *Control) Destroy(ctx context.Context, name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}

	if err := c.sessions.destroy(ctx, c.target(name)); err != nil {
		return newOperationError(ErrorUnavailable, "destroy session failed", err)
	}

	return nil
}

// Publish commits a retained session workspace and opens or recovers its pull request.
func (c *Control) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	publishRequest := sessionPublishRequest{
		target:        c.target(request.Name),
		branch:        request.Branch,
		baseBranch:    request.BaseBranch,
		commitMessage: request.CommitMessage,
		draft:         request.Draft,
		timeout:       request.Timeout,
	}
	if err := publishRequest.validate(); err != nil {
		return PublishResult{}, newOperationError(ErrorInvalid, err.Error(), err)
	}

	result, err := c.sessions.publish(ctx, publishRequest)
	if err != nil {
		return PublishResult{}, newOperationError(ErrorUnavailable, "publish session failed", err)
	}

	return result, nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.Namespace) == "" {
		return errors.New("session namespace is required")
	}

	if strings.TrimSpace(config.Image) == "" {
		return errors.New("session image is required")
	}

	if config.TaskTimeout <= 0 {
		return errors.New("task timeout must be greater than zero")
	}

	return nil
}

func (c *Control) findAgent(name string) (AgentDefinition, bool) {
	if c.catalog == nil {
		return AgentDefinition{}, false
	}

	return c.catalog.Find(name)
}

func (c *Control) target(name string) sessionTarget {
	return sessionTarget{namespace: c.config.Namespace, name: name}
}

func validateSessionName(name string) error {
	if strings.TrimSpace(name) == "" {
		return newOperationError(ErrorInvalid, "session name is required", nil)
	}

	return nil
}

func newOperationError(kind ErrorKind, message string, cause error) error {
	return &operationError{kind: kind, message: message, cause: cause}
}

func randomTaskID() (string, error) {
	contents := make([]byte, 6)
	if _, err := rand.Read(contents); err != nil {
		return "", err
	}

	return hex.EncodeToString(contents), nil
}
