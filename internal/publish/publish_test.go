package publish

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/github"
	"github.com/yarlson/dyne/internal/kubernetes"
)

func TestPublishSessionRecoversExistingPullRequestAndCleansPublisher(t *testing.T) {
	request := validPublishRequest()
	wantIntentID := "34205b65a7344f406dd8d5802ceb811d7216f535751324a35dadac24bd776d92"
	var operations []string
	cluster := publishClusterForRequest(t)
	cluster.intent = func(context.Context, string, string) (string, bool, error) {
		return wantIntentID, true, nil
	}
	cluster.waitPublisher = func(_ context.Context, namespace, session, intentID string, timeout time.Duration) (kubernetes.PublisherJobResult, error) {
		operations = append(operations, "wait for publisher")
		assert.Equal(t, "coding-agents", namespace)
		assert.Equal(t, "review", session)
		assert.Equal(t, wantIntentID, intentID)
		assert.Equal(t, time.Minute, timeout)

		return kubernetes.PublisherJobResult{Branch: "yar/review", CommitSHA: "9a4484441215661904e02a807adf5034d13f5bbe", Title: "Review changes", Body: "Ready for review"}, nil
	}
	cluster.deletePublisher = func(_ context.Context, namespace, session string) error {
		operations = append(operations, "delete publisher")
		assert.Equal(t, "coding-agents", namespace)
		assert.Equal(t, "review", session)

		return nil
	}
	client := githubClientStub{
		findPull: func(_ context.Context, _ github.Repository, baseBranch, branch string) (*github.PullRequest, error) {
			operations = append(operations, "find pull request")
			assert.Equal(t, "main", baseBranch)
			assert.Equal(t, "yar/review", branch)

			return &github.PullRequest{Number: 17, URL: "https://github.com/lokalise/kargo/pull/17"}, nil
		},
	}
	result, err := publishSession(context.Background(), cluster, request, githubClientFactory(t, client))
	require.NoError(t, err)

	wantResult := Result{
		PullRequestNumber: 17,
		PullRequestURL:    "https://github.com/lokalise/kargo/pull/17",
		Branch:            "yar/review",
	}
	assert.Equal(t, wantResult, result)

	wantOperations := []string{"find pull request", "wait for publisher", "delete publisher"}
	assert.Equal(t, wantOperations, operations)
}

func TestSessionRejectsInvalidRequestBeforeUsingCluster(t *testing.T) {
	request := validPublishRequest()
	request.Branch = " yar/review"
	_, err := Session(context.Background(), nil, request)
	require.EqualError(t, err, "branch must not start or end with whitespace")
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
	require.EqualError(t, err, "remote branch yar/review already exists and is not owned by this publish request")
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
	require.ErrorIs(t, err, publisherFailure)

	wantOperations := []string{"check branch", "publish branch", "check branch", "delete publisher"}
	assert.Equal(t, wantOperations, operations)
}

func TestPublishSessionStopsAfterCancellation(t *testing.T) {
	cluster := publishClusterForRequest(t)
	cluster.source = func(context.Context, string, string) (kubernetes.PublishSource, error) {
		return kubernetes.PublishSource{}, context.Canceled
	}
	_, err := publishSession(context.Background(), cluster, validPublishRequest(), func(string) (githubClient, error) {
		require.FailNow(t, "created a GitHub client after cancellation")

		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestPublishSessionRecoversPullRequestAfterAmbiguousCreateFailure(t *testing.T) {
	request := validPublishRequest()
	commitSHA := "9a4484441215661904e02a807adf5034d13f5bbe"
	createFailure := errors.New("create request connection reset")
	var operations []string
	cluster := publishClusterForRequest(t)
	cluster.runPublisher = func(_ context.Context, job kubernetes.PublisherJobRequest) (kubernetes.PublisherJobResult, error) {
		operations = append(operations, "publish branch")
		assert.Equal(t, "coding-agents", job.Namespace)
		assert.Equal(t, "review", job.Session)
		assert.Equal(t, "https://github.com/lokalise/kargo.git", job.Repository)
		assert.Equal(t, "main", job.BaseRef)
		assert.Equal(t, "yar/review", job.Branch)
		assert.Equal(t, "Review changes", job.CommitMessage)
		assert.Equal(t, "yar", job.AuthorName)
		assert.Equal(t, "12345+yar@users.noreply.github.com", job.AuthorEmail)
		assert.Equal(t, "coding-agent:test", job.Image)
		assert.Equal(t, "workspace-review", job.WorkspaceClaim)
		assert.Equal(t, time.Minute, job.Timeout)
		assert.NotEmpty(t, job.IntentID)

		return kubernetes.PublisherJobResult{Branch: "yar/review", CommitSHA: commitSHA, Title: "Review changes", Body: "Ready for review"}, nil
	}
	cluster.deletePublisher = func(_ context.Context, namespace, session string) error {
		operations = append(operations, "delete publisher")
		assert.Equal(t, "coding-agents", namespace)
		assert.Equal(t, "review", session)

		return nil
	}
	client := githubClientStub{
		author: func(context.Context) (string, string, error) {
			return "yar", "12345+yar@users.noreply.github.com", nil
		},
		branchCommit: func(_ context.Context, _ github.Repository, branch string) (string, bool, error) {
			operations = append(operations, "find branch")
			assert.Equal(t, "yar/review", branch)

			return "", false, nil
		},
		waitBranch: func(_ context.Context, _ github.Repository, branch, expectedCommit string, timeout time.Duration) error {
			operations = append(operations, "wait for branch")
			assert.Equal(t, "yar/review", branch)
			assert.Equal(t, commitSHA, expectedCommit)
			assert.Equal(t, 30*time.Second, timeout)

			return nil
		},
		findPull: func(context.Context, github.Repository, string, string) (*github.PullRequest, error) {
			operations = append(operations, "find pull request")

			return nil, nil
		},
		createPull: func(_ context.Context, _ github.Repository, baseBranch, branch, title, body string, draft bool) (github.PullRequest, error) {
			operations = append(operations, "create pull request")
			assert.Equal(t, "main", baseBranch)
			assert.Equal(t, "yar/review", branch)
			assert.Equal(t, "Review changes", title)
			assert.Equal(t, "Ready for review", body)
			assert.True(t, draft)

			return github.PullRequest{}, createFailure
		},
		waitPull: func(_ context.Context, _ github.Repository, baseBranch, branch string, timeout time.Duration) (*github.PullRequest, error) {
			operations = append(operations, "recover pull request")
			assert.Equal(t, "main", baseBranch)
			assert.Equal(t, "yar/review", branch)
			assert.Equal(t, 10*time.Second, timeout)

			return &github.PullRequest{Number: 23, URL: "https://github.com/lokalise/kargo/pull/23"}, nil
		},
	}
	result, err := publishSession(context.Background(), cluster, request, githubClientFactory(t, client))
	require.NoError(t, err)

	wantResult := Result{
		PullRequestNumber: 23,
		PullRequestURL:    "https://github.com/lokalise/kargo/pull/23",
		Branch:            "yar/review",
		CommitSHA:         commitSHA,
	}
	assert.Equal(t, wantResult, result)

	wantOperations := []string{
		"find pull request",
		"find branch",
		"publish branch",
		"wait for branch",
		"create pull request",
		"recover pull request",
		"delete publisher",
	}
	assert.Equal(t, wantOperations, operations)
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
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", session)

			return eligiblePublishSource(), nil
		},
		token: func(_ context.Context, namespace string) (string, error) {
			assert.Equal(t, "coding-agents", namespace)

			return "github-token", nil
		},
		intent: func(_ context.Context, namespace, session string) (string, bool, error) {
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", session)

			return "", false, nil
		},
	}
}

func githubClientFactory(t *testing.T, client githubClient) func(string) (githubClient, error) {
	t.Helper()

	return func(token string) (githubClient, error) {
		assert.Equal(t, "github-token", token)

		return client, nil
	}
}
