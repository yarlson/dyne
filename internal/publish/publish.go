package publish

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"coding-agent-k8s/internal/github"
	"coding-agent-k8s/internal/kubernetes"
)

// Request defines one idempotent workspace publish operation.
type Request struct {
	// Namespace owns the source session.
	Namespace string
	// Session identifies the completed or stopped source session.
	Session string
	// Branch is the new remote branch that will contain the changes.
	Branch string
	// BaseBranch is the pull request target and defaults to the session's initial ref.
	BaseBranch string
	// CommitMessage is the message used for the workspace commit.
	CommitMessage string
	// Title is the pull request title.
	Title string
	// Body is the pull request description.
	Body string
	// Draft controls whether GitHub creates a draft pull request.
	Draft bool
	// Timeout bounds the publisher Job and the wait for its result.
	Timeout time.Duration
}

// Result identifies the pull request, branch, and commit produced by publishing.
type Result struct {
	// PullRequestNumber is the repository-local pull request number.
	PullRequestNumber int
	// PullRequestURL is the pull request's GitHub web URL.
	PullRequestURL string
	// Branch is the remote branch containing the published changes.
	Branch string
	// CommitSHA is the published commit, or empty when publishing recovers an existing pull request.
	CommitSHA string
}

type publishCluster interface {
	SessionPublishSource(context.Context, string, string) (kubernetes.PublishSource, error)
	GitHubToken(context.Context, string) (string, error)
	PublisherJobIntent(context.Context, string, string) (string, bool, error)
	RunPublisherJob(context.Context, kubernetes.PublisherJobRequest) (kubernetes.PublisherJobResult, error)
	WaitForPublisherJob(context.Context, string, string, string, time.Duration) (kubernetes.PublisherJobResult, error)
	DeletePublisherJob(context.Context, string, string) error
}

type githubClient interface {
	CommitAuthor(context.Context) (string, string, error)
	BranchCommitSHA(context.Context, github.Repository, string) (string, bool, error)
	WaitForBranchCommit(context.Context, github.Repository, string, string, time.Duration) error
	FindOpenPullRequest(context.Context, github.Repository, string, string) (*github.PullRequest, error)
	WaitForOpenPullRequest(context.Context, github.Repository, string, string, time.Duration) (*github.PullRequest, error)
	CreatePullRequest(context.Context, github.Repository, string, string, string, string, bool) (github.PullRequest, error)
}

// Session publishes an eligible session workspace and opens or recovers its pull request.
func Session(ctx context.Context, cluster *kubernetes.Client, request Request) (Result, error) {
	return publishSession(ctx, cluster, request, func(token string) (githubClient, error) {
		return github.New(token)
	})
}

func publishSession(ctx context.Context, cluster publishCluster, request Request, newGitHubClient func(string) (githubClient, error)) (Result, error) {
	if err := request.Validate(); err != nil {
		return Result{}, err
	}

	source, err := cluster.SessionPublishSource(ctx, request.Namespace, request.Session)
	if err != nil {
		return Result{}, err
	}

	if request.BaseBranch == "" {
		request.BaseBranch = source.InitialRef
	}

	repository, err := github.ParseRepository(source.Repository)
	if err != nil {
		return Result{}, err
	}

	token, err := cluster.GitHubToken(ctx, request.Namespace)
	if err != nil {
		return Result{}, err
	}

	client, err := newGitHubClient(token)
	if err != nil {
		return Result{}, err
	}

	intentID, err := publishIntentID(request, source.Repository)
	if err != nil {
		return Result{}, err
	}

	publisherIntent, publisherExists, err := cluster.PublisherJobIntent(ctx, request.Namespace, request.Session)
	if err != nil {
		return Result{}, err
	}

	if publisherExists && publisherIntent != intentID {
		return Result{}, errors.New("publisher Job belongs to a different publish request")
	}

	existingPull, err := client.FindOpenPullRequest(ctx, repository, request.BaseBranch, request.Branch)
	if err != nil {
		return Result{}, err
	}

	if existingPull != nil {
		if publisherExists {
			if err := removePublisherJobAfterCompletion(ctx, cluster, request, intentID, existingPull.URL); err != nil {
				return Result{}, err
			}
		}

		return pullRequestResult(*existingPull, request.Branch, ""), nil
	}

	remoteCommit, branchExists, err := client.BranchCommitSHA(ctx, repository, request.Branch)
	if err != nil {
		return Result{}, err
	}

	if branchExists && !publisherExists {
		return Result{}, fmt.Errorf("remote branch %s already exists and is not owned by this publish request", request.Branch)
	}

	if branchExists {
		result, err := cluster.WaitForPublisherJob(ctx, request.Namespace, request.Session, intentID, request.Timeout)
		if err != nil {
			return Result{}, err
		}

		if result.Branch != request.Branch || result.CommitSHA != remoteCommit {
			return Result{}, fmt.Errorf("publisher result %s at %s does not match remote branch %s at %s", result.Branch, result.CommitSHA, request.Branch, remoteCommit)
		}
	} else {
		remoteCommit, err = publishBranch(ctx, cluster, client, repository, source, request, intentID)
		if err != nil {
			return Result{}, err
		}
	}

	pull, err := client.CreatePullRequest(ctx, repository, request.BaseBranch, request.Branch, request.Title, request.Body, request.Draft)
	if err != nil {
		existingPull, findErr := client.WaitForOpenPullRequest(ctx, repository, request.BaseBranch, request.Branch, 10*time.Second)
		if findErr != nil {
			return Result{}, errors.Join(err, findErr)
		}

		if existingPull == nil {
			return Result{}, err
		}

		pull = *existingPull
	}

	if err := cluster.DeletePublisherJob(ctx, request.Namespace, request.Session); err != nil {
		return Result{}, fmt.Errorf("pull request created at %s; clean publisher Job: %w", pull.URL, err)
	}

	return pullRequestResult(pull, request.Branch, remoteCommit), nil
}

func removePublisherJobAfterCompletion(ctx context.Context, cluster publishCluster, request Request, intentID, pullRequestURL string) error {
	if _, err := cluster.WaitForPublisherJob(ctx, request.Namespace, request.Session, intentID, request.Timeout); err != nil {
		return fmt.Errorf("pull request already exists at %s; wait for publisher Job: %w", pullRequestURL, err)
	}

	if err := cluster.DeletePublisherJob(ctx, request.Namespace, request.Session); err != nil {
		return fmt.Errorf("pull request already exists at %s; clean publisher Job: %w", pullRequestURL, err)
	}

	return nil
}

func publishBranch(ctx context.Context, cluster publishCluster, client githubClient, repository github.Repository, source kubernetes.PublishSource, request Request, intentID string) (string, error) {
	authorName, authorEmail, err := client.CommitAuthor(ctx)
	if err != nil {
		return "", err
	}

	result, err := cluster.RunPublisherJob(ctx, kubernetes.PublisherJobRequest{
		Namespace:      request.Namespace,
		Session:        request.Session,
		IntentID:       intentID,
		Repository:     source.Repository,
		BaseRef:        request.BaseBranch,
		Branch:         request.Branch,
		CommitMessage:  request.CommitMessage,
		AuthorName:     authorName,
		AuthorEmail:    authorEmail,
		Image:          source.Image,
		WorkspaceClaim: source.WorkspaceClaim,
		Timeout:        request.Timeout,
	})
	if err != nil {
		_, branchExists, branchErr := client.BranchCommitSHA(ctx, repository, request.Branch)
		if branchErr != nil {
			return "", errors.Join(err, branchErr)
		}

		if branchExists {
			return "", err
		}

		return "", errors.Join(err, cluster.DeletePublisherJob(ctx, request.Namespace, request.Session))
	}

	if result.Branch != request.Branch {
		return "", fmt.Errorf("publisher reported branch %s, want %s", result.Branch, request.Branch)
	}

	if err := client.WaitForBranchCommit(ctx, repository, request.Branch, result.CommitSHA, 30*time.Second); err != nil {
		return "", err
	}

	return result.CommitSHA, nil
}

func publishIntentID(request Request, repository string) (string, error) {
	contents, err := json.Marshal(struct {
		Namespace     string `json:"namespace"`
		Session       string `json:"session"`
		Repository    string `json:"repository"`
		BaseBranch    string `json:"base"`
		Branch        string `json:"branch"`
		CommitMessage string `json:"commitMessage"`
		Title         string `json:"title"`
		Body          string `json:"body"`
		Draft         bool   `json:"draft"`
	}{
		Namespace:     request.Namespace,
		Session:       request.Session,
		Repository:    repository,
		BaseBranch:    request.BaseBranch,
		Branch:        request.Branch,
		CommitMessage: request.CommitMessage,
		Title:         request.Title,
		Body:          request.Body,
		Draft:         request.Draft,
	})
	if err != nil {
		return "", fmt.Errorf("encode publish intent: %w", err)
	}

	return fmt.Sprintf("%x", sha256.Sum256(contents)), nil
}

func pullRequestResult(pull github.PullRequest, branch, commit string) Result {
	return Result{
		PullRequestNumber: pull.Number,
		PullRequestURL:    pull.URL,
		Branch:            branch,
		CommitSHA:         commit,
	}
}

// Validate checks whether a request contains the values required to publish.
func (request Request) Validate() error {
	if strings.TrimSpace(request.Namespace) == "" {
		return errors.New("--namespace is required")
	}

	if strings.TrimSpace(request.Session) == "" || strings.TrimSpace(request.Branch) == "" || strings.TrimSpace(request.CommitMessage) == "" {
		return errors.New("publish requires --name, --branch, and --commit-message")
	}

	if request.Branch != strings.TrimSpace(request.Branch) {
		return errors.New("--branch must not start or end with whitespace")
	}

	if strings.TrimSpace(request.Title) == "" {
		return errors.New("--title must not be empty")
	}

	if request.Timeout < time.Second {
		return errors.New("--timeout must be at least one second")
	}

	return nil
}
