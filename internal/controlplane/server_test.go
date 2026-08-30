package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/agent"
	"github.com/yarlson/dyne/internal/workflow"
)

func TestCreateSessionDoesNotExposeRawSessionContract(t *testing.T) {
	server := New(operationsStub{}, Config{})
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

func TestListAgentsReturnsSafeProductSummaries(t *testing.T) {
	server := New(operationsStub{agents: []agent.AgentSummary{
		{Name: "implementer", Description: "Implements focused changes.", Storage: agent.StoragePersistent, Skills: []string{"code-review"}},
		{Name: "reviewer", Description: "Reviews repository changes.", Storage: agent.StorageEphemeral, Skills: []string{"code-review"}},
	}}, Config{})
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

func TestCreateAgentSessionDelegatesOnlyClientOwnedInputs(t *testing.T) {
	var received agent.StartRequest
	server := New(operationsStub{start: func(_ context.Context, request agent.StartRequest) (agent.StartResult, error) {
		received = request

		return agent.StartResult{Agent: request.Agent, Name: request.Name, TaskID: request.Name}, nil
	}}, Config{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/agents/implementer/sessions", strings.NewReader(`{
		"name":"change-123",
		"repository":"https://github.com/lokalise/kargo.git",
		"prompt":"fix the failed checks",
		"timeout_seconds":1800
	}`))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusAccepted, response.Code)
	assert.Equal(t, agent.StartRequest{
		Agent:      "implementer",
		Name:       "change-123",
		Repository: "https://github.com/lokalise/kargo.git",
		Prompt:     "fix the failed checks",
		Timeout:    30 * time.Minute,
	}, received)
	assert.JSONEq(t, `{"agent":"implementer","name":"change-123","task_id":"change-123"}`, response.Body.String())
}

func TestWorkflowRoutesDelegateDurableRunOperations(t *testing.T) {
	var started workflow.StartRequest
	var canceled string
	workflows := workflowOperationsStub{
		workflows: []workflow.Summary{{Name: "delivery", Description: "Review then implement.", MaxParallelism: 2, Steps: 3}},
		start: func(_ context.Context, request workflow.StartRequest) (workflow.Run, error) {
			started = request

			return workflow.Run{Name: request.Name, Workflow: request.Workflow, State: workflow.StatePending}, nil
		},
		cancel: func(_ context.Context, name string) error {
			canceled = name

			return nil
		},
	}
	server := New(operationsStub{}, Config{Workflows: workflows})

	listResponse := httptest.NewRecorder()
	server.ServeHTTP(listResponse, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/workflows", nil))
	require.Equal(t, http.StatusOK, listResponse.Code)
	assert.JSONEq(t, `{"workflows":[{"name":"delivery","description":"Review then implement.","max_parallelism":2,"steps":3}]}`, listResponse.Body.String())

	startResponse := httptest.NewRecorder()
	server.ServeHTTP(startResponse, httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/v1/workflows/delivery/runs",
		strings.NewReader(`{"name":"change-123","repository":"https://github.com/lokalise/kargo.git","ref":"main","prompt":"fix it"}`),
	))
	require.Equal(t, http.StatusAccepted, startResponse.Code)
	assert.Equal(t, workflow.StartRequest{
		Workflow: "delivery", Name: "change-123", Repository: "https://github.com/lokalise/kargo.git", Ref: "main", Prompt: "fix it",
	}, started)

	cancelResponse := httptest.NewRecorder()
	server.ServeHTTP(cancelResponse, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/workflow-runs/change-123/cancel", nil))
	assert.Equal(t, http.StatusNoContent, cancelResponse.Code)
	assert.Equal(t, "change-123", canceled)
}

func TestCreateAgentSessionReturnsNotFoundForUnknownAgent(t *testing.T) {
	server := New(&agent.Control{}, Config{})
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

func TestContinueSessionReturnsProductGeneratedTaskID(t *testing.T) {
	var received agent.ContinueRequest
	server := New(operationsStub{continueTask: func(_ context.Context, request agent.ContinueRequest) (agent.TaskResult, error) {
		received = request

		return agent.TaskResult{Name: request.Name, TaskID: "abc123"}, nil
	}}, Config{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions/review/tasks", strings.NewReader(`{"prompt":"continue the fix"}`))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	assert.Equal(t, http.StatusAccepted, response.Code)
	assert.Equal(t, agent.ContinueRequest{Name: "review", Prompt: "continue the fix"}, received)
	assert.JSONEq(t, `{"name":"review","task_id":"abc123"}`, response.Body.String())
}

func TestStatusReturnsStableJSON(t *testing.T) {
	server := New(operationsStub{status: func(_ context.Context, name string) (agent.Status, error) {
		assert.Equal(t, "review", name)

		return agent.Status{Resources: []agent.ResourceStatus{{Kind: "Job", Name: "review", Ready: "1/1", State: "Complete"}}}, nil
	}}, Config{})
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
	var received agent.PublishRequest
	server := New(operationsStub{publish: func(_ context.Context, request agent.PublishRequest) (agent.PublishResult, error) {
		received = request

		return agent.PublishResult{PullRequestNumber: 17, PullRequestURL: "https://github.com/lokalise/kargo/pull/17"}, nil
	}}, Config{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions/review/publish", strings.NewReader(`{
		"branch":"yar/review",
		"commit_message":"Review changes"
	}`))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, agent.PublishRequest{
		Name: "review", Branch: "yar/review", CommitMessage: "Review changes", Draft: true, Timeout: 10 * time.Minute,
	}, received)
	assert.NotContains(t, response.Body.String(), "title")
}

func TestOperationFailureReturnsSafeServerErrorAndRetainsCause(t *testing.T) {
	var errorOutput bytes.Buffer
	server := New(operationsStub{status: func(context.Context, string) (agent.Status, error) {
		return agent.Status{}, errors.New("list Pods: private cluster detail")
	}}, Config{ErrorOutput: &errorOutput})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/sessions/review", nil)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.JSONEq(t, `{"error":"operation failed"}`, response.Body.String())
	assert.Contains(t, errorOutput.String(), "private cluster detail")
	assert.NotContains(t, response.Body.String(), "private cluster detail")
}

func TestLogFailureAfterStreamingDoesNotAppendJSON(t *testing.T) {
	var errorOutput bytes.Buffer
	server := New(operationsStub{writeLogs: func(_ context.Context, _ string, _ bool, output io.Writer) error {
		_, err := io.WriteString(output, "first log line\n")
		require.NoError(t, err)

		return errors.New("stream interrupted")
	}}, Config{ErrorOutput: &errorOutput})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/sessions/review/logs", nil)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "first log line\n", response.Body.String())
	assert.Contains(t, errorOutput.String(), "stream interrupted")
}

type operationsStub struct {
	agents       []agent.AgentSummary
	start        func(context.Context, agent.StartRequest) (agent.StartResult, error)
	continueTask func(context.Context, agent.ContinueRequest) (agent.TaskResult, error)
	status       func(context.Context, string) (agent.Status, error)
	publish      func(context.Context, agent.PublishRequest) (agent.PublishResult, error)
	writeLogs    func(context.Context, string, bool, io.Writer) error
}

type workflowOperationsStub struct {
	workflows []workflow.Summary
	start     func(context.Context, workflow.StartRequest) (workflow.Run, error)
	get       func(context.Context, string) (workflow.Run, error)
	artifacts func(context.Context, string) (workflow.Artifacts, error)
	cancel    func(context.Context, string) error
	delete    func(context.Context, string) error
}

func (s workflowOperationsStub) Workflows() []workflow.Summary { return s.workflows }

func (s workflowOperationsStub) Start(ctx context.Context, request workflow.StartRequest) (workflow.Run, error) {
	return s.start(ctx, request)
}

func (s workflowOperationsStub) Get(ctx context.Context, name string) (workflow.Run, error) {
	return s.get(ctx, name)
}

func (s workflowOperationsStub) Artifacts(ctx context.Context, name string) (workflow.Artifacts, error) {
	return s.artifacts(ctx, name)
}

func (s workflowOperationsStub) Cancel(ctx context.Context, name string) error {
	return s.cancel(ctx, name)
}

func (s workflowOperationsStub) Delete(ctx context.Context, name string) error {
	return s.delete(ctx, name)
}

func (s operationsStub) Agents() []agent.AgentSummary { return s.agents }

func (s operationsStub) Start(ctx context.Context, request agent.StartRequest) (agent.StartResult, error) {
	return s.start(ctx, request)
}

func (s operationsStub) Continue(ctx context.Context, request agent.ContinueRequest) (agent.TaskResult, error) {
	return s.continueTask(ctx, request)
}

func (s operationsStub) Status(ctx context.Context, name string) (agent.Status, error) {
	return s.status(ctx, name)
}

func (s operationsStub) Artifacts(context.Context, string) (agent.Artifacts, error) {
	return agent.Artifacts{}, nil
}

func (s operationsStub) WriteLogs(ctx context.Context, name string, follow bool, output io.Writer) error {
	if s.writeLogs == nil {
		return nil
	}

	return s.writeLogs(ctx, name, follow, output)
}
func (s operationsStub) Delete(context.Context, string) error  { return nil }
func (s operationsStub) Destroy(context.Context, string) error { return nil }

func (s operationsStub) Publish(ctx context.Context, request agent.PublishRequest) (agent.PublishResult, error) {
	return s.publish(ctx, request)
}
