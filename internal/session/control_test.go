package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/workload"
)

func TestStartRecordsIntentBeforeProjectingExecution(t *testing.T) {
	var operations []string
	repository := newMemoryRepository()
	repository.create = func(record Record, task Task) {
		operations = append(operations, "record intent")
		assert.Equal(t, "review", record.Name)
		assert.NotEmpty(t, record.IntentID)
		assert.Equal(t, TaskPending, task.State)
	}
	runtime := runtimeStub{start: func(request workload.TaskRequest) error {
		operations = append(operations, "start runtime")
		assert.Equal(t, "installation-token", request.RepositoryCredential)
		assert.Equal(t, "Review correctness.", request.Instructions)

		return nil
	}}
	control := newTestControl(t, repository, runtime, tokenProviderStub("installation-token"))

	result, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)
	assert.Equal(t, StartResult{Agent: "reviewer", Name: "review", TaskID: "review"}, result)
	assert.Equal(t, []string{"record intent", "start runtime"}, operations)
}

func TestStartPersistsAndProjectsWorkflowChangeInput(t *testing.T) {
	repository := newMemoryRepository()
	change := &ChangeInput{
		Session:  "implementation",
		Artifact: ChangeArtifact{SHA256: strings.Repeat("a", 64), Bytes: 123},
	}
	var projected workload.TaskRequest
	control := newTestControl(t, repository, runtimeStub{start: func(request workload.TaskRequest) error {
		projected = request

		return nil
	}}, nil)
	definition := validDefinition()
	definition.Storage = StorageEphemeral
	request := validStartRequest()
	request.WorkflowRun = "change-200"
	request.WorkflowStep = "review"
	request.ResultKind = ResultKindWorkflowOutput
	request.ChangeInput = change

	_, err := control.Start(context.Background(), definition, request)
	require.NoError(t, err)
	stored, err := repository.LatestTask(context.Background(), "review")
	require.NoError(t, err)
	require.NotNil(t, stored.ChangeInput)
	assert.Equal(t, *change, *stored.ChangeInput)
	require.NotNil(t, projected.ChangeInput)
	assert.Equal(t, workload.ChangeInput{
		Session:  "implementation",
		Artifact: workload.ChangeArtifact{SHA256: strings.Repeat("a", 64), Bytes: 123},
	}, *projected.ChangeInput)
}

func TestStartWithSameIntentEnsuresPendingExecutionAfterRestart(t *testing.T) {
	repository := newMemoryRepository()
	firstRuntime := runtimeStub{}
	first := newTestControl(t, repository, firstRuntime, nil)
	_, err := first.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)

	starts := 0
	secondRuntime := runtimeStub{start: func(workload.TaskRequest) error {
		starts++

		return nil
	}}
	second := newTestControl(t, repository, secondRuntime, nil)
	result, err := second.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)
	assert.Equal(t, "review", result.TaskID)
	assert.Equal(t, 1, starts)
}

func TestStartRejectsDifferentIntentBeforeRuntimeMutation(t *testing.T) {
	repository := newMemoryRepository()
	control := newTestControl(t, repository, runtimeStub{}, nil)
	_, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)

	request := validStartRequest()
	request.Prompt = "different work"
	_, err = control.Start(context.Background(), validDefinition(), request)
	assert.Equal(t, ErrorConflict, ErrorKindOf(err))
	assert.ErrorIs(t, err, ErrConflict)
	assert.EqualError(t, err, "session review already exists with different inputs")
}

func TestStatusPersistsValidatedRuntimeResult(t *testing.T) {
	repository := newMemoryRepository()
	runtime := runtimeStub{observe: func(string, string) (workload.TaskObservation, error) {
		return workload.TaskObservation{Phase: workload.TaskSucceeded, Artifacts: workload.TaskArtifacts{
			Outcome:     []byte(`{"status":"completed","summary":"fixed","blocker":""}`),
			PullRequest: []byte(`{"title":"Fix link","body":"Updates the README."}`),
			Change:      &workload.ChangeArtifact{SHA256: strings.Repeat("a", 64), Bytes: 123},
		}}, nil
	}}
	control := newTestControl(t, repository, runtime, nil)
	_, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)

	status, err := control.Status(context.Background(), "review")
	require.NoError(t, err)
	assert.Equal(t, TaskCompleted, status.State)

	restarted := newTestControl(t, repository, runtimeStub{observe: func(string, string) (workload.TaskObservation, error) {
		return workload.TaskObservation{}, errors.New("runtime result was deleted")
	}}, nil)
	artifacts, err := restarted.Artifacts(context.Background(), "review")
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"completed","summary":"fixed","blocker":""}`, string(artifacts.Outcome))
}

func TestStatusRejectsInvalidRetainedChangeMetadata(t *testing.T) {
	repository := newMemoryRepository()
	runtime := runtimeStub{observe: func(string, string) (workload.TaskObservation, error) {
		return workload.TaskObservation{Phase: workload.TaskSucceeded, Artifacts: workload.TaskArtifacts{
			Outcome:     []byte(`{"status":"completed","summary":"fixed","blocker":""}`),
			PullRequest: []byte(`{"title":"Fix link","body":"Updates the README."}`),
			Change:      &workload.ChangeArtifact{SHA256: "invalid", Bytes: 123},
		}}, nil
	}}
	control := newTestControl(t, repository, runtime, nil)
	_, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)

	status, err := control.Status(context.Background(), "review")
	require.NoError(t, err)
	assert.Equal(t, TaskFailed, status.State)
	task, err := repository.LatestTask(context.Background(), "review")
	require.NoError(t, err)
	assert.Contains(t, task.Failure, "validate retained change metadata")
}

func TestContinueUsesDurableDefinitionInsteadOfRuntimeMetadata(t *testing.T) {
	repository := newMemoryRepository()
	control := newTestControl(t, repository, runtimeStub{observe: completedObservation}, nil)
	_, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)
	_, err = control.Status(context.Background(), "review")
	require.NoError(t, err)

	var continuation workload.TaskRequest
	restarted := newTestControl(t, repository, runtimeStub{start: func(request workload.TaskRequest) error {
		continuation = request

		return nil
	}}, nil)
	result, err := restarted.Continue(context.Background(), ContinueRequest{Name: "review", Prompt: "run the tests"})
	require.NoError(t, err)
	assert.Equal(t, "fixed-task", result.TaskID)
	assert.True(t, continuation.Resume)
	assert.Equal(t, "Review correctness.", continuation.Instructions)
	assert.Equal(t, "make tools", continuation.SetupCommand)
}

func TestDeleteKeepsDurableCleanupIntentAfterRuntimeFailure(t *testing.T) {
	repository := newMemoryRepository()
	runtimeFailure := errors.New("cluster unavailable")
	control := newTestControl(t, repository, runtimeStub{delete: func(name string, _ bool) error {
		task, err := repository.LatestTask(context.Background(), name)
		require.NoError(t, err)
		assert.Equal(t, TaskCanceled, task.State)

		return runtimeFailure
	}}, nil)
	_, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)

	err = control.Destroy(context.Background(), "review")
	require.ErrorIs(t, err, runtimeFailure)
	assert.Equal(t, &Deletion{Storage: true}, repository.records["review"].Deletion)
	task, taskErr := repository.LatestTask(context.Background(), "review")
	require.NoError(t, taskErr)
	assert.Equal(t, TaskCanceled, task.State)

	restarted := newTestControl(t, repository, runtimeStub{}, nil)
	require.NoError(t, restarted.ReconcileDeletions(context.Background()))
	_, exists := repository.records["review"]
	assert.False(t, exists)
}

func TestDeletePersistsCompletedTaskBeforeRemovingRuntimeEvidence(t *testing.T) {
	repository := newMemoryRepository()
	runtime := runtimeStub{
		observe: completedObservation,
		delete: func(name string, storage bool) error {
			task, err := repository.LatestTask(context.Background(), name)
			require.NoError(t, err)
			assert.Equal(t, TaskCompleted, task.State)
			assert.JSONEq(t, `{"status":"completed","summary":"fixed","blocker":""}`, string(task.Artifacts.Outcome))
			assert.False(t, storage)

			return nil
		},
	}
	control := newTestControl(t, repository, runtime, nil)
	_, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)

	require.NoError(t, control.Delete(context.Background(), "review"))
	task, err := repository.LatestTask(context.Background(), "review")
	require.NoError(t, err)
	assert.Equal(t, TaskCompleted, task.State)
}

func TestDeleteKeepsRuntimeEvidenceWhenObservationFails(t *testing.T) {
	repository := newMemoryRepository()
	observationFailure := errors.New("cluster unavailable")
	deleted := false
	runtime := runtimeStub{
		observe: func(string, string) (workload.TaskObservation, error) {
			return workload.TaskObservation{}, observationFailure
		},
		delete: func(string, bool) error {
			deleted = true

			return nil
		},
	}
	control := newTestControl(t, repository, runtime, nil)
	_, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)

	err = control.Delete(context.Background(), "review")
	require.ErrorIs(t, err, observationFailure)
	assert.False(t, deleted)
	assert.Equal(t, &Deletion{Storage: false}, repository.records["review"].Deletion)
}

func TestPreparePublicationUsesOnlyDurableSessionState(t *testing.T) {
	repository := newMemoryRepository()
	control := newTestControl(t, repository, runtimeStub{observe: completedObservation}, nil)
	_, err := control.Start(context.Background(), validDefinition(), validStartRequest())
	require.NoError(t, err)

	publication, err := control.PreparePublication(context.Background(), "review")
	require.NoError(t, err)
	defer publication.Close()
	assert.Equal(t, "https://github.com/lokalise/ratchet-test-service", publication.Source.Repository)
	assert.Equal(t, "coding-agent:test", publication.Source.Image)
	assert.JSONEq(t, `{"title":"Fix link","body":"Updates the README."}`, string(publication.Source.PullRequest))
	require.NotNil(t, publication.Source.Change)
	assert.Equal(t, ChangeArtifact{SHA256: strings.Repeat("a", 64), Bytes: 123}, *publication.Source.Change)
}

func completedObservation(string, string) (workload.TaskObservation, error) {
	return workload.TaskObservation{Phase: workload.TaskSucceeded, Artifacts: workload.TaskArtifacts{
		Outcome:     []byte(`{"status":"completed","summary":"fixed","blocker":""}`),
		PullRequest: []byte(`{"title":"Fix link","body":"Updates the README."}`),
		Change:      &workload.ChangeArtifact{SHA256: strings.Repeat("a", 64), Bytes: 123},
	}}, nil
}

func validDefinition() Definition {
	return Definition{
		Agent: "reviewer", Storage: StoragePersistent, Instructions: "Review correctness.",
		SetupCommand: "make tools", CloneDepth: 1, StorageSize: "10Gi", Timeout: time.Hour,
	}
}

func validStartRequest() StartRequest {
	return StartRequest{
		Name: "review", Repository: "https://github.com/lokalise/ratchet-test-service",
		InitialRef: "main", Prompt: "fix the README link", ResultKind: ResultKindPullRequest,
	}
}

func newTestControl(t *testing.T, repository *memoryRepository, runtime runtimeStub, auth RepositoryTokenProvider) *Control {
	t.Helper()
	if runtime.scope == "" {
		runtime.scope = "coding-agents"
	}

	control, err := newControl(Config{
		Repository: repository, Runtime: runtime, RepositoryAuth: auth,
		Image: "coding-agent:test", TaskTimeout: time.Hour,
	}, func() (string, error) { return "fixed-task", nil }, func() time.Time {
		return time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	})
	require.NoError(t, err)

	return control
}

type tokenProviderStub string

func (t tokenProviderStub) InstallationToken(context.Context) (string, error) { return string(t), nil }

type runtimeStub struct {
	scope   string
	start   func(workload.TaskRequest) error
	observe func(string, string) (workload.TaskObservation, error)
	delete  func(string, bool) error
}

func (r runtimeStub) Scope() string { return r.scope }
func (r runtimeStub) Start(_ context.Context, request workload.TaskRequest) error {
	if r.start != nil {
		return r.start(request)
	}

	return nil
}

func (r runtimeStub) Observe(_ context.Context, name, task string) (workload.TaskObservation, error) {
	if r.observe != nil {
		return r.observe(name, task)
	}

	return workload.TaskObservation{Phase: workload.TaskPending}, nil
}

func (r runtimeStub) WriteLogs(context.Context, string, string, bool, io.Writer) error { return nil }
func (r runtimeStub) Delete(_ context.Context, name string, storage bool) error {
	if r.delete != nil {
		return r.delete(name, storage)
	}

	return nil
}

type memoryRepository struct {
	records map[string]Record
	tasks   map[string][]Task
	create  func(Record, Task)
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{records: map[string]Record{}, tasks: map[string][]Task{}}
}

func (r *memoryRepository) Create(_ context.Context, record Record, task Task) error {
	if _, exists := r.records[record.Name]; exists {
		return ErrConflict
	}

	r.records[record.Name] = record
	r.tasks[record.Name] = []Task{task}
	if r.create != nil {
		r.create(record, task)
	}

	return nil
}

func (r *memoryRepository) Get(_ context.Context, name string) (Record, error) {
	record, exists := r.records[name]
	if !exists {
		return Record{}, ErrNotFound
	}

	return record, nil
}

func (r *memoryRepository) AddTask(_ context.Context, name string, task Task) error {
	if _, exists := r.records[name]; !exists {
		return ErrNotFound
	}

	for _, existing := range r.tasks[name] {
		if !taskTerminal(existing.State) {
			return ErrActiveTask
		}
	}

	r.tasks[name] = append(r.tasks[name], task)

	return nil
}

func (r *memoryRepository) LatestTask(_ context.Context, name string) (Task, error) {
	tasks := r.tasks[name]
	if len(tasks) == 0 {
		return Task{}, ErrNotFound
	}

	return tasks[len(tasks)-1], nil
}

func (r *memoryRepository) UpdateTask(_ context.Context, name string, task Task) error {
	tasks := r.tasks[name]
	for i := range tasks {
		if tasks[i].ID == task.ID {
			tasks[i] = task
			r.tasks[name] = tasks

			return nil
		}
	}

	return ErrNotFound
}

func (r *memoryRepository) BeginDeletion(_ context.Context, name string, storage bool) error {
	record, exists := r.records[name]
	if !exists {
		return ErrNotFound
	}

	if record.Deletion != nil && record.Deletion.Storage != storage {
		return ErrConflict
	}

	record.Deletion = &Deletion{Storage: storage}
	r.records[name] = record

	return nil
}

func (r *memoryRepository) FinishDeletion(_ context.Context, name string, remove bool) error {
	record, exists := r.records[name]
	if !exists {
		return ErrNotFound
	}

	if remove {
		delete(r.records, name)
		delete(r.tasks, name)

		return nil
	}

	record.Deletion = nil
	r.records[name] = record

	return nil
}

func (r *memoryRepository) Deleting(context.Context) ([]Record, error) {
	var records []Record
	for _, record := range r.records {
		if record.Deletion != nil {
			records = append(records, record)
		}
	}

	return records, nil
}
