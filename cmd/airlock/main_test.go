package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartSendsSessionRequestOnlyToControlPlane(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/v1/sessions", request.URL.Path)
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"name":"review","task_id":"review"}`))
	}))
	defer server.Close()

	var output bytes.Buffer
	err := run(context.Background(), []string{
		"start",
		"--server", server.URL,
		"--name", "review",
		"--storage", "persistent",
		"--repo", "https://github.com/lokalise/kargo.git",
		"--prompt", "fix the failed checks",
	}, &output, &output)
	require.NoError(t, err)
	assert.Equal(t, "review", body["name"])
	assert.Equal(t, "persistent", body["storage"])
	assert.NotContains(t, body, "context")
	assert.JSONEq(t, `{"name":"review","task_id":"review"}`, output.String())
}

func TestRunDoesNotExposeRemovedDirectClusterCommands(t *testing.T) {
	err := run(context.Background(), []string{"shell"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.ErrorContains(t, err, `unknown command "shell"`)
}
