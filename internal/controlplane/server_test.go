package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/airlock/internal/agentconfig"
	"github.com/yarlson/airlock/pkg/agentsandbox"
)

func TestCreateSessionDoesNotExposeRawSessionContract(t *testing.T) {
	server := New(operationsStub{start: func(context.Context, agentsandbox.StartRequest) error {
		require.FailNow(t, "started a session through the raw HTTP contract")

		return nil
	}}, Config{}, nil)

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions", strings.NewReader(`{
		"name":"review",
		"storage":"persistent",
		"repository":"https://github.com/lokalise/kargo.git",
		"ref":"main",
		"prompt":"fix the failed checks"
	}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	assert.Equal(t, http.StatusNotFound, response.Code)
}

func TestListAgentsReturnsSafeSortedDefinitions(t *testing.T) {
	server := New(operationsStub{}, Config{Agents: testAgentCatalog(t)}, nil)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/agents", nil)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.JSONEq(t, `{
		"agents":[
			{"name":"implementer","description":"Implements focused changes.","storage":"persistent","skills":["code-review"]},
			{"name":"reviewer","description":"Reviews repository changes.","storage":"ephemeral","skills":["code-review"]}
		]
	}`, response.Body.String())
	assert.NotContains(t, response.Body.String(), "Review correctness")
	assert.NotContains(t, response.Body.String(), "mise install")
}

func TestCreateAgentSessionResolvesDefinition(t *testing.T) {
	var received agentsandbox.StartRequest
	server := New(operationsStub{start: func(_ context.Context, request agentsandbox.StartRequest) error {
		received = request

		return nil
	}}, Config{
		Namespace: "coding-agents", Image: "coding-agent:test", TaskTimeout: time.Hour,
		Agents: testAgentCatalog(t),
	}, nil)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/agents/implementer/sessions", strings.NewReader(`{
		"name":"change-123",
		"repository":"https://github.com/lokalise/kargo.git",
		"prompt":"fix the failed checks",
		"timeout_seconds":1800
	}`))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusAccepted, response.Code)
	assert.Equal(t, agentsandbox.StartRequest{
		Target:       agentsandbox.Target{Namespace: "coding-agents", Name: "change-123"},
		Image:        "coding-agent:test",
		Storage:      agentsandbox.StoragePersistent,
		Repository:   "https://github.com/lokalise/kargo.git",
		InitialRef:   "main",
		SetupCommand: "mise install",
		Prompt:       "fix the failed checks",
		AgentName:    "implementer",
		Instructions: "Implement the smallest safe change.",
		Skills: []agentsandbox.AgentSkill{{
			Name: "code-review",
			Contents: `---
name: code-review
description: Review changed code.
---

Review correctness and tests.
`,
		}},
		CloneDepth:  0,
		StorageSize: "20Gi",
		Timeout:     30 * time.Minute,
	}, received)
	assert.JSONEq(t, `{"agent":"implementer","name":"change-123","task_id":"change-123"}`, response.Body.String())
}

func TestCreateAgentSessionRejectsUnknownAgent(t *testing.T) {
	server := New(operationsStub{start: func(context.Context, agentsandbox.StartRequest) error {
		require.FailNow(t, "started a session for an unknown agent")

		return nil
	}}, Config{Agents: testAgentCatalog(t)}, nil)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/agents/missing/sessions", strings.NewReader(`{
		"name":"change-123",
		"repository":"https://github.com/lokalise/kargo.git",
		"prompt":"fix the failed checks"
	}`))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.JSONEq(t, `{"error":"agent missing is not configured"}`, response.Body.String())
}

func TestContinueSessionReturnsServerGeneratedTaskID(t *testing.T) {
	var received agentsandbox.ContinueRequest
	server := New(operationsStub{continueTask: func(_ context.Context, request agentsandbox.ContinueRequest) error {
		received = request

		return nil
	}}, Config{Namespace: "coding-agents", TaskTimeout: time.Hour}, func() (string, error) {
		return "abc123", nil
	})

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions/review/tasks", strings.NewReader(`{"prompt":"continue the fix"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	assert.Equal(t, http.StatusAccepted, response.Code)
	assert.Equal(t, agentsandbox.ContinueRequest{
		Target:  agentsandbox.Target{Namespace: "coding-agents", Name: "review"},
		TaskID:  "abc123",
		Prompt:  "continue the fix",
		Timeout: time.Hour,
	}, received)
	assert.JSONEq(t, `{"name":"review","task_id":"abc123"}`, response.Body.String())
}

func TestStatusReturnsStableJSON(t *testing.T) {
	server := New(operationsStub{status: func(context.Context, agentsandbox.Target) (agentsandbox.Status, error) {
		return agentsandbox.Status{Resources: []agentsandbox.ResourceStatus{{Kind: "Job", Name: "review", Ready: "1/1", State: "Complete"}}}, nil
	}}, Config{Namespace: "coding-agents"}, nil)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/sessions/review", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "review", body["name"])
	assert.Len(t, body["resources"], 1)
}

func TestPublishUsesAgentAuthoredMetadataContract(t *testing.T) {
	var received agentsandbox.PublishRequest
	server := New(operationsStub{publish: func(_ context.Context, request agentsandbox.PublishRequest) (agentsandbox.PublishResult, error) {
		received = request

		return agentsandbox.PublishResult{PullRequestNumber: 17, PullRequestURL: "https://github.com/lokalise/kargo/pull/17"}, nil
	}}, Config{Namespace: "coding-agents"}, nil)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions/review/publish", strings.NewReader(`{
		"branch":"yar/review",
		"commit_message":"Review changes"
	}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, agentsandbox.PublishRequest{
		Target:        agentsandbox.Target{Namespace: "coding-agents", Name: "review"},
		Branch:        "yar/review",
		CommitMessage: "Review changes",
		Draft:         true,
		Timeout:       10 * time.Minute,
	}, received)
	assert.NotContains(t, response.Body.String(), "title")
}

type operationsStub struct {
	start        func(context.Context, agentsandbox.StartRequest) error
	continueTask func(context.Context, agentsandbox.ContinueRequest) error
	status       func(context.Context, agentsandbox.Target) (agentsandbox.Status, error)
	publish      func(context.Context, agentsandbox.PublishRequest) (agentsandbox.PublishResult, error)
}

func (s operationsStub) Start(ctx context.Context, request agentsandbox.StartRequest) error {
	return s.start(ctx, request)
}

func (s operationsStub) Continue(ctx context.Context, request agentsandbox.ContinueRequest) error {
	return s.continueTask(ctx, request)
}

func (s operationsStub) Status(ctx context.Context, target agentsandbox.Target) (agentsandbox.Status, error) {
	return s.status(ctx, target)
}

func (s operationsStub) Artifacts(context.Context, agentsandbox.Target) (agentsandbox.Artifacts, error) {
	return agentsandbox.Artifacts{}, nil
}

func (s operationsStub) WriteLogs(context.Context, agentsandbox.LogRequest, io.Writer) error {
	return nil
}
func (s operationsStub) Delete(context.Context, agentsandbox.Target) error  { return nil }
func (s operationsStub) Destroy(context.Context, agentsandbox.Target) error { return nil }
func (s operationsStub) Publish(ctx context.Context, request agentsandbox.PublishRequest) (agentsandbox.PublishResult, error) {
	return s.publish(ctx, request)
}

func testAgentCatalog(t *testing.T) *agentconfig.Catalog {
	t.Helper()
	directory := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(directory, "skills", "code-review"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "skills", "code-review", "SKILL.md"), []byte(`---
name: code-review
description: Review changed code.
---

Review correctness and tests.
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "agents.yaml"), []byte(`version: v1
agents:
  reviewer:
    description: Reviews repository changes.
    storage: ephemeral
    instructions: Review correctness, security, and tests.
    skills:
      - skills/code-review/SKILL.md
  implementer:
    description: Implements focused changes.
    storage: persistent
    instructions: Implement the smallest safe change.
    skills:
      - skills/code-review/SKILL.md
    setup: mise install
    clone_depth: 0
    storage_size: 20Gi
    timeout: 4h
`), 0o600))

	catalog, err := agentconfig.Load(filepath.Join(directory, "agents.yaml"), agentconfig.Defaults{
		StorageSize: "10Gi", TaskTimeout: time.Hour,
	})
	require.NoError(t, err)

	return catalog
}
