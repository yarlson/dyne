package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/agent"
)

type memoryRepository struct {
	runs    map[string]Run
	agents  map[string]agent.AgentDefinition
	version int64
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{runs: map[string]Run{}, agents: map[string]agent.AgentDefinition{}}
}

func (s *memoryRepository) Create(_ context.Context, run Run, agents map[string]agent.AgentDefinition) (Run, error) {
	if _, found := s.runs[run.Name]; found {
		return Run{}, ErrConcurrentUpdate
	}

	s.version++
	run.Revision = s.version
	s.runs[run.Name] = cloneRun(run)
	for name, definition := range agents {
		definition.Skills = slices.Clone(definition.Skills)
		s.agents[run.Name+"/"+name] = definition
	}

	return cloneRun(run), nil
}

func (s *memoryRepository) Update(_ context.Context, run Run) (Run, error) {
	current, found := s.runs[run.Name]
	if !found {
		return Run{}, ErrRunNotFound
	}

	if current.Revision != run.Revision {
		return Run{}, ErrConcurrentUpdate
	}

	s.version++
	run.Revision = s.version
	s.runs[run.Name] = cloneRun(run)

	return cloneRun(run), nil
}

func (s *memoryRepository) Run(_ context.Context, name string) (Run, error) {
	run, found := s.runs[name]
	if !found {
		return Run{}, ErrRunNotFound
	}

	return cloneRun(run), nil
}

func (s *memoryRepository) Runs(context.Context) ([]Run, error) {
	runs := make([]Run, 0, len(s.runs))
	for _, run := range s.runs {
		runs = append(runs, cloneRun(run))
	}

	return runs, nil
}

func (s *memoryRepository) Agent(_ context.Context, run, name string) (agent.AgentDefinition, error) {
	definition, found := s.agents[run+"/"+name]
	if !found {
		return agent.AgentDefinition{}, ErrRunNotFound
	}

	definition.Skills = slices.Clone(definition.Skills)

	return definition, nil
}

func (s *memoryRepository) Delete(_ context.Context, name string) error {
	delete(s.runs, name)
	for key := range s.agents {
		if len(key) > len(name) && key[:len(name)+1] == name+"/" {
			delete(s.agents, key)
		}
	}

	return nil
}

func cloneRun(run Run) Run {
	clone := run
	clone.Steps = make(map[string]Step, len(run.Steps))
	for name, step := range run.Steps {
		step.After = slices.Clone(step.After)
		step.Output = slices.Clone(step.Output)
		clone.Steps[name] = step
	}

	return clone
}

type fakeSessions struct {
	starts    []agent.StartRequest
	statuses  map[string]agent.Status
	artifacts map[string]agent.Artifacts
	deleted   []string
	destroyed []string
}

func (s *fakeSessions) StartDefinition(_ context.Context, _ agent.AgentDefinition, request agent.StartRequest) (agent.StartResult, error) {
	s.starts = append(s.starts, request)

	return agent.StartResult{Name: request.Name, Agent: request.Agent, TaskID: request.Name}, nil
}

func (s *fakeSessions) Status(_ context.Context, name string) (agent.Status, error) {
	return s.statuses[name], nil
}

func (s *fakeSessions) Artifacts(_ context.Context, name string) (agent.Artifacts, error) {
	return s.artifacts[name], nil
}

func (s *fakeSessions) Delete(_ context.Context, name string) error {
	s.deleted = append(s.deleted, name)

	return nil
}

func (s *fakeSessions) Destroy(_ context.Context, name string) error {
	s.destroyed = append(s.destroyed, name)

	return nil
}

type definitionCatalog map[string]Definition

func (c definitionCatalog) List() []Summary { return nil }

func (c definitionCatalog) Find(name string) (Definition, bool) {
	definition, found := c[name]

	return CloneDefinition(definition), found
}

func TestReconcileRunsIndependentStepsThenPassesOutputsToDependentStep(t *testing.T) {
	repository := newMemoryRepository()
	sessions := &fakeSessions{statuses: map[string]agent.Status{}, artifacts: map[string]agent.Artifacts{}}
	control := newControl(repository, sessions, testCatalog(), func() time.Time { return time.Unix(100, 0).UTC() })

	_, err := control.Start(context.Background(), StartRequest{
		Workflow: "delivery", Name: "change-123", Repository: "https://github.com/example/service.git", Ref: "main", Prompt: "Fix the parser.",
	})
	require.NoError(t, err)
	require.NoError(t, control.Reconcile(context.Background(), "change-123"))
	require.NoError(t, control.Reconcile(context.Background(), "change-123"))
	require.Len(t, sessions.starts, 2)
	assert.ElementsMatch(t, []string{"security", "tests"}, []string{sessions.starts[0].WorkflowStep, sessions.starts[1].WorkflowStep})

	for _, start := range sessions.starts {
		sessions.statuses[start.Name] = completedStatus(start.Name)
		sessions.artifacts[start.Name] = agent.Artifacts{
			Outcome:        json.RawMessage(`{"status":"completed","summary":"done","blocker":""}`),
			WorkflowOutput: json.RawMessage(fmt.Sprintf(`{"source":%q}`, start.WorkflowStep)),
		}
	}

	require.NoError(t, control.Reconcile(context.Background(), "change-123"))
	require.NoError(t, control.Reconcile(context.Background(), "change-123"))
	require.Len(t, sessions.starts, 3)

	implementation := sessions.starts[2]
	assert.Equal(t, "implement", implementation.WorkflowStep)
	assert.Contains(t, implementation.Prompt, `"security":{"source":"security"}`)
	assert.Contains(t, implementation.Prompt, `"tests":{"source":"tests"}`)
	assert.Equal(t, agent.ResultKindPullRequest, implementation.ResultKind)
}

func TestReconcileSkipsDescendantsAndCompletesIndependentBranch(t *testing.T) {
	repository := newMemoryRepository()
	sessions := &fakeSessions{statuses: map[string]agent.Status{}, artifacts: map[string]agent.Artifacts{}}
	control := newControl(repository, sessions, testCatalog(), time.Now)
	_, err := control.Start(context.Background(), StartRequest{
		Workflow: "delivery", Name: "change-124", Repository: "https://github.com/example/service.git", Ref: "main", Prompt: "Fix it.",
	})
	require.NoError(t, err)
	require.NoError(t, control.Reconcile(context.Background(), "change-124"))
	require.NoError(t, control.Reconcile(context.Background(), "change-124"))

	for _, start := range sessions.starts {
		sessions.statuses[start.Name] = completedStatus(start.Name)
		if start.WorkflowStep == "security" {
			sessions.artifacts[start.Name] = agent.Artifacts{Outcome: json.RawMessage(`{"status":"blocked","summary":"needs policy","blocker":"policy missing"}`)}
		} else {
			sessions.artifacts[start.Name] = agent.Artifacts{
				Outcome:        json.RawMessage(`{"status":"completed","summary":"covered","blocker":""}`),
				WorkflowOutput: json.RawMessage(`{"tests":"covered"}`),
			}
		}
	}

	require.NoError(t, control.Reconcile(context.Background(), "change-124"))
	run, err := control.Get(context.Background(), "change-124")
	require.NoError(t, err)
	assert.Equal(t, StateBlocked, run.State)
	assert.Equal(t, StepSkipped, run.Steps["implement"].State)
	assert.Equal(t, StepCompleted, run.Steps["tests"].State)
}

func TestReconcileRecoversPersistedStartingStep(t *testing.T) {
	repository := newMemoryRepository()
	sessions := &fakeSessions{statuses: map[string]agent.Status{}, artifacts: map[string]agent.Artifacts{}}
	control := newControl(repository, sessions, testCatalog(), time.Now)
	_, err := control.Start(context.Background(), StartRequest{
		Workflow: "delivery", Name: "change-125", Repository: "https://github.com/example/service.git", Ref: "main", Prompt: "Fix it.",
	})
	require.NoError(t, err)
	require.NoError(t, control.Reconcile(context.Background(), "change-125"))

	restarted := newControl(repository, sessions, nil, time.Now)
	require.NoError(t, restarted.Reconcile(context.Background(), "change-125"))
	assert.Len(t, sessions.starts, 2)
}

func TestCancelStopsSchedulingAndDeleteDestroysEverySession(t *testing.T) {
	repository := newMemoryRepository()
	sessions := &fakeSessions{statuses: map[string]agent.Status{}, artifacts: map[string]agent.Artifacts{}}
	control := newControl(repository, sessions, testCatalog(), time.Now)
	_, err := control.Start(context.Background(), StartRequest{
		Workflow: "delivery", Name: "change-126", Repository: "https://github.com/example/service.git", Ref: "main", Prompt: "Fix it.",
	})
	require.NoError(t, err)
	require.NoError(t, control.Reconcile(context.Background(), "change-126"))
	require.NoError(t, control.Cancel(context.Background(), "change-126"))
	require.NoError(t, control.Reconcile(context.Background(), "change-126"))

	run, err := control.Get(context.Background(), "change-126")
	require.NoError(t, err)
	assert.Equal(t, StateCanceled, run.State)
	assert.Len(t, sessions.deleted, 2)

	require.NoError(t, control.Delete(context.Background(), "change-126"))
	assert.Len(t, sessions.destroyed, 3)
	_, err = control.Get(context.Background(), "change-126")
	assert.Error(t, err)
}

func completedStatus(name string) agent.Status {
	return agent.Status{Resources: []agent.ResourceStatus{{Kind: "Job", Name: name, State: "Complete"}}}
}

func testCatalog() definitionCatalog {
	reviewer := agent.AgentDefinition{Name: "reviewer", Storage: agent.StorageEphemeral, Timeout: time.Hour}
	implementer := agent.AgentDefinition{Name: "implementer", Storage: agent.StoragePersistent, Timeout: time.Hour}

	return definitionCatalog{"delivery": {
		Name: "delivery", Description: "Review then implement.", MaxParallelism: 2,
		Steps: map[string]StepDefinition{
			"security":  {Name: "security", Agent: "reviewer", Prompt: "Review security.", ResolvedAgent: reviewer},
			"tests":     {Name: "tests", Agent: "reviewer", Prompt: "Review tests.", ResolvedAgent: reviewer},
			"implement": {Name: "implement", Agent: "implementer", Prompt: "Implement.", After: []string{"security", "tests"}, Publishable: true, ResolvedAgent: implementer},
		},
	}}
}
