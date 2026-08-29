package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRepositoryRejectsUnsafeOrUnsupportedLocations(t *testing.T) {
	for _, repositoryURL := range []string{
		"http://github.com/lokalise/kargo.git",
		"https://token@github.com/lokalise/kargo.git",
		"https://gitlab.com/lokalise/kargo.git",
		"https://github.com/lokalise/group/kargo.git",
	} {
		t.Run(repositoryURL, func(t *testing.T) {
			_, err := ParseRepository(repositoryURL)
			require.Error(t, err)
		})
	}
}

func TestCreatePullRequestSendsDraftRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/repos/lokalise/kargo/pulls", request.URL.Path)
		assert.Equal(t, "Bearer secret-token", request.Header.Get("Authorization"))

		var body struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
			Draft bool   `json:"draft"`
		}
		assert.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		assert.Equal(t, "Improve delivery", body.Title)
		assert.Equal(t, "yar/improve-delivery", body.Head)
		assert.Equal(t, "main", body.Base)
		assert.Equal(t, "PR details", body.Body)
		assert.True(t, body.Draft)

		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"number":42,"html_url":"https://github.com/lokalise/kargo/pull/42"}`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	pull, err := client.CreatePullRequest(context.Background(), kargoRepository(t), "main", "yar/improve-delivery", "Improve delivery", "PR details", true)
	require.NoError(t, err)
	assert.Equal(t, PullRequest{Number: 42, URL: "https://github.com/lokalise/kargo/pull/42"}, pull)
}

func TestCommitAuthorUsesAuthenticatedGitHubIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodGet, request.Method)
		assert.Equal(t, "/user", request.URL.Path)

		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"login":"yar","id":12345}`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	name, email, err := client.CommitAuthor(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "yar", name)
	assert.Equal(t, "12345+yar@users.noreply.github.com", email)
}

func TestBranchCommitSHAPreservesNestedBranchName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/repos/lokalise/kargo/git/ref/heads/yar/KARGO-123-description", request.RequestURI)

		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"object":{"sha":"7e79cf1ec3840a9340bc9fa07d2ca96c514142d4"}}`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	commit, exists, err := client.BranchCommitSHA(context.Background(), kargoRepository(t), "yar/KARGO-123-description")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, "7e79cf1ec3840a9340bc9fa07d2ca96c514142d4", commit)
}

func TestBranchCommitSHAReportsMissingBranch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusNotFound)
		_, _ = response.Write([]byte(`{"message":"Branch not found"}`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	commit, exists, err := client.BranchCommitSHA(context.Background(), kargoRepository(t), "yar/missing")
	require.NoError(t, err)
	assert.False(t, exists)
	assert.Empty(t, commit)
}

func TestWaitForOpenPullRequestHandlesDelayedVisibility(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "open", request.URL.Query().Get("state"))
		assert.Equal(t, "lokalise:yar/change", request.URL.Query().Get("head"))
		assert.Equal(t, "main", request.URL.Query().Get("base"))
		assert.Equal(t, "2", request.URL.Query().Get("per_page"))

		response.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			_, _ = response.Write([]byte(`[]`))

			return
		}

		_, _ = response.Write([]byte(`[{"number":7,"html_url":"https://github.com/lokalise/kargo/pull/7"}]`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	client.pollInterval = time.Millisecond
	pull, err := client.WaitForOpenPullRequest(context.Background(), kargoRepository(t), "main", "yar/change", 100*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, pull)
	assert.Equal(t, 7, pull.Number)
}

func testClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	client, err := New("secret-token")
	require.NoError(t, err)

	baseURL, err := url.Parse(serverURL + "/")
	require.NoError(t, err)

	client.api.BaseURL = baseURL

	return client
}

func kargoRepository(t *testing.T) Repository {
	t.Helper()
	repository, err := ParseRepository("https://github.com/lokalise/kargo.git")
	require.NoError(t, err)

	return repository
}
