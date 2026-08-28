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
	// Base is the pull request target and defaults to the session's initial ref.
	Base string
	// CommitMessage is the message used for the workspace commit.
	CommitMessage string
	// Title is the pull request title.
	Title string
	// Body is the pull request description.
	Body string
	// Draft controls whether GitHub creates a draft pull request.
	Draft bool
	// Timeout bounds the publisher Job and waits for its result.
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
	// Commit is the published commit SHA when this run created the branch.
	Commit string
}

// Run publishes an eligible session workspace and opens or recovers its pull request.
func Run(ctx context.Context, cluster *kubernetes.Client, request Request) (Result, error) {
	if err := request.Validate(); err != nil {
		return Result{}, err
	}
	source, err := cluster.PublishSource(ctx, request.Namespace, request.Session)
	if err != nil {
		return Result{}, err
	}
	if request.Base == "" {
		request.Base = source.Base
	}
	repository, err := github.ParseRepository(source.Repository)
	if err != nil {
		return Result{}, err
	}
	token, err := cluster.GitHubToken(ctx, request.Namespace)
	if err != nil {
		return Result{}, err
	}
	githubClient, err := github.New(token)
	if err != nil {
		return Result{}, err
	}
	intent, err := publishIntent(request, source.Repository)
	if err != nil {
		return Result{}, err
	}
	publisherIntent, publisherExists, err := cluster.PublisherIntent(ctx, request.Namespace, request.Session)
	if err != nil {
		return Result{}, err
	}
	if publisherExists && publisherIntent != intent {
		return Result{}, errors.New("publisher Job belongs to a different publish request")
	}
	existingPull, err := githubClient.OpenPullRequest(ctx, repository, request.Base, request.Branch)
	if err != nil {
		return Result{}, err
	}
	if existingPull != nil {
		if err := finishExistingPublisher(ctx, cluster, request, intent, publisherExists, existingPull.URL); err != nil {
			return Result{}, err
		}
		return pullRequestResult(*existingPull, request.Branch, ""), nil
	}
	remoteCommit, branchExists, err := githubClient.BranchCommit(ctx, repository, request.Branch)
	if err != nil {
		return Result{}, err
	}
	if branchExists && !publisherExists {
		return Result{}, fmt.Errorf("remote branch %s already exists and is not owned by this publish request", request.Branch)
	}
	if branchExists {
		result, err := cluster.WaitPublisher(ctx, request.Namespace, request.Session, intent, request.Timeout)
		if err != nil {
			return Result{}, err
		}
		if result.Branch != request.Branch || result.Commit != remoteCommit {
			return Result{}, fmt.Errorf("publisher result %s at %s does not match remote branch %s at %s", result.Branch, result.Commit, request.Branch, remoteCommit)
		}
	} else {
		remoteCommit, err = runPublisher(ctx, cluster, githubClient, repository, source, request, intent)
		if err != nil {
			return Result{}, err
		}
	}
	pull, err := githubClient.CreatePullRequest(ctx, repository, request.Base, request.Branch, request.Title, request.Body, request.Draft)
	if err != nil {
		existingPull, findErr := githubClient.WaitOpenPullRequest(ctx, repository, request.Base, request.Branch, 10*time.Second)
		if findErr != nil {
			return Result{}, errors.Join(err, findErr)
		}
		if existingPull == nil {
			return Result{}, err
		}
		pull = *existingPull
	}
	if err := cluster.DeletePublisher(ctx, request.Namespace, request.Session); err != nil {
		return Result{}, fmt.Errorf("pull request created at %s; clean publisher Job: %w", pull.URL, err)
	}
	return pullRequestResult(pull, request.Branch, remoteCommit), nil
}

func finishExistingPublisher(ctx context.Context, cluster *kubernetes.Client, request Request, intent string, publisherExists bool, pullRequestURL string) error {
	if !publisherExists {
		return nil
	}
	if _, err := cluster.WaitPublisher(ctx, request.Namespace, request.Session, intent, request.Timeout); err != nil {
		return fmt.Errorf("pull request already exists at %s; wait for publisher Job: %w", pullRequestURL, err)
	}
	if err := cluster.DeletePublisher(ctx, request.Namespace, request.Session); err != nil {
		return fmt.Errorf("pull request already exists at %s; clean publisher Job: %w", pullRequestURL, err)
	}
	return nil
}

func runPublisher(ctx context.Context, cluster *kubernetes.Client, githubClient *github.Client, repository github.Repository, source kubernetes.PublishSource, request Request, intent string) (string, error) {
	authorName, authorEmail, err := githubClient.CommitAuthor(ctx)
	if err != nil {
		return "", err
	}
	result, err := cluster.RunPublisher(ctx, kubernetes.PublishRequest{
		Namespace:     request.Namespace,
		Session:       request.Session,
		Intent:        intent,
		Repository:    source.Repository,
		Base:          request.Base,
		Branch:        request.Branch,
		CommitMessage: request.CommitMessage,
		AuthorName:    authorName,
		AuthorEmail:   authorEmail,
		Image:         source.Image,
		Workspace:     source.WorkspaceClaim,
		Timeout:       request.Timeout,
	})
	if err != nil {
		_, branchExists, branchErr := githubClient.BranchCommit(ctx, repository, request.Branch)
		if branchErr != nil {
			return "", errors.Join(err, branchErr)
		}
		if branchExists {
			return "", err
		}
		return "", errors.Join(err, cluster.DeletePublisher(ctx, request.Namespace, request.Session))
	}
	if result.Branch != request.Branch {
		return "", fmt.Errorf("publisher reported branch %s, want %s", result.Branch, request.Branch)
	}
	if err := githubClient.WaitBranchCommit(ctx, repository, request.Branch, result.Commit, 30*time.Second); err != nil {
		return "", err
	}
	return result.Commit, nil
}

func publishIntent(request Request, repository string) (string, error) {
	contents, err := json.Marshal(struct {
		Namespace     string `json:"namespace"`
		Session       string `json:"session"`
		Repository    string `json:"repository"`
		Base          string `json:"base"`
		Branch        string `json:"branch"`
		CommitMessage string `json:"commitMessage"`
		Title         string `json:"title"`
		Body          string `json:"body"`
		Draft         bool   `json:"draft"`
	}{
		Namespace:     request.Namespace,
		Session:       request.Session,
		Repository:    repository,
		Base:          request.Base,
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
		Commit:            commit,
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
