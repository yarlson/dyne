package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlStartsSessionFromConfiguredDefinition(t *testing.T) {
	var received sessionStartRequest
	sessions := sessionOperationsStub{startSession: func(_ context.Context, request sessionStartRequest) error {
		received = request

		return nil
	}}
	agents, err := newControl(sessions, agentCatalogStub{definition: AgentDefinition{
		Name:         "implementer",
		Description:  "Implements focused changes.",
		Storage:      StoragePersistent,
		Instructions: "Implement the smallest safe change.",
		Skills:       []AgentSkill{{Name: "code-review", Contents: "review the change"}},
		SetupCommand: "mise install",
		CloneDepth:   1,
		StorageSize:  "20Gi",
		Timeout:      time.Hour,
	}}, Config{Namespace: "coding-agents", Image: "coding-agent:test", TaskTimeout: 2 * time.Hour}, nil)
	require.NoError(t, err)

	result, err := agents.Start(context.Background(), StartRequest{
		Agent:      "implementer",
		Name:       "change-123",
		Repository: "https://github.com/lokalise/kargo.git",
		InitialRef: "main",
		Prompt:     "fix the failed checks",
		Timeout:    30 * time.Minute,
	})
	require.NoError(t, err)

	assert.Equal(t, StartResult{Agent: "implementer", Name: "change-123", TaskID: "change-123"}, result)
	assert.Equal(t, sessionStartRequest{
		target:       sessionTarget{namespace: "coding-agents", name: "change-123"},
		image:        "coding-agent:test",
		storage:      StoragePersistent,
		repository:   "https://github.com/lokalise/kargo.git",
		initialRef:   "main",
		setupCommand: "mise install",
		prompt:       "fix the failed checks",
		agentName:    "implementer",
		instructions: "Implement the smallest safe change.",
		skills:       []AgentSkill{{Name: "code-review", Contents: "review the change"}},
		cloneDepth:   1,
		storageSize:  "20Gi",
		timeout:      30 * time.Minute,
	}, received)
}

func TestControlRejectsUnknownAgentWithoutStartingSession(t *testing.T) {
	sessions := sessionOperationsStub{startSession: func(context.Context, sessionStartRequest) error {
		require.FailNow(t, "started a session for an unknown agent")

		return nil
	}}
	agents, err := newControl(sessions, agentCatalogStub{}, Config{
		Namespace: "coding-agents", Image: "coding-agent:test", TaskTimeout: time.Hour,
	}, nil)
	require.NoError(t, err)

	_, err = agents.Start(context.Background(), StartRequest{Agent: "missing", Name: "change-123"})
	require.Error(t, err)
	assert.Equal(t, ErrorNotFound, ErrorKindOf(err))
	assert.EqualError(t, err, "agent missing is not configured")
}

func TestControlHidesSessionIntegrationFailureAndRetainsCause(t *testing.T) {
	cause := errors.New("list Jobs: private cluster detail")
	sessions := sessionOperationsStub{startSession: func(context.Context, sessionStartRequest) error { return cause }}
	agents, err := newControl(sessions, agentCatalogStub{definition: AgentDefinition{
		Name: "implementer", Storage: StorageEphemeral, Instructions: "Implement the task.", StorageSize: "10Gi", Timeout: time.Hour,
	}}, Config{Namespace: "coding-agents", Image: "coding-agent:test", TaskTimeout: time.Hour}, nil)
	require.NoError(t, err)

	_, err = agents.Start(context.Background(), StartRequest{
		Agent: "implementer", Name: "change-123", InitialRef: "main", Prompt: "fix the failed checks",
	})

	assert.Equal(t, ErrorUnavailable, ErrorKindOf(err))
	assert.EqualError(t, err, "start session failed")
	assert.ErrorIs(t, err, cause)
}

func TestControlContinuesSessionWithServerOwnedTaskIdentityAndDefaults(t *testing.T) {
	var received sessionContinueRequest
	sessions := sessionOperationsStub{continueSession: func(_ context.Context, request sessionContinueRequest) error {
		received = request

		return nil
	}}
	agents, err := newControl(sessions, agentCatalogStub{}, Config{
		Namespace: "coding-agents", Image: "coding-agent:test", TaskTimeout: 2 * time.Hour,
	}, func() (string, error) { return "abc123", nil })
	require.NoError(t, err)

	result, err := agents.Continue(context.Background(), ContinueRequest{Name: "change-123", Prompt: "continue the fix"})
	require.NoError(t, err)

	assert.Equal(t, TaskResult{Name: "change-123", TaskID: "abc123"}, result)
	assert.Equal(t, sessionContinueRequest{
		target:  sessionTarget{namespace: "coding-agents", name: "change-123"},
		taskID:  "abc123",
		prompt:  "continue the fix",
		timeout: 2 * time.Hour,
	}, received)
}

type agentCatalogStub struct {
	definition AgentDefinition
}

type sessionOperationsStub struct {
	sessionOperations
	startSession    func(context.Context, sessionStartRequest) error
	continueSession func(context.Context, sessionContinueRequest) error
}

func (s sessionOperationsStub) start(ctx context.Context, request sessionStartRequest) error {
	return s.startSession(ctx, request)
}

func (s sessionOperationsStub) continueTask(ctx context.Context, request sessionContinueRequest) error {
	return s.continueSession(ctx, request)
}

func (c agentCatalogStub) List() []AgentSummary {
	if c.definition.Name == "" {
		return []AgentSummary{}
	}

	return []AgentSummary{{Name: c.definition.Name, Description: c.definition.Description, Storage: c.definition.Storage}}
}

func (c agentCatalogStub) Find(name string) (AgentDefinition, bool) {
	return c.definition, c.definition.Name == name
}
