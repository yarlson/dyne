package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yarlson/dyne/internal/workload"
)

const maxAgentConfigBytes = 900 * 1024

var (
	dnsLabelPattern     = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	changeSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ErrorKind classifies a session operation failure for entrypoints.
type ErrorKind string

const (
	// ErrorInvalid identifies an invalid request or lifecycle transition.
	ErrorInvalid ErrorKind = "invalid"
	// ErrorNotFound identifies a session that does not exist.
	ErrorNotFound ErrorKind = "not_found"
	// ErrorConflict identifies an identity owned by different durable intent.
	ErrorConflict ErrorKind = "conflict"
	// ErrorUnavailable identifies a storage or runtime dependency failure.
	ErrorUnavailable ErrorKind = "unavailable"
)

type operationError struct {
	kind    ErrorKind
	message string
	cause   error
}

func (e *operationError) Error() string { return e.message }
func (e *operationError) Unwrap() error { return e.cause }

// ErrorKindOf returns the stable classification of a session operation error.
func ErrorKindOf(err error) ErrorKind {
	var target *operationError
	if errors.As(err, &target) {
		return target.kind
	}

	return ""
}

// Config contains session control dependencies and server-owned defaults.
type Config struct {
	Repository     Repository
	Runtime        Runtime
	RepositoryAuth RepositoryTokenProvider
	Image          string
	TaskTimeout    time.Duration
}

type taskIDGenerator func() (string, error)

// Control coordinates durable session lifecycle operations.
type Control struct {
	repository     Repository
	runtime        Runtime
	repositoryAuth RepositoryTokenProvider
	image          string
	taskTimeout    time.Duration
	newTaskID      taskIDGenerator
	now            func() time.Time
	locksMu        sync.Mutex
	locks          map[string]*sessionLock
}

type sessionLock struct {
	mutex sync.Mutex
	users int
}

// New creates session control backed by durable storage and one runtime.
func New(config Config) (*Control, error) {
	return newControl(config, nil, nil)
}

func newControl(config Config, newTaskID taskIDGenerator, now func() time.Time) (*Control, error) {
	if config.Repository == nil {
		return nil, errors.New("session repository is required")
	}

	if config.Runtime == nil {
		return nil, errors.New("session runtime is required")
	}

	if strings.TrimSpace(config.Image) == "" {
		return nil, errors.New("session image is required")
	}

	if config.TaskTimeout <= 0 {
		return nil, errors.New("task timeout must be greater than zero")
	}

	if newTaskID == nil {
		newTaskID = randomTaskID
	}

	if now == nil {
		now = time.Now
	}

	return &Control{
		repository: config.Repository, runtime: config.Runtime, repositoryAuth: config.RepositoryAuth,
		image: config.Image, taskTimeout: config.TaskTimeout, newTaskID: newTaskID, now: now,
		locks: make(map[string]*sessionLock),
	}, nil
}

// Start durably records a session and ensures its initial runtime execution.
func (c *Control) Start(ctx context.Context, definition Definition, request StartRequest) (StartResult, error) {
	if request.InitialRef == "" {
		request.InitialRef = "main"
	}

	if request.Timeout == 0 {
		request.Timeout = definition.Timeout
	}

	if request.ResultKind == "" {
		request.ResultKind = ResultKindPullRequest
	}

	if err := validateStart(definition, request); err != nil {
		return StartResult{}, newOperationError(ErrorInvalid, err.Error(), err)
	}

	release := c.lock(request.Name)
	defer release()

	record, task := c.initialIntent(definition, request)

	created := true
	if err := c.repository.Create(ctx, record, task); err != nil {
		if !errors.Is(err, ErrConflict) {
			return StartResult{}, newOperationError(ErrorUnavailable, "record session intent failed", err)
		}

		created = false
		record, err = c.repository.Get(ctx, request.Name)
		if err != nil {
			return StartResult{}, newOperationError(ErrorUnavailable, "read existing session failed", err)
		}

		if record.IntentID != taskIntentID(definition, request, c.runtime.Scope(), c.image) {
			return StartResult{}, newOperationError(
				ErrorConflict, fmt.Sprintf("session %s already exists with different inputs", request.Name), ErrConflict,
			)
		}

		task, err = c.repository.LatestTask(ctx, request.Name)
		if err != nil {
			return StartResult{}, newOperationError(ErrorUnavailable, "read existing session task failed", err)
		}
	}

	if created || !taskTerminal(task.State) {
		credential, err := c.repositoryCredential(ctx)
		if err != nil {
			return StartResult{}, newOperationError(ErrorUnavailable, "prepare repository credential failed", err)
		}

		if err := c.runtime.Start(ctx, taskRequest(record, task, false, credential)); err != nil {
			return StartResult{}, newOperationError(ErrorUnavailable, "start session execution failed", err)
		}
	}

	return StartResult{Agent: definition.Agent, Name: request.Name, TaskID: task.ID}, nil
}

// Continue durably records and starts another task for a persistent session.
func (c *Control) Continue(ctx context.Context, request ContinueRequest) (TaskResult, error) {
	if strings.TrimSpace(request.Name) == "" {
		return TaskResult{}, newOperationError(ErrorInvalid, "session name is required", nil)
	}

	if strings.TrimSpace(request.Prompt) == "" {
		return TaskResult{}, newOperationError(ErrorInvalid, "prompt is required", nil)
	}

	if request.Timeout == 0 {
		request.Timeout = c.taskTimeout
	}

	if request.Timeout <= 0 {
		return TaskResult{}, newOperationError(ErrorInvalid, "timeout must be greater than zero", nil)
	}

	release := c.lock(request.Name)
	defer release()

	record, err := c.session(ctx, request.Name)
	if err != nil {
		return TaskResult{}, err
	}

	if record.Definition.Storage != StoragePersistent {
		return TaskResult{}, newOperationError(ErrorInvalid, "ephemeral sessions cannot continue", nil)
	}

	if record.Deletion != nil {
		return TaskResult{}, newOperationError(ErrorConflict, "session deletion is in progress", nil)
	}

	if _, err := c.synchronizeLatestTask(ctx, record); err != nil {
		return TaskResult{}, err
	}

	taskID, err := c.newTaskID()
	if err != nil {
		return TaskResult{}, newOperationError(ErrorUnavailable, "create task identity failed", err)
	}

	task := Task{
		ID: request.Name + "-" + taskID, Prompt: request.Prompt, Timeout: request.Timeout,
		ResultKind: ResultKindPullRequest, State: TaskPending, CreatedAt: c.now().UTC(),
	}
	if err := c.repository.AddTask(ctx, request.Name, task); err != nil {
		if errors.Is(err, ErrActiveTask) {
			return TaskResult{}, newOperationError(ErrorConflict, "session already has an active task", err)
		}

		return TaskResult{}, newOperationError(ErrorUnavailable, "record session task failed", err)
	}

	if err := c.runtime.Start(ctx, taskRequest(record, task, true, "")); err != nil {
		return TaskResult{}, newOperationError(ErrorUnavailable, "start session task failed", err)
	}

	return TaskResult{Name: request.Name, TaskID: taskID}, nil
}

// Status returns the latest durable task state after observing the runtime.
func (c *Control) Status(ctx context.Context, name string) (Status, error) {
	if err := validateName(name); err != nil {
		return Status{}, err
	}

	release := c.lock(name)
	defer release()

	record, err := c.session(ctx, name)
	if err != nil {
		return Status{}, err
	}

	task, err := c.synchronizeLatestTask(ctx, record)
	if err != nil {
		return Status{}, err
	}

	return Status{Name: name, TaskID: task.ID, State: task.State}, nil
}

// Artifacts returns the latest terminal task's durable validated artifacts.
func (c *Control) Artifacts(ctx context.Context, name string) (Artifacts, error) {
	if err := validateName(name); err != nil {
		return Artifacts{}, err
	}

	release := c.lock(name)
	defer release()

	record, err := c.session(ctx, name)
	if err != nil {
		return Artifacts{}, err
	}

	task, err := c.synchronizeLatestTask(ctx, record)
	if err != nil {
		return Artifacts{}, err
	}

	if !taskTerminal(task.State) {
		return Artifacts{}, newOperationError(ErrorConflict, "session task has not finished", nil)
	}

	if len(task.Artifacts.Outcome) == 0 {
		return Artifacts{}, newOperationError(ErrorUnavailable, "session task has no outcome artifact", nil)
	}

	return task.Artifacts, nil
}

// WriteLogs writes logs from the latest task execution.
func (c *Control) WriteLogs(ctx context.Context, name string, follow bool, output io.Writer) error {
	if err := validateName(name); err != nil {
		return err
	}

	task, err := c.repository.LatestTask(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return newOperationError(ErrorNotFound, fmt.Sprintf("session %s does not exist", name), err)
	}

	if err != nil {
		return newOperationError(ErrorUnavailable, "read session task failed", err)
	}

	if err := c.runtime.WriteLogs(ctx, name, task.ID, follow, output); err != nil {
		return newOperationError(ErrorUnavailable, "stream session logs failed", err)
	}

	return nil
}

// Delete removes runtime compute and retains a persistent session's files and SQL record.
func (c *Control) Delete(ctx context.Context, name string) error {
	return c.delete(ctx, name, false)
}

// Destroy removes runtime resources and the durable session record.
func (c *Control) Destroy(ctx context.Context, name string) error {
	return c.delete(ctx, name, true)
}

func (c *Control) delete(ctx context.Context, name string, deleteStorage bool) error {
	if err := validateName(name); err != nil {
		return err
	}

	release := c.lock(name)
	defer release()

	record, err := c.repository.Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	if err != nil {
		return newOperationError(ErrorUnavailable, "read session before deletion failed", err)
	}

	if err := c.repository.BeginDeletion(ctx, name, deleteStorage); err != nil {
		kind := ErrorUnavailable
		if errors.Is(err, ErrConflict) {
			kind = ErrorConflict
		}

		return newOperationError(kind, "record session deletion failed", err)
	}

	if err := c.finishDeletion(ctx, record, deleteStorage); err != nil {
		operation := "delete session failed"
		if deleteStorage {
			operation = "destroy session failed"
		}

		return newOperationError(ErrorUnavailable, operation, err)
	}

	return nil
}

// ReconcileDeletions retries cleanup that was interrupted after its intent became durable.
func (c *Control) ReconcileDeletions(ctx context.Context) error {
	records, err := c.repository.Deleting(ctx)
	if err != nil {
		return fmt.Errorf("read session deletion intents: %w", err)
	}

	var cleanupErrors []error
	for _, record := range records {
		if err := c.finishDeletion(ctx, record, record.Deletion.Storage); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("finish session %s deletion: %w", record.Name, err))
		}
	}

	return errors.Join(cleanupErrors...)
}

func (c *Control) finishDeletion(ctx context.Context, record Record, deleteStorage bool) error {
	if record.RuntimeScope != c.runtime.Scope() {
		return fmt.Errorf("session runtime scope is %q, server runtime scope is %q", record.RuntimeScope, c.runtime.Scope())
	}

	task, err := c.repository.LatestTask(ctx, record.Name)
	if err == nil && !taskTerminal(task.State) {
		task, err = c.synchronizeLatestTask(ctx, record)
	}

	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	if err == nil && !taskTerminal(task.State) {
		now := c.now().UTC()
		task.State = TaskCanceled
		task.FinishedAt = &now
		if err := c.repository.UpdateTask(ctx, record.Name, task); err != nil {
			return err
		}
	}

	if err := c.runtime.Delete(ctx, record.Name, deleteStorage); err != nil {
		return err
	}

	removeRecord := deleteStorage || record.Definition.Storage == StorageEphemeral

	return c.repository.FinishDeletion(ctx, record.Name, removeRecord)
}

// Publication holds an exclusive session lease and its eligible source.
type Publication struct {
	Source  PublicationSource
	release func()
	once    sync.Once
}

// Close releases the exclusive session lease.
func (p *Publication) Close() {
	if p == nil {
		return
	}

	p.once.Do(func() {
		if p.release != nil {
			p.release()
		}
	})
}

// PreparePublication validates durable session state and holds an exclusive lease.
func (c *Control) PreparePublication(ctx context.Context, name string) (*Publication, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}

	release := c.lock(name)
	succeeded := false
	defer func() {
		if !succeeded {
			release()
		}
	}()

	record, err := c.session(ctx, name)
	if err != nil {
		return nil, err
	}

	if record.Deletion != nil {
		return nil, newOperationError(ErrorConflict, "session deletion is in progress", nil)
	}

	task, err := c.synchronizeLatestTask(ctx, record)
	if err != nil {
		return nil, err
	}

	if task.State != TaskCompleted {
		return nil, newOperationError(ErrorInvalid, "latest session task must complete successfully before publishing", nil)
	}

	if record.Definition.Storage != StoragePersistent {
		return nil, newOperationError(ErrorInvalid, "ephemeral sessions cannot be published", nil)
	}

	if len(task.Artifacts.PullRequest) == 0 {
		return nil, newOperationError(ErrorUnavailable, "session task has no pull request artifact", nil)
	}

	succeeded = true

	return &Publication{Source: PublicationSource{
		Repository: record.Repository, InitialRef: record.InitialRef, Image: record.Image,
		PullRequest: task.Artifacts.PullRequest, Change: cloneChangeArtifact(task.Artifacts.Change),
	}, release: release}, nil
}

func (c *Control) initialIntent(definition Definition, request StartRequest) (Record, Task) {
	intentID := taskIntentID(definition, request, c.runtime.Scope(), c.image)
	now := c.now().UTC()

	return Record{
			Name: request.Name, IntentID: intentID, RuntimeScope: c.runtime.Scope(), Image: c.image,
			Definition: definition, Repository: request.Repository, InitialRef: request.InitialRef,
			WorkflowRun: request.WorkflowRun, WorkflowStep: request.WorkflowStep, CreatedAt: now,
		}, Task{
			ID: request.Name, Prompt: request.Prompt, Timeout: request.Timeout, ResultKind: request.ResultKind,
			ChangeInput: cloneChangeInput(request.ChangeInput), State: TaskPending, CreatedAt: now,
		}
}

func (c *Control) session(ctx context.Context, name string) (Record, error) {
	record, err := c.repository.Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return Record{}, newOperationError(ErrorNotFound, fmt.Sprintf("session %s does not exist", name), err)
	}

	if err != nil {
		return Record{}, newOperationError(ErrorUnavailable, "read session failed", err)
	}

	if record.RuntimeScope != c.runtime.Scope() {
		return Record{}, newOperationError(ErrorConflict, "session belongs to a different runtime scope", nil)
	}

	return record, nil
}

func (c *Control) synchronizeLatestTask(ctx context.Context, record Record) (Task, error) {
	task, err := c.repository.LatestTask(ctx, record.Name)
	if err != nil {
		return Task{}, newOperationError(ErrorUnavailable, "read session task failed", err)
	}

	if taskTerminal(task.State) {
		return task, nil
	}

	observation, err := c.runtime.Observe(ctx, record.Name, task.ID)
	if err != nil {
		return Task{}, newOperationError(ErrorUnavailable, "observe session task failed", err)
	}

	if err := applyObservation(&task, observation, record.Definition.Storage, c.now().UTC()); err != nil {
		task.State = TaskFailed
		task.Failure = err.Error()
		now := c.now().UTC()
		task.FinishedAt = &now
	}

	if err := c.repository.UpdateTask(ctx, record.Name, task); err != nil {
		return Task{}, newOperationError(ErrorUnavailable, "record session task observation failed", err)
	}

	return task, nil
}

func applyObservation(task *Task, observation workload.TaskObservation, storage Storage, now time.Time) error {
	switch observation.Phase {
	case workload.TaskPending:
		task.State = TaskPending
		task.Failure = observation.Failure

		return nil
	case workload.TaskRunning:
		task.State = TaskRunning
		task.Failure = observation.Failure

		return nil
	case workload.TaskFailed:
		task.State = TaskFailed
		task.Artifacts = taskArtifacts(observation.Artifacts)
		task.Failure = observation.Failure
		task.FinishedAt = &now

		return nil
	case workload.TaskSucceeded:
	default:
		return fmt.Errorf("runtime returned unsupported task phase %q", observation.Phase)
	}

	var outcome struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(observation.Artifacts.Outcome, &outcome); err != nil {
		return fmt.Errorf("decode outcome artifact: %w", err)
	}

	switch outcome.Status {
	case "completed":
		task.State = TaskCompleted
	case "blocked":
		task.State = TaskBlocked
	case "failed":
		task.State = TaskFailed
	default:
		return fmt.Errorf("unsupported outcome status %q", outcome.Status)
	}

	if task.State == TaskCompleted {
		switch task.ResultKind {
		case ResultKindPullRequest:
			if len(observation.Artifacts.PullRequest) == 0 {
				return errors.New("completed task did not report pull request metadata")
			}

			if storage == StoragePersistent && observation.Artifacts.Change != nil {
				if err := validateChangeArtifact(observation.Artifacts.Change.SHA256, observation.Artifacts.Change.Bytes); err != nil {
					return fmt.Errorf("validate retained change metadata: %w", err)
				}
			}
		case ResultKindWorkflowOutput:
			if len(observation.Artifacts.WorkflowOutput) == 0 {
				return errors.New("completed task did not report workflow output")
			}
		case ResultKindWorkflowChange:
			if len(observation.Artifacts.WorkflowOutput) == 0 {
				return errors.New("completed workflow change task did not report workflow output")
			}

			if observation.Artifacts.Change == nil {
				return errors.New("completed workflow change task did not report retained change metadata")
			}

			if err := validateChangeArtifact(observation.Artifacts.Change.SHA256, observation.Artifacts.Change.Bytes); err != nil {
				return fmt.Errorf("validate retained change metadata: %w", err)
			}
		}
	}

	task.Artifacts = taskArtifacts(observation.Artifacts)
	task.Failure = observation.Failure
	task.FinishedAt = &now

	return nil
}

func taskRequest(record Record, task Task, resume bool, credential string) workload.TaskRequest {
	skills := make([]workload.Skill, len(record.Definition.Skills))
	for i, skill := range record.Definition.Skills {
		skills[i] = workload.Skill{Name: skill.Name, Contents: skill.Contents}
	}

	return workload.TaskRequest{
		SessionName: record.Name, TaskName: task.ID, Image: record.Image,
		Storage: workload.Storage(record.Definition.Storage), Repository: record.Repository,
		InitialRef: record.InitialRef, SetupCommand: record.Definition.SetupCommand, Prompt: task.Prompt,
		AgentName: record.Definition.Agent, Instructions: record.Definition.Instructions, Skills: skills,
		CloneDepth: record.Definition.CloneDepth, StorageSize: record.Definition.StorageSize,
		Timeout: task.Timeout, ResultKind: workload.ResultKind(task.ResultKind),
		WorkflowRun: record.WorkflowRun, WorkflowStep: record.WorkflowStep,
		ChangeInput: workloadChangeInput(task.ChangeInput),
		Resume:      resume, RepositoryCredential: credential,
	}
}

func taskArtifacts(artifacts workload.TaskArtifacts) Artifacts {
	return Artifacts{
		Outcome: artifacts.Outcome, PullRequest: artifacts.PullRequest, WorkflowOutput: artifacts.WorkflowOutput,
		Change: sessionChangeArtifact(artifacts.Change),
	}
}

func validateStart(definition Definition, request StartRequest) error {
	if err := validateDNSLabel("name", request.Name, 40); err != nil {
		return err
	}

	if (request.WorkflowRun == "") != (request.WorkflowStep == "") {
		return errors.New("workflow run and step must be provided together")
	}

	if request.WorkflowRun != "" {
		if err := validateDNSLabel("workflow run", request.WorkflowRun, 40); err != nil {
			return err
		}

		if err := validateDNSLabel("workflow step", request.WorkflowStep, 63); err != nil {
			return err
		}
	}

	if request.ChangeInput != nil {
		if request.WorkflowRun == "" {
			return errors.New("change input requires a workflow-owned session")
		}

		if err := validateDNSLabel("change input session", request.ChangeInput.Session, 40); err != nil {
			return err
		}

		if request.ChangeInput.Session == request.Name {
			return errors.New("change input session must differ from the target session")
		}

		if err := validateChangeArtifact(request.ChangeInput.Artifact.SHA256, request.ChangeInput.Artifact.Bytes); err != nil {
			return fmt.Errorf("change input: %w", err)
		}
	}

	if strings.TrimSpace(request.Repository) == "" {
		return errors.New("repository is required")
	}

	if request.InitialRef == "" {
		return errors.New("ref is required")
	}

	if definition.StorageSize == "" {
		return errors.New("storage size is required")
	}

	if definition.CloneDepth < 0 {
		return errors.New("clone depth cannot be negative")
	}

	if request.Timeout <= 0 {
		return errors.New("timeout must be greater than zero")
	}

	if strings.TrimSpace(request.Prompt) == "" {
		return errors.New("prompt is required")
	}

	if err := validateDefinition(definition); err != nil {
		return err
	}

	switch request.ResultKind {
	case ResultKindPullRequest, ResultKindWorkflowOutput:
	case ResultKindWorkflowChange:
		if request.WorkflowRun == "" {
			return errors.New("workflow change result requires a workflow-owned session")
		}

		if definition.Storage != StoragePersistent {
			return errors.New("workflow change result requires persistent storage")
		}
	default:
		return fmt.Errorf("unsupported result kind %q", request.ResultKind)
	}

	return nil
}

func validateChangeArtifact(sha256 string, bytes int64) error {
	if !changeSHA256Pattern.MatchString(sha256) {
		return errors.New("sha256 must contain 64 lowercase hexadecimal characters")
	}

	if bytes <= 0 {
		return errors.New("bytes must be greater than zero")
	}

	return nil
}

func cloneChangeInput(input *ChangeInput) *ChangeInput {
	if input == nil {
		return nil
	}

	clone := *input

	return &clone
}

func cloneChangeArtifact(artifact *ChangeArtifact) *ChangeArtifact {
	if artifact == nil {
		return nil
	}

	clone := *artifact

	return &clone
}

func workloadChangeInput(input *ChangeInput) *workload.ChangeInput {
	if input == nil {
		return nil
	}

	return &workload.ChangeInput{
		Session:  input.Session,
		Artifact: workload.ChangeArtifact{SHA256: input.Artifact.SHA256, Bytes: input.Artifact.Bytes},
	}
}

func sessionChangeArtifact(artifact *workload.ChangeArtifact) *ChangeArtifact {
	if artifact == nil {
		return nil
	}

	return &ChangeArtifact{SHA256: artifact.SHA256, Bytes: artifact.Bytes}
}

func validateDefinition(definition Definition) error {
	if definition.Agent == "" {
		if definition.Instructions != "" || len(definition.Skills) > 0 {
			return errors.New("agent name is required for instructions and skills")
		}
	} else {
		if err := validateDNSLabel("agent name", definition.Agent, 63); err != nil {
			return err
		}

		if strings.TrimSpace(definition.Instructions) == "" {
			return errors.New("agent instructions are required")
		}

		if err := validateSkills(definition.Skills); err != nil {
			return err
		}

		size := len(definition.Instructions)
		for _, skill := range definition.Skills {
			size += len(skill.Name) + len(skill.Contents)
		}

		if size > maxAgentConfigBytes {
			return fmt.Errorf("instructions and skills exceed %d bytes", maxAgentConfigBytes)
		}
	}

	switch definition.Storage {
	case StorageEphemeral, StoragePersistent:
	default:
		return fmt.Errorf("unsupported storage %q", definition.Storage)
	}

	return nil
}

func validateSkills(skills []Skill) error {
	names := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		if err := validateDNSLabel("skill name", skill.Name, 63); err != nil {
			return err
		}

		if strings.TrimSpace(skill.Contents) == "" {
			return fmt.Errorf("skill %s contents are required", skill.Name)
		}

		if _, exists := names[skill.Name]; exists {
			return fmt.Errorf("skill %s is duplicated", skill.Name)
		}

		names[skill.Name] = struct{}{}
	}

	return nil
}

func taskIntentID(definition Definition, request StartRequest, scope, image string) string {
	contents, _ := json.Marshal(struct {
		Definition Definition
		Request    StartRequest
		Scope      string
		Image      string
	}{Definition: definition, Request: request, Scope: scope, Image: image})
	digest := sha256.Sum256(contents)

	return hex.EncodeToString(digest[:])
}

func (c *Control) repositoryCredential(ctx context.Context) (string, error) {
	if c.repositoryAuth == nil {
		return "", nil
	}

	return c.repositoryAuth.InstallationToken(ctx)
}

func (c *Control) lock(name string) func() {
	c.locksMu.Lock()
	lock := c.locks[name]
	if lock == nil {
		lock = &sessionLock{}
		c.locks[name] = lock
	}

	lock.users++
	c.locksMu.Unlock()
	lock.mutex.Lock()

	return func() {
		lock.mutex.Unlock()
		c.locksMu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(c.locks, name)
		}

		c.locksMu.Unlock()
	}
}

func validateDNSLabel(field, value string, maxLength int) error {
	if !dnsLabelPattern.MatchString(value) || len(value) > maxLength {
		return fmt.Errorf("%s must be a lowercase DNS label no longer than %d characters", field, maxLength)
	}

	return nil
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return newOperationError(ErrorInvalid, "session name is required", nil)
	}

	return nil
}

func newOperationError(kind ErrorKind, message string, cause error) error {
	return &operationError{kind: kind, message: message, cause: cause}
}

func taskTerminal(state TaskState) bool {
	return state == TaskCompleted || state == TaskBlocked || state == TaskFailed || state == TaskCanceled
}

func randomTaskID() (string, error) {
	contents := make([]byte, 6)
	if _, err := rand.Read(contents); err != nil {
		return "", err
	}

	return hex.EncodeToString(contents), nil
}
