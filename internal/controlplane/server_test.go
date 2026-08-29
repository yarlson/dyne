package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/airlock/pkg/agentsandbox"
)

func TestCreateSessionMapsHTTPContractToSandbox(t *testing.T) {
	var received agentsandbox.StartRequest
	server := New(operationsStub{start: func(_ context.Context, request agentsandbox.StartRequest) error {
		received = request

		return nil
	}}, Config{Namespace: "coding-agents", Image: "coding-agent:test", StorageSize: "10Gi", TaskTimeout: time.Hour}, func() (string, error) {
		return "unused", nil
	})

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions", strings.NewReader(`{
		"name":"review",
		"storage":"persistent",
		"repository":"https://github.com/lokalise/kargo.git",
		"ref":"main",
		"prompt":"fix the failed checks"
	}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	assert.Equal(t, http.StatusAccepted, response.Code)
	assert.Equal(t, agentsandbox.StartRequest{
		Target:      agentsandbox.Target{Namespace: "coding-agents", Name: "review"},
		Image:       "coding-agent:test",
		Storage:     agentsandbox.StoragePersistent,
		Repository:  "https://github.com/lokalise/kargo.git",
		InitialRef:  "main",
		Prompt:      "fix the failed checks",
		CloneDepth:  1,
		StorageSize: "10Gi",
		Timeout:     time.Hour,
	}, received)
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
