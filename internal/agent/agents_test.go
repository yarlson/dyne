package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/session"
)

func TestControlStartsSessionFromConfiguredDefinition(t *testing.T) {
	var receivedDefinition session.Definition
	var receivedRequest session.StartRequest
	sessions := sessionStarterStub{start: func(
		_ context.Context, definition session.Definition, request session.StartRequest,
	) (session.StartResult, error) {
		receivedDefinition = definition
		receivedRequest = request

		return session.StartResult{Agent: definition.Agent, Name: request.Name, TaskID: request.Name}, nil
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
	}})
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
	assert.Equal(t, session.Definition{
		Agent: "implementer", Storage: session.StoragePersistent,
		Instructions: "Implement the smallest safe change.",
		Skills:       []session.Skill{{Name: "code-review", Contents: "review the change"}},
		SetupCommand: "mise install", CloneDepth: 1, StorageSize: "20Gi", Timeout: time.Hour,
	}, receivedDefinition)
	assert.Equal(t, session.StartRequest{
		Name: "change-123", Repository: "https://github.com/lokalise/kargo.git",
		InitialRef: "main", Prompt: "fix the failed checks", Timeout: 30 * time.Minute,
	}, receivedRequest)
}

func TestControlRejectsUnknownAgentWithoutStartingSession(t *testing.T) {
	sessions := sessionStarterStub{start: func(
		context.Context, session.Definition, session.StartRequest,
	) (session.StartResult, error) {
		require.FailNow(t, "started a session for an unknown agent")

		return session.StartResult{}, nil
	}}
	agents, err := newControl(sessions, agentCatalogStub{})
	require.NoError(t, err)

	_, err = agents.Start(context.Background(), StartRequest{Agent: "missing", Name: "change-123"})
	require.Error(t, err)
	assert.Equal(t, ErrorNotFound, ErrorKindOf(err))
	assert.EqualError(t, err, "agent missing is not configured")
}

func TestControlHidesSessionFailureAndRetainsCause(t *testing.T) {
	cause := errors.New("list Jobs: private cluster detail")
	sessions := sessionStarterStub{start: func(
		context.Context, session.Definition, session.StartRequest,
	) (session.StartResult, error) {
		return session.StartResult{}, cause
	}}
	agents, err := newControl(sessions, agentCatalogStub{definition: AgentDefinition{Name: "implementer"}})
	require.NoError(t, err)

	_, err = agents.Start(context.Background(), StartRequest{Agent: "implementer", Name: "change-123"})
	assert.Equal(t, ErrorUnavailable, ErrorKindOf(err))
	assert.EqualError(t, err, "start session failed")
	assert.ErrorIs(t, err, cause)
}

func TestControlPreservesSessionIntentConflict(t *testing.T) {
	conflict := fmt.Errorf("session change-123 already exists with different inputs: %w", session.ErrConflict)
	sessions := sessionStarterStub{start: func(
		context.Context, session.Definition, session.StartRequest,
	) (session.StartResult, error) {
		return session.StartResult{}, conflict
	}}
	agents, err := newControl(sessions, agentCatalogStub{definition: AgentDefinition{
		Name: "implementer", Storage: StoragePersistent, Instructions: "Implement the change.",
		StorageSize: "10Gi", Timeout: time.Hour,
	}})
	require.NoError(t, err)

	_, err = agents.Start(context.Background(), StartRequest{
		Agent: "implementer", Name: "change-123", Repository: "https://github.com/lokalise/kargo.git",
		Prompt: "fix the failed checks",
	})
	assert.Equal(t, ErrorConflict, ErrorKindOf(err))
	assert.EqualError(t, err, conflict.Error())
	assert.ErrorIs(t, err, session.ErrConflict)
}

type agentCatalogStub struct {
	definition AgentDefinition
}

type sessionStarterStub struct {
	start func(context.Context, session.Definition, session.StartRequest) (session.StartResult, error)
}

func (s sessionStarterStub) Start(
	ctx context.Context, definition session.Definition, request session.StartRequest,
) (session.StartResult, error) {
	return s.start(ctx, definition, request)
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
