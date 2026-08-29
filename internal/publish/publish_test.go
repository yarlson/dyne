package publish

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"coding-agent-k8s/internal/github"
	"coding-agent-k8s/internal/kubernetes"
)

func TestPublishSessionRecoversExistingPullRequestAndCleansPublisher(t *testing.T) {
	request := validPublishRequest()
	wantIntentID := "fcb5bce47798f08a35e945dd9d21559f587cd4711b85263d1c86711b521d2ffb"
	var operations []string
	cluster := publishClusterForRequest(t)
	cluster.intent = func(context.Context, string, string) (string, bool, error) {
		return wantIntentID, true, nil
	}
	cluster.waitPublisher = func(_ context.Context, namespace, session, intentID string, timeout time.Duration) (kubernetes.PublisherJobResult, error) {
		operations = append(operations, "wait for publisher")
		if namespace != "coding-agents" || session != "review" || intentID != wantIntentID || timeout != time.Minute {
			t.Fatalf("waited namespace=%q session=%q intent=%q timeout=%s", namespace, session, intentID, timeout)
		}

		return kubernetes.PublisherJobResult{Branch: "yar/review", CommitSHA: "9a4484441215661904e02a807adf5034d13f5bbe"}, nil
	}
	cluster.deletePublisher = func(_ context.Context, namespace, session string) error {
		operations = append(operations, "delete publisher")
		if namespace != "coding-agents" || session != "review" {
			t.Fatalf("deleted publisher for %s/%s", namespace, session)
		}

		return nil
	}
	client := githubClientStub{
		findPull: func(_ context.Context, _ github.Repository, baseBranch, branch string) (*github.PullRequest, error) {
			operations = append(operations, "find pull request")
			if baseBranch != "main" || branch != "yar/review" {
				t.Fatalf("searched base=%q branch=%q", baseBranch, branch)
			}

			return &github.PullRequest{Number: 17, URL: "https://github.com/lokalise/kargo/pull/17"}, nil
		},
	}
	result, err := publishSession(context.Background(), cluster, request, githubClientFactory(t, client))
	if err != nil {
		t.Fatal(err)
	}

	wantResult := Result{
		PullRequestNumber: 17,
		PullRequestURL:    "https://github.com/lokalise/kargo/pull/17",
		Branch:            "yar/review",
	}
	if result != wantResult {
		t.Fatalf("got result %#v, want %#v", result, wantResult)
	}

	wantOperations := []string{"find pull request", "wait for publisher", "delete publisher"}
	if !slices.Equal(operations, wantOperations) {
		t.Fatalf("got operations %q, want %q", operations, wantOperations)
	}
}

func TestSessionRejectsInvalidRequestBeforeUsingCluster(t *testing.T) {
	request := validPublishRequest()
	request.Branch = " yar/review"
	_, err := Session(context.Background(), nil, request)
	if err == nil || err.Error() != "--branch must not start or end with whitespace" {
		t.Fatalf("got error %v", err)
	}
}

func TestPublishSessionRejectsExistingBranchWithoutPublisherOwnership(t *testing.T) {
	request := validPublishRequest()
	cluster := publishClusterForRequest(t)
	client := githubClientStub{
		findPull: func(context.Context, github.Repository, string, string) (*github.PullRequest, error) {
			return nil, nil
		},
		branchCommit: func(context.Context, github.Repository, string) (string, bool, error) {
			return "9a4484441215661904e02a807adf5034d13f5bbe", true, nil
		},
	}
	_, err := publishSession(context.Background(), cluster, request, githubClientFactory(t, client))
	if err == nil || err.Error() != "remote branch yar/review already exists and is not owned by this publish request" {
		t.Fatalf("got error %v", err)
	}
}

func TestPublishSessionCleansFailedPublisherWhenBranchWasNotCreated(t *testing.T) {
	publisherFailure := errors.New("publisher Pod failed")
	var operations []string
	cluster := publishClusterForRequest(t)
	cluster.runPublisher = func(context.Context, kubernetes.PublisherJobRequest) (kubernetes.PublisherJobResult, error) {
		operations = append(operations, "publish branch")

		return kubernetes.PublisherJobResult{}, publisherFailure
	}
	cluster.deletePublisher = func(context.Context, string, string) error {
		operations = append(operations, "delete publisher")

		return nil
	}
	client := githubClientStub{
		author: func(context.Context) (string, string, error) {
			return "yar", "12345+yar@users.noreply.github.com", nil
		},
		findPull: func(context.Context, github.Repository, string, string) (*github.PullRequest, error) {
			return nil, nil
		},
		branchCommit: func(context.Context, github.Repository, string) (string, bool, error) {
			operations = append(operations, "check branch")

			return "", false, nil
		},
	}
	_, err := publishSession(context.Background(), cluster, validPublishRequest(), githubClientFactory(t, client))
	if !errors.Is(err, publisherFailure) {
		t.Fatalf("got error %v, want publisher failure", err)
	}

	wantOperations := []string{"check branch", "publish branch", "check branch", "delete publisher"}
	if !slices.Equal(operations, wantOperations) {
		t.Fatalf("got operations %q, want %q", operations, wantOperations)
	}
}

func TestPublishSessionStopsAfterCancellation(t *testing.T) {
	cluster := publishClusterForRequest(t)
	cluster.source = func(context.Context, string, string) (kubernetes.PublishSource, error) {
		return kubernetes.PublishSource{}, context.Canceled
	}
	_, err := publishSession(context.Background(), cluster, validPublishRequest(), func(string) (githubClient, error) {
		t.Fatal("created a GitHub client after cancellation")

		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want cancellation", err)
	}
}

func TestPublishSessionRecoversPullRequestAfterAmbiguousCreateFailure(t *testing.T) {
	request := validPublishRequest()
	commitSHA := "9a4484441215661904e02a807adf5034d13f5bbe"
	createFailure := errors.New("create request connection reset")
	var operations []string
	cluster := publishClusterForRequest(t)
	cluster.runPublisher = func(_ context.Context, job kubernetes.PublisherJobRequest) (kubernetes.PublisherJobResult, error) {
		operations = append(operations, "publish branch")
		if job.Namespace != "coding-agents" || job.Session != "review" || job.Repository != "https://github.com/lokalise/kargo.git" || job.BaseRef != "main" || job.Branch != "yar/review" {
			t.Fatalf("got publisher destination %#v", job)
		}

		if job.CommitMessage != "Review changes" || job.AuthorName != "yar" || job.AuthorEmail != "12345+yar@users.noreply.github.com" {
			t.Fatalf("got publisher commit %#v", job)
		}

		if job.Image != "coding-agent:test" || job.WorkspaceClaim != "workspace-review" || job.Timeout != time.Minute || job.IntentID == "" {
			t.Fatalf("got publisher runtime %#v", job)
		}

		return kubernetes.PublisherJobResult{Branch: "yar/review", CommitSHA: commitSHA}, nil
	}
	cluster.deletePublisher = func(_ context.Context, namespace, session string) error {
		operations = append(operations, "delete publisher")
		if namespace != "coding-agents" || session != "review" {
			t.Fatalf("deleted publisher for %s/%s", namespace, session)
		}

		return nil
	}
	client := githubClientStub{
		author: func(context.Context) (string, string, error) {
			return "yar", "12345+yar@users.noreply.github.com", nil
		},
		branchCommit: func(_ context.Context, _ github.Repository, branch string) (string, bool, error) {
			operations = append(operations, "find branch")
			if branch != "yar/review" {
				t.Fatalf("searched branch=%q", branch)
			}

			return "", false, nil
		},
		waitBranch: func(_ context.Context, _ github.Repository, branch, expectedCommit string, timeout time.Duration) error {
			operations = append(operations, "wait for branch")
			if branch != "yar/review" || expectedCommit != commitSHA || timeout != 30*time.Second {
				t.Fatalf("waited branch=%q commit=%q timeout=%s", branch, expectedCommit, timeout)
			}

			return nil
		},
		findPull: func(context.Context, github.Repository, string, string) (*github.PullRequest, error) {
			operations = append(operations, "find pull request")

			return nil, nil
		},
		createPull: func(_ context.Context, _ github.Repository, baseBranch, branch, title, body string, draft bool) (github.PullRequest, error) {
			operations = append(operations, "create pull request")
			if baseBranch != "main" || branch != "yar/review" || title != "Review changes" || body != "Ready for review" || !draft {
				t.Fatalf("created base=%q branch=%q title=%q body=%q draft=%t", baseBranch, branch, title, body, draft)
			}

			return github.PullRequest{}, createFailure
		},
		waitPull: func(_ context.Context, _ github.Repository, baseBranch, branch string, timeout time.Duration) (*github.PullRequest, error) {
			operations = append(operations, "recover pull request")
			if baseBranch != "main" || branch != "yar/review" || timeout != 10*time.Second {
				t.Fatalf("waited base=%q branch=%q timeout=%s", baseBranch, branch, timeout)
			}

			return &github.PullRequest{Number: 23, URL: "https://github.com/lokalise/kargo/pull/23"}, nil
		},
	}
	result, err := publishSession(context.Background(), cluster, request, githubClientFactory(t, client))
	if err != nil {
		t.Fatal(err)
	}

	wantResult := Result{
		PullRequestNumber: 23,
		PullRequestURL:    "https://github.com/lokalise/kargo/pull/23",
		Branch:            "yar/review",
		CommitSHA:         commitSHA,
	}
	if result != wantResult {
		t.Fatalf("got result %#v, want %#v", result, wantResult)
	}

	wantOperations := []string{
		"find pull request",
		"find branch",
		"publish branch",
		"wait for branch",
		"create pull request",
		"recover pull request",
		"delete publisher",
	}
	if !slices.Equal(operations, wantOperations) {
		t.Fatalf("got operations %q, want %q", operations, wantOperations)
	}
}

type publishClusterStub struct {
	source          func(context.Context, string, string) (kubernetes.PublishSource, error)
	token           func(context.Context, string) (string, error)
	intent          func(context.Context, string, string) (string, bool, error)
	runPublisher    func(context.Context, kubernetes.PublisherJobRequest) (kubernetes.PublisherJobResult, error)
	waitPublisher   func(context.Context, string, string, string, time.Duration) (kubernetes.PublisherJobResult, error)
	deletePublisher func(context.Context, string, string) error
}

func (c publishClusterStub) SessionPublishSource(ctx context.Context, namespace, session string) (kubernetes.PublishSource, error) {
	return c.source(ctx, namespace, session)
}

func (c publishClusterStub) GitHubToken(ctx context.Context, namespace string) (string, error) {
	return c.token(ctx, namespace)
}

func (c publishClusterStub) PublisherJobIntent(ctx context.Context, namespace, session string) (string, bool, error) {
	return c.intent(ctx, namespace, session)
}

func (c publishClusterStub) RunPublisherJob(ctx context.Context, request kubernetes.PublisherJobRequest) (kubernetes.PublisherJobResult, error) {
	return c.runPublisher(ctx, request)
}

func (c publishClusterStub) WaitForPublisherJob(ctx context.Context, namespace, session, intentID string, timeout time.Duration) (kubernetes.PublisherJobResult, error) {
	return c.waitPublisher(ctx, namespace, session, intentID, timeout)
}

func (c publishClusterStub) DeletePublisherJob(ctx context.Context, namespace, session string) error {
	return c.deletePublisher(ctx, namespace, session)
}

type githubClientStub struct {
	author       func(context.Context) (string, string, error)
	branchCommit func(context.Context, github.Repository, string) (string, bool, error)
	waitBranch   func(context.Context, github.Repository, string, string, time.Duration) error
	findPull     func(context.Context, github.Repository, string, string) (*github.PullRequest, error)
	waitPull     func(context.Context, github.Repository, string, string, time.Duration) (*github.PullRequest, error)
	createPull   func(context.Context, github.Repository, string, string, string, string, bool) (github.PullRequest, error)
}

func (c githubClientStub) CommitAuthor(ctx context.Context) (string, string, error) {
	return c.author(ctx)
}

func (c githubClientStub) BranchCommitSHA(ctx context.Context, repository github.Repository, branch string) (string, bool, error) {
	return c.branchCommit(ctx, repository, branch)
}

func (c githubClientStub) WaitForBranchCommit(ctx context.Context, repository github.Repository, branch, commit string, timeout time.Duration) error {
	return c.waitBranch(ctx, repository, branch, commit, timeout)
}

func (c githubClientStub) FindOpenPullRequest(ctx context.Context, repository github.Repository, baseBranch, branch string) (*github.PullRequest, error) {
	return c.findPull(ctx, repository, baseBranch, branch)
}

func (c githubClientStub) WaitForOpenPullRequest(ctx context.Context, repository github.Repository, baseBranch, branch string, timeout time.Duration) (*github.PullRequest, error) {
	return c.waitPull(ctx, repository, baseBranch, branch, timeout)
}

func (c githubClientStub) CreatePullRequest(ctx context.Context, repository github.Repository, baseBranch, branch, title, body string, draft bool) (github.PullRequest, error) {
	return c.createPull(ctx, repository, baseBranch, branch, title, body, draft)
}

func validPublishRequest() Request {
	return Request{
		Namespace:     "coding-agents",
		Session:       "review",
		Branch:        "yar/review",
		CommitMessage: "Review changes",
		Title:         "Review changes",
		Body:          "Ready for review",
		Draft:         true,
		Timeout:       time.Minute,
	}
}

func eligiblePublishSource() kubernetes.PublishSource {
	return kubernetes.PublishSource{
		Repository:     "https://github.com/lokalise/kargo.git",
		InitialRef:     "main",
		Image:          "coding-agent:test",
		WorkspaceClaim: "workspace-review",
	}
}

func publishClusterForRequest(t *testing.T) publishClusterStub {
	t.Helper()

	return publishClusterStub{
		source: func(_ context.Context, namespace, session string) (kubernetes.PublishSource, error) {
			if namespace != "coding-agents" || session != "review" {
				t.Fatalf("loaded source for %s/%s", namespace, session)
			}

			return eligiblePublishSource(), nil
		},
		token: func(_ context.Context, namespace string) (string, error) {
			if namespace != "coding-agents" {
				t.Fatalf("loaded token from namespace %q", namespace)
			}

			return "github-token", nil
		},
		intent: func(_ context.Context, namespace, session string) (string, bool, error) {
			if namespace != "coding-agents" || session != "review" {
				t.Fatalf("loaded publisher intent for %s/%s", namespace, session)
			}

			return "", false, nil
		},
	}
}

func githubClientFactory(t *testing.T, client githubClient) func(string) (githubClient, error) {
	t.Helper()

	return func(token string) (githubClient, error) {
		if token != "github-token" {
			t.Fatalf("created GitHub client with token %q", token)
		}

		return client, nil
	}
}
