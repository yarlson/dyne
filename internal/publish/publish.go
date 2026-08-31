package publish

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/yarlson/dyne/internal/github"
	"github.com/yarlson/dyne/internal/session"
	"github.com/yarlson/dyne/internal/workload"
)

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Request defines one idempotent workspace publish operation.
type Request struct {
	Session       string
	Branch        string
	BaseBranch    string
	CommitMessage string
	Draft         bool
	Timeout       time.Duration
}

// Result identifies the pull request, branch, and commit produced by publishing.
type Result struct {
	PullRequestNumber int
	PullRequestURL    string
	Branch            string
	CommitSHA         string
}

// State is the durable progress of one publish operation.
type State string

const (
	// StatePending means the intent is durable but remote ownership is not established.
	StatePending State = "pending"
	// StateReady means GitHub confirmed that the requested branch and pull request were absent.
	StateReady State = "ready"
	// StateBranchPublished means the owned remote branch and commit are durable.
	StateBranchPublished State = "branch_published"
	// StatePullRequestCreated means GitHub has the owned pull request but runtime cleanup remains.
	StatePullRequestCreated State = "pull_request_created"
	// StateCompleted means the pull request exists and disposable publisher resources are removed.
	StateCompleted State = "completed"
	// StateConflicted means the requested GitHub names existed before ownership was established.
	StateConflicted State = "conflicted"
)

// Record is the durable publish intent, progress, and result for one session.
type Record struct {
	Session           string
	IntentID          string
	Request           Request
	Repository        string
	Image             string
	Title             string
	Body              string
	Change            *session.ChangeArtifact
	State             State
	CommitSHA         string
	PullRequestNumber int
	PullRequestURL    string
	Failure           string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Revision          int64
}

// Runtime executes isolated workspace publication.
type Runtime interface {
	Scope() string
	RunPublisher(context.Context, workload.PublishRequest) (workload.PublishResult, error)
	DeletePublisher(context.Context, string) error
}

type sessionOperations interface {
	PreparePublication(context.Context, string) (*session.Publication, error)
}

type githubClient interface {
	BranchCommitSHA(context.Context, github.Repository, string) (string, bool, error)
	WaitForBranchCommit(context.Context, github.Repository, string, string, time.Duration) error
	FindOpenPullRequest(context.Context, github.Repository, string, string) (*github.PullRequest, error)
	WaitForOpenPullRequest(context.Context, github.Repository, string, string, time.Duration) (*github.PullRequest, error)
	CreatePullRequest(context.Context, github.Repository, string, string, string, string, bool) (github.PullRequest, error)
}

// ErrorKind classifies a publishing failure for entrypoints.
type ErrorKind string

const (
	// ErrorInvalid identifies an invalid publish request.
	ErrorInvalid ErrorKind = "invalid"
	// ErrorConflict identifies a branch, pull request, or intent ownership conflict.
	ErrorConflict ErrorKind = "conflict"
	// ErrorUnavailable identifies a storage, runtime, or GitHub failure.
	ErrorUnavailable ErrorKind = "unavailable"
)

type operationError struct {
	kind    ErrorKind
	message string
	cause   error
}

func (e *operationError) Error() string { return e.message }
func (e *operationError) Unwrap() error { return e.cause }

// ErrorKindOf returns the stable classification of a publishing failure.
func ErrorKindOf(err error) ErrorKind {
	var target *operationError
	if errors.As(err, &target) {
		return target.kind
	}

	return ""
}

// Config contains durable publishing dependencies.
type Config struct {
	Sessions       sessionOperations
	Repository     Repository
	Runtime        Runtime
	RepositoryAuth session.RepositoryTokenProvider
}

// Control publishes eligible session workspaces.
type Control struct {
	sessions        sessionOperations
	repository      Repository
	runtime         Runtime
	repositoryAuth  session.RepositoryTokenProvider
	newGitHubClient func(string) (githubClient, error)
	now             func() time.Time
}

// New creates publishing control with durable state and explicit integrations.
func New(config Config) (*Control, error) {
	if config.Sessions == nil {
		return nil, errors.New("session control is required")
	}

	if config.Repository == nil {
		return nil, errors.New("publish repository is required")
	}

	if config.Runtime == nil {
		return nil, errors.New("publisher runtime is required")
	}

	if config.RepositoryAuth == nil {
		return nil, errors.New("repository credential provider is required")
	}

	return &Control{
		sessions: config.Sessions, repository: config.Repository, runtime: config.Runtime,
		repositoryAuth:  config.RepositoryAuth,
		newGitHubClient: func(token string) (githubClient, error) { return github.New(token) },
		now:             time.Now,
	}, nil
}

// Publish creates or resumes one durable publish operation.
func (c *Control) Publish(ctx context.Context, request Request) (Result, error) {
	if err := request.Validate(); err != nil {
		return Result{}, &operationError{kind: ErrorInvalid, message: err.Error(), cause: err}
	}

	result, err := c.publish(ctx, request)
	if err != nil {
		kind := ErrorUnavailable
		if errors.Is(err, ErrConflict) {
			kind = ErrorConflict
		}

		return Result{}, &operationError{kind: kind, message: "publish session failed", cause: err}
	}

	return result, nil
}

func (c *Control) publish(ctx context.Context, request Request) (Result, error) {
	publication, err := c.sessions.PreparePublication(ctx, request.Session)
	if err != nil {
		return Result{}, err
	}
	defer publication.Close()
	source := publication.Source

	if request.BaseBranch == "" {
		request.BaseBranch = source.InitialRef
	}

	repository, err := github.ParseRepository(source.Repository)
	if err != nil {
		return Result{}, err
	}

	metadata, err := pullRequestMetadata(source.PullRequest)
	if err != nil {
		return Result{}, err
	}

	intentID := publishIntentID(request, c.runtime.Scope(), source.Repository)
	now := c.now().UTC()
	record := Record{
		Session: request.Session, IntentID: intentID, Request: request,
		Repository: source.Repository, Image: source.Image, Title: metadata.Title, Body: metadata.Body,
		Change: source.Change,
		State:  StatePending, CreatedAt: now, UpdatedAt: now,
	}
	record, err = c.createOrLoad(ctx, record)
	if err != nil {
		return Result{}, err
	}

	if record.State == StateCompleted {
		return recordResult(record), nil
	}

	if record.State == StateConflicted {
		if err := c.runtime.DeletePublisher(ctx, record.Session); err != nil {
			return Result{}, fmt.Errorf("clean conflicted publisher execution: %w", err)
		}

		return Result{}, fmt.Errorf("%w: %s", ErrConflict, record.Failure)
	}

	token, err := c.repositoryAuth.InstallationToken(ctx)
	if err != nil {
		return Result{}, err
	}

	client, err := c.newGitHubClient(token)
	if err != nil {
		return Result{}, err
	}

	if record.State == StatePending {
		record, err = c.establishOwnership(ctx, record, repository, client)
		if err != nil {
			return Result{}, err
		}
	}

	if pull, err := client.FindOpenPullRequest(ctx, repository, request.BaseBranch, request.Branch); err != nil {
		return Result{}, err
	} else if pull != nil {
		record.PullRequestNumber = pull.Number
		record.PullRequestURL = pull.URL
		record.State = StatePullRequestCreated
		if record, err = c.save(ctx, record); err != nil {
			return Result{}, err
		}

		return c.finish(ctx, record)
	}

	if record.State == StateReady {
		record, err = c.publishBranch(ctx, record, token, repository, client)
		if err != nil {
			return Result{}, err
		}
	}

	if err := client.WaitForBranchCommit(ctx, repository, request.Branch, record.CommitSHA, 30*time.Second); err != nil {
		return Result{}, err
	}

	pull, err := client.CreatePullRequest(
		ctx, repository, request.BaseBranch, request.Branch, record.Title, record.Body, request.Draft,
	)
	if err != nil {
		existing, findErr := client.WaitForOpenPullRequest(ctx, repository, request.BaseBranch, request.Branch, 10*time.Second)
		if findErr != nil {
			return Result{}, errors.Join(err, findErr)
		}

		if existing == nil {
			return Result{}, err
		}

		pull = *existing
	}

	record.PullRequestNumber = pull.Number
	record.PullRequestURL = pull.URL
	record.State = StatePullRequestCreated
	record, err = c.save(ctx, record)
	if err != nil {
		return Result{}, err
	}

	return c.finish(ctx, record)
}

func (c *Control) createOrLoad(ctx context.Context, record Record) (Record, error) {
	created, err := c.repository.Create(ctx, record)
	if err == nil {
		return created, nil
	}

	if !errors.Is(err, ErrConflict) {
		return Record{}, err
	}

	existing, err := c.repository.Get(ctx, record.Session)
	if err != nil {
		return Record{}, err
	}

	if existing.IntentID != record.IntentID {
		return Record{}, fmt.Errorf("%w: session already has a different publish intent", ErrConflict)
	}

	return existing, nil
}

func (c *Control) establishOwnership(
	ctx context.Context,
	record Record,
	repository github.Repository,
	client githubClient,
) (Record, error) {
	pull, err := client.FindOpenPullRequest(ctx, repository, record.Request.BaseBranch, record.Request.Branch)
	if err != nil {
		return Record{}, err
	}

	_, branchExists, err := client.BranchCommitSHA(ctx, repository, record.Request.Branch)
	if err != nil {
		return Record{}, err
	}

	if pull != nil || branchExists {
		record.State = StateConflicted
		record.Failure = fmt.Sprintf("remote branch %s or its pull request existed before publish ownership was established", record.Request.Branch)
		if _, saveErr := c.save(ctx, record); saveErr != nil {
			return Record{}, saveErr
		}

		return Record{}, fmt.Errorf("%w: %s", ErrConflict, record.Failure)
	}

	record.State = StateReady

	return c.save(ctx, record)
}

func (c *Control) publishBranch(
	ctx context.Context,
	record Record,
	token string,
	repository github.Repository,
	client githubClient,
) (Record, error) {
	authorName, authorEmail := github.CommitAuthor()
	result, err := c.runtime.RunPublisher(ctx, workload.PublishRequest{
		Session: record.Session, IntentID: record.IntentID, Image: record.Image,
		Repository: record.Repository, RepositoryCredential: token,
		BaseRef: record.Request.BaseBranch, Branch: record.Request.Branch,
		CommitMessage: record.Request.CommitMessage, AuthorName: authorName, AuthorEmail: authorEmail,
		Change: workloadChangeArtifact(record.Change), Timeout: record.Request.Timeout,
	})
	if err != nil {
		if errors.Is(err, workload.ErrExecutionFailed) {
			return c.recoverFailedPublisher(ctx, record, repository, client, result, err)
		}

		return Record{}, err
	}

	if result.Branch != record.Request.Branch || !commitSHAPattern.MatchString(result.CommitSHA) {
		return Record{}, errors.New("publisher reported an invalid branch or commit")
	}

	record.CommitSHA = result.CommitSHA
	record.State = StateBranchPublished

	return c.save(ctx, record)
}

func workloadChangeArtifact(artifact *session.ChangeArtifact) *workload.ChangeArtifact {
	if artifact == nil {
		return nil
	}

	return &workload.ChangeArtifact{SHA256: artifact.SHA256, Bytes: artifact.Bytes}
}

func (c *Control) recoverFailedPublisher(
	ctx context.Context,
	record Record,
	repository github.Repository,
	client githubClient,
	result workload.PublishResult,
	executionFailure error,
) (Record, error) {
	remoteCommit, exists, err := client.BranchCommitSHA(ctx, repository, record.Request.Branch)
	if err != nil {
		return Record{}, errors.Join(executionFailure, fmt.Errorf("check branch after failed publisher execution: %w", err))
	}

	if !exists {
		if err := c.runtime.DeletePublisher(ctx, record.Session); err != nil {
			return Record{}, errors.Join(executionFailure, fmt.Errorf("clean failed publisher execution: %w", err))
		}

		return Record{}, executionFailure
	}

	if result.Branch == record.Request.Branch && commitSHAPattern.MatchString(result.CommitSHA) && remoteCommit == result.CommitSHA {
		record.CommitSHA = result.CommitSHA
		record.State = StateBranchPublished

		recovered, err := c.save(ctx, record)
		if err != nil {
			return Record{}, errors.Join(executionFailure, err)
		}

		return recovered, nil
	}

	record.State = StateConflicted
	record.Failure = fmt.Sprintf(
		"remote branch %s changed while the publisher execution was running", record.Request.Branch,
	)
	if _, err := c.save(ctx, record); err != nil {
		return Record{}, errors.Join(executionFailure, err)
	}

	conflict := fmt.Errorf("%w: %s", ErrConflict, record.Failure)
	if err := c.runtime.DeletePublisher(ctx, record.Session); err != nil {
		return Record{}, errors.Join(conflict, fmt.Errorf("clean conflicted publisher execution: %w", err))
	}

	return Record{}, conflict
}

func (c *Control) finish(ctx context.Context, record Record) (Result, error) {
	if err := c.runtime.DeletePublisher(ctx, record.Session); err != nil {
		return Result{}, fmt.Errorf("pull request created at %s; clean publisher execution: %w", record.PullRequestURL, err)
	}

	record.State = StateCompleted
	record, err := c.save(ctx, record)
	if err != nil {
		return Result{}, err
	}

	return recordResult(record), nil
}

func (c *Control) save(ctx context.Context, record Record) (Record, error) {
	record.UpdatedAt = c.now().UTC()

	return c.repository.Update(ctx, record)
}

type pullMetadata struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func pullRequestMetadata(contents json.RawMessage) (pullMetadata, error) {
	var metadata pullMetadata
	if err := json.Unmarshal(contents, &metadata); err != nil {
		return metadata, fmt.Errorf("decode pull request artifact: %w", err)
	}

	if strings.TrimSpace(metadata.Title) == "" || strings.TrimSpace(metadata.Body) == "" {
		return metadata, errors.New("pull request artifact requires a title and body")
	}

	return metadata, nil
}

func publishIntentID(request Request, scope, repository string) string {
	contents, _ := json.Marshal(struct {
		Scope      string
		Repository string
		Request    Request
	}{Scope: scope, Repository: repository, Request: request})

	return fmt.Sprintf("%x", sha256.Sum256(contents))
}

func recordResult(record Record) Result {
	return Result{
		PullRequestNumber: record.PullRequestNumber, PullRequestURL: record.PullRequestURL,
		Branch: record.Request.Branch, CommitSHA: record.CommitSHA,
	}
}

// Validate checks whether a request contains the values required to publish.
func (request Request) Validate() error {
	if strings.TrimSpace(request.Session) == "" || strings.TrimSpace(request.Branch) == "" || strings.TrimSpace(request.CommitMessage) == "" {
		return errors.New("session name, branch, and commit message are required")
	}

	if request.Branch != strings.TrimSpace(request.Branch) {
		return errors.New("branch must not start or end with whitespace")
	}

	if request.Timeout < time.Second {
		return errors.New("timeout must be at least one second")
	}

	return nil
}
