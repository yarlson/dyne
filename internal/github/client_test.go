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
)

func TestParseRepositoryRejectsUnsafeOrUnsupportedLocations(t *testing.T) {
	for _, repositoryURL := range []string{
		"http://github.com/lokalise/kargo.git",
		"https://token@github.com/lokalise/kargo.git",
		"https://gitlab.com/lokalise/kargo.git",
		"https://github.com/lokalise/group/kargo.git",
	} {
		t.Run(repositoryURL, func(t *testing.T) {
			if _, err := ParseRepository(repositoryURL); err == nil {
				t.Fatal("expected repository URL to be rejected")
			}
		})
	}
}

func TestCreatePullRequestSendsDraftRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/repos/lokalise/kargo/pulls" {
			t.Errorf("got %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("request does not use the GitHub token")
		}
		var body struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
			Draft bool   `json:"draft"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Title != "Improve delivery" || body.Head != "yar/improve-delivery" || body.Base != "main" || body.Body != "PR details" || !body.Draft {
			t.Errorf("got request body %#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"number":42,"html_url":"https://github.com/lokalise/kargo/pull/42"}`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	pull, err := client.CreatePullRequest(context.Background(), kargoRepository(t), "main", "yar/improve-delivery", "Improve delivery", "PR details", true)
	if err != nil {
		t.Fatal(err)
	}
	if pull.Number != 42 || pull.URL != "https://github.com/lokalise/kargo/pull/42" {
		t.Fatalf("got pull request %#v", pull)
	}
}

func TestCommitAuthorUsesAuthenticatedGitHubIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/user" {
			t.Errorf("got %s %s", request.Method, request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"login":"yar","id":12345}`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	name, email, err := client.CommitAuthor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if name != "yar" || email != "12345+yar@users.noreply.github.com" {
		t.Fatalf("got commit identity %q <%s>", name, email)
	}
}

func TestBranchCommitPreservesNestedBranchName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.RequestURI != "/repos/lokalise/kargo/git/ref/heads/yar/KARGO-123-description" {
			t.Errorf("got request URI %s", request.RequestURI)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"object":{"sha":"7e79cf1ec3840a9340bc9fa07d2ca96c514142d4"}}`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	commit, exists, err := client.BranchCommit(context.Background(), kargoRepository(t), "yar/KARGO-123-description")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || commit != "7e79cf1ec3840a9340bc9fa07d2ca96c514142d4" {
		t.Fatalf("got commit %q, exists %t", commit, exists)
	}
}

func TestBranchCommitReportsMissingBranch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusNotFound)
		_, _ = response.Write([]byte(`{"message":"Branch not found"}`))
	}))
	t.Cleanup(server.Close)
	client := testClient(t, server.URL)
	commit, exists, err := client.BranchCommit(context.Background(), kargoRepository(t), "yar/missing")
	if err != nil {
		t.Fatal(err)
	}
	if exists || commit != "" {
		t.Fatalf("got commit %q, exists %t", commit, exists)
	}
}

func TestWaitOpenPullRequestHandlesDelayedVisibility(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("state") != "open" || request.URL.Query().Get("head") != "lokalise:yar/change" || request.URL.Query().Get("base") != "main" || request.URL.Query().Get("per_page") != "2" {
			t.Errorf("got query %s", request.URL.RawQuery)
		}
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
	pull, err := client.WaitOpenPullRequest(context.Background(), kargoRepository(t), "main", "yar/change", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if pull == nil || pull.Number != 7 {
		t.Fatalf("got pull request %#v", pull)
	}
}

func testClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	client, err := New("secret-token")
	if err != nil {
		t.Fatal(err)
	}
	baseURL, err := url.Parse(serverURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.api.BaseURL = baseURL
	return client
}

func kargoRepository(t *testing.T) Repository {
	t.Helper()
	repository, err := ParseRepository("https://github.com/lokalise/kargo.git")
	if err != nil {
		t.Fatal(err)
	}
	return repository
}
