package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/catalog"
)

func TestLoadServerCatalogsUsesBuiltInEngineeringCatalogByDefault(t *testing.T) {
	agents, workflows, err := loadServerCatalogs("", "", catalog.AgentDefaults{
		StorageSize: "10Gi",
		TaskTimeout: time.Hour,
	})
	require.NoError(t, err)
	assert.Len(t, agents.List(), 6)
	assert.Len(t, workflows.List(), 2)
}

func TestLoadServerCatalogsUsesCustomAgentsWithoutBuiltInWorkflows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: v1
agents:
  reviewer:
    description: Reviews changes.
    storage: ephemeral
    instructions: Review the requested change.
`), 0o600))

	agents, workflows, err := loadServerCatalogs(path, "", catalog.AgentDefaults{
		StorageSize: "10Gi",
		TaskTimeout: time.Hour,
	})
	require.NoError(t, err)
	assert.Len(t, agents.List(), 1)
	assert.Nil(t, workflows)
}

func TestLoadServerCatalogsRequiresCustomAgentsForCustomWorkflows(t *testing.T) {
	_, _, err := loadServerCatalogs("", "workflows.yaml", catalog.AgentDefaults{
		StorageSize: "10Gi",
		TaskTimeout: time.Hour,
	})
	require.EqualError(t, err, "--agents-file is required with --workflows-file")
}

func TestAgentsListsServerDefinitions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodGet, request.Method)
		assert.Equal(t, "/v1/agents", request.URL.Path)
		_, _ = writer.Write([]byte(`{"agents":[]}`))
	}))
	defer server.Close()

	var output bytes.Buffer
	err := run(context.Background(), []string{"agents", "--server", server.URL}, &output, &output)
	require.NoError(t, err)
	assert.JSONEq(t, `{"agents":[]}`, output.String())
}

func TestStartRequiresConfiguredAgent(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requested = true
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	err := run(context.Background(), []string{
		"start", "--server", server.URL, "--name", "review",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	require.EqualError(t, err, "--agent is required")
	assert.False(t, requested, "sent a raw session request without an agent")
}

func TestWorkflowCommandsUseWorkflowRoutes(t *testing.T) {
	requests := make(chan *http.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- request.Clone(context.Background())
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"name":"change-123","workflow":"delivery","state":"pending"}`))

			return
		}

		_, _ = writer.Write([]byte(`{"workflows":[]}`))
	}))
	defer server.Close()

	var output bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"workflows", "--server", server.URL}, &output, &output))
	listRequest := <-requests
	assert.Equal(t, http.MethodGet, listRequest.Method)
	assert.Equal(t, "/v1/workflows", listRequest.URL.Path)

	output.Reset()
	require.NoError(t, run(context.Background(), []string{
		"workflow-start", "--server", server.URL, "--workflow", "delivery", "--name", "change-123",
		"--repo", "https://github.com/lokalise/kargo.git", "--prompt", "fix it",
	}, &output, &output))
	startRequest := <-requests
	assert.Equal(t, http.MethodPost, startRequest.Method)
	assert.Equal(t, "/v1/workflows/delivery/runs", startRequest.URL.Path)
}

func TestStartAgentSendsOnlyInstanceInputs(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/v1/agents/reviewer/sessions", request.URL.Path)
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"agent":"reviewer","name":"review","task_id":"review"}`))
	}))
	defer server.Close()

	var output bytes.Buffer
	err := run(context.Background(), []string{
		"start",
		"--server", server.URL,
		"--agent", "reviewer",
		"--name", "review",
		"--repo", "https://github.com/lokalise/kargo.git",
		"--prompt", "review the changes",
	}, &output, &output)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"name":       "review",
		"repository": "https://github.com/lokalise/kargo.git",
		"ref":        "main",
		"prompt":     "review the changes",
	}, body)
}

func TestStartDoesNotExposeSessionConfigurationFlags(t *testing.T) {
	err := run(context.Background(), []string{
		"start", "--agent", "reviewer", "--name", "review", "--storage", "persistent",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	require.EqualError(t, err, "flag provided but not defined: -storage")
}

func TestServerRejectsInvalidAgentsFileBeforeConnecting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: unsupported\n"), 0o600))

	err := run(context.Background(), []string{"server", "--agents-file", path}, &bytes.Buffer{}, &bytes.Buffer{})
	require.ErrorContains(t, err, "load agents file: unsupported agents file version")
}

func TestRunDoesNotExposeRemovedDirectClusterCommands(t *testing.T) {
	err := run(context.Background(), []string{"shell"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.ErrorContains(t, err, `unknown command "shell"`)
}
