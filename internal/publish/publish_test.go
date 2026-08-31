package publish

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/github"
	"github.com/yarlson/dyne/internal/session"
	"github.com/yarlson/dyne/internal/workload"
)

func TestPublishRecordsIntentBeforeCheckingOrChangingGitHub(t *testing.T) {
	var operations []string
	repository := newMemoryRepository()
	repository.created = func(Record) { operations = append(operations, "record intent") }
	runtime := runtimeStub{run: func(request workload.PublishRequest) (workload.PublishResult, error) {
		operations = append(operations, "publish branch")
		assert.Equal(t, "installation-token", request.RepositoryCredential)

		return workload.PublishResult{Branch: "yar/review", CommitSHA: validCommitSHA}, nil
	}}
	client := githubClientStub{
		findPull: func() (*github.PullRequest, error) {
			operations = append(operations, "find pull request")

			return nil, nil
		},
		branchCommit: func() (string, bool, error) {
			operations = append(operations, "find branch")

			return "", false, nil
		},
		createPull: func() (github.PullRequest, error) {
			operations = append(operations, "create pull request")

			return github.PullRequest{Number: 17, URL: "https://github.com/lokalise/ratchet-test-service/pull/17"}, nil
		},
	}
	control := newTestControl(t, repository, runtime, client)

	result, err := control.Publish(context.Background(), validRequest())
	require.NoError(t, err)
	assert.Equal(t, 17, result.PullRequestNumber)
	assert.Equal(t, StateCompleted, repository.records["review"].State)
	require.NotEmpty(t, operations)
	assert.Equal(t, "record intent", operations[0])
}

func TestPublishDoesNotClaimBranchThatPredatesDurableOwnership(t *testing.T) {
	repository := newMemoryRepository()
	runtimeCalls := 0
	runtime := runtimeStub{run: func(workload.PublishRequest) (workload.PublishResult, error) {
		runtimeCalls++

		return workload.PublishResult{}, nil
	}}
	client := githubClientStub{
		findPull:     func() (*github.PullRequest, error) { return nil, nil },
		branchCommit: func() (string, bool, error) { return validCommitSHA, true, nil },
	}
	control := newTestControl(t, repository, runtime, client)

	_, err := control.Publish(context.Background(), validRequest())
	assert.Equal(t, ErrorConflict, ErrorKindOf(err))
	assert.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, StateConflicted, repository.records["review"].State)
	assert.Equal(t, 0, runtimeCalls)
}

func TestPublishRecoversPullRequestAfterAmbiguousCreateFailure(t *testing.T) {
	createFailure := errors.New("connection reset")
	repository := newMemoryRepository()
	runtime := runtimeStub{run: func(workload.PublishRequest) (workload.PublishResult, error) {
		return workload.PublishResult{Branch: "yar/review", CommitSHA: validCommitSHA}, nil
	}}
	findCalls := 0
	client := githubClientStub{
		findPull: func() (*github.PullRequest, error) {
			findCalls++

			return nil, nil
		},
		branchCommit: func() (string, bool, error) { return "", false, nil },
		createPull:   func() (github.PullRequest, error) { return github.PullRequest{}, createFailure },
		waitPull: func() (*github.PullRequest, error) {
			return &github.PullRequest{Number: 23, URL: "https://github.com/lokalise/ratchet-test-service/pull/23"}, nil
		},
	}
	control := newTestControl(t, repository, runtime, client)

	result, err := control.Publish(context.Background(), validRequest())
	require.NoError(t, err)
	assert.Equal(t, 23, result.PullRequestNumber)
	assert.GreaterOrEqual(t, findCalls, 2)
	assert.Equal(t, StateCompleted, repository.records["review"].State)
}

func TestPublishDoesNotClaimBranchAfterAmbiguousPublisherFailure(t *testing.T) {
	repository := newMemoryRepository()
	publishFailure := errors.New("publisher connection lost")
	runtime := runtimeStub{run: func(workload.PublishRequest) (workload.PublishResult, error) {
		return workload.PublishResult{}, publishFailure
	}}
	branchChecks := 0
	client := githubClientStub{
		findPull: func() (*github.PullRequest, error) { return nil, nil },
		branchCommit: func() (string, bool, error) {
			branchChecks++
			if branchChecks < 3 {
				return "", false, nil
			}

			return validCommitSHA, true, nil
		},
	}
	control := newTestControl(t, repository, runtime, client)

	_, err := control.Publish(context.Background(), validRequest())
	require.ErrorIs(t, err, publishFailure)
	assert.Equal(t, StateReady, repository.records["review"].State)
	assert.Empty(t, repository.records["review"].CommitSHA)
}

func TestPublishRemovesFailedExecutionBeforeAllowingRetry(t *testing.T) {
	repository := newMemoryRepository()
	var operations []string
	runtime := runtimeStub{
		run: func(workload.PublishRequest) (workload.PublishResult, error) {
			operations = append(operations, "run publisher")

			return workload.PublishResult{}, fmt.Errorf("container exited: %w", workload.ErrExecutionFailed)
		},
		delete: func(string) error {
			operations = append(operations, "delete publisher")

			return nil
		},
	}
	client := githubClientStub{
		findPull:     func() (*github.PullRequest, error) { return nil, nil },
		branchCommit: func() (string, bool, error) { return "", false, nil },
	}
	control := newTestControl(t, repository, runtime, client)

	_, err := control.Publish(context.Background(), validRequest())
	require.ErrorIs(t, err, workload.ErrExecutionFailed)
	assert.Equal(t, []string{"run publisher", "delete publisher"}, operations)
	assert.Equal(t, StateReady, repository.records["review"].State)
}

func TestPublishRecoversOwnedBranchAfterTerminalPublisherFailure(t *testing.T) {
	repository := newMemoryRepository()
	deleted := 0
	runtime := runtimeStub{
		run: func(workload.PublishRequest) (workload.PublishResult, error) {
			return workload.PublishResult{Branch: "yar/review", CommitSHA: validCommitSHA}, fmt.Errorf("lost completion: %w", workload.ErrExecutionFailed)
		},
		delete: func(string) error {
			deleted++

			return nil
		},
	}
	branchChecks := 0
	client := githubClientStub{
		findPull: func() (*github.PullRequest, error) { return nil, nil },
		branchCommit: func() (string, bool, error) {
			branchChecks++
			if branchChecks == 1 {
				return "", false, nil
			}

			return validCommitSHA, true, nil
		},
		createPull: func() (github.PullRequest, error) {
			return github.PullRequest{Number: 31, URL: "https://github.com/lokalise/ratchet-test-service/pull/31"}, nil
		},
	}
	control := newTestControl(t, repository, runtime, client)

	result, err := control.Publish(context.Background(), validRequest())
	require.NoError(t, err)
	assert.Equal(t, 31, result.PullRequestNumber)
	assert.Equal(t, validCommitSHA, result.CommitSHA)
	assert.Equal(t, StateCompleted, repository.records["review"].State)
	assert.Equal(t, 1, deleted)
}

func TestPublishRejectsDifferentBranchCommitAfterTerminalPublisherFailure(t *testing.T) {
	repository := newMemoryRepository()
	deleted := 0
	runtime := runtimeStub{
		run: func(workload.PublishRequest) (workload.PublishResult, error) {
			return workload.PublishResult{Branch: "yar/review", CommitSHA: validCommitSHA}, fmt.Errorf("lost completion: %w", workload.ErrExecutionFailed)
		},
		delete: func(string) error {
			deleted++

			return nil
		},
	}
	branchChecks := 0
	client := githubClientStub{
		findPull: func() (*github.PullRequest, error) { return nil, nil },
		branchCommit: func() (string, bool, error) {
			branchChecks++
			if branchChecks == 1 {
				return "", false, nil
			}

			return "1111111111111111111111111111111111111111", true, nil
		},
	}
	control := newTestControl(t, repository, runtime, client)

	_, err := control.Publish(context.Background(), validRequest())
	assert.Equal(t, ErrorConflict, ErrorKindOf(err))
	assert.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, StateConflicted, repository.records["review"].State)
	assert.Equal(t, 1, deleted)
}

func TestPublishRetryFinishesCleanupFromDurablePullRequestState(t *testing.T) {
	repository := newMemoryRepository()
	runtimeFailure := errors.New("cluster unavailable")
	runtime := runtimeStub{
		run: func(workload.PublishRequest) (workload.PublishResult, error) {
			return workload.PublishResult{Branch: "yar/review", CommitSHA: validCommitSHA}, nil
		},
		delete: func(string) error { return runtimeFailure },
	}
	pull := github.PullRequest{Number: 17, URL: "https://github.com/lokalise/ratchet-test-service/pull/17"}
	client := githubClientStub{
		findPull:     func() (*github.PullRequest, error) { return nil, nil },
		branchCommit: func() (string, bool, error) { return "", false, nil },
		createPull:   func() (github.PullRequest, error) { return pull, nil },
	}
	control := newTestControl(t, repository, runtime, client)

	_, err := control.Publish(context.Background(), validRequest())
	require.ErrorIs(t, err, runtimeFailure)
	assert.Equal(t, StatePullRequestCreated, repository.records["review"].State)

	restartedClient := githubClientStub{findPull: func() (*github.PullRequest, error) { return &pull, nil }}
	restarted := newTestControl(t, repository, runtimeStub{}, restartedClient)
	result, err := restarted.Publish(context.Background(), validRequest())
	require.NoError(t, err)
	assert.Equal(t, 17, result.PullRequestNumber)
	assert.Equal(t, StateCompleted, repository.records["review"].State)
}

const validCommitSHA = "9a4484441215661904e02a807adf5034d13f5bbe"

func validRequest() Request {
	return Request{
		Session: "review", Branch: "yar/review", CommitMessage: "Fix the README link",
		Draft: true, Timeout: time.Minute,
	}
}

func newTestControl(t *testing.T, repository *memoryRepository, runtime runtimeStub, client githubClientStub) *Control {
	t.Helper()
	control, err := New(Config{
		Sessions: sessionStub{}, Repository: repository, Runtime: runtime,
		RepositoryAuth: tokenProviderStub("installation-token"),
	})
	require.NoError(t, err)
	control.newGitHubClient = func(string) (githubClient, error) { return client, nil }
	control.now = func() time.Time { return time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC) }

	return control
}

type sessionStub struct{}

func (sessionStub) PreparePublication(context.Context, string) (*session.Publication, error) {
	return &session.Publication{Source: session.PublicationSource{
		Repository: "https://github.com/lokalise/ratchet-test-service", InitialRef: "main",
		Image:       "coding-agent:test",
		PullRequest: []byte(`{"title":"Fix link","body":"Updates the README."}`),
	}}, nil
}

type tokenProviderStub string

func (t tokenProviderStub) InstallationToken(context.Context) (string, error) { return string(t), nil }

type runtimeStub struct {
	run    func(workload.PublishRequest) (workload.PublishResult, error)
	delete func(string) error
}

func (runtimeStub) Scope() string { return "coding-agents" }
func (r runtimeStub) RunPublisher(_ context.Context, request workload.PublishRequest) (workload.PublishResult, error) {
	if r.run != nil {
		return r.run(request)
	}

	return workload.PublishResult{}, nil
}

func (r runtimeStub) DeletePublisher(_ context.Context, sessionName string) error {
	if r.delete != nil {
		return r.delete(sessionName)
	}

	return nil
}

type githubClientStub struct {
	branchCommit func() (string, bool, error)
	findPull     func() (*github.PullRequest, error)
	waitPull     func() (*github.PullRequest, error)
	createPull   func() (github.PullRequest, error)
}

func (c githubClientStub) BranchCommitSHA(context.Context, github.Repository, string) (string, bool, error) {
	if c.branchCommit != nil {
		return c.branchCommit()
	}

	return "", false, nil
}

func (githubClientStub) WaitForBranchCommit(context.Context, github.Repository, string, string, time.Duration) error {
	return nil
}

func (c githubClientStub) FindOpenPullRequest(context.Context, github.Repository, string, string) (*github.PullRequest, error) {
	if c.findPull != nil {
		return c.findPull()
	}

	return nil, nil
}

func (c githubClientStub) WaitForOpenPullRequest(context.Context, github.Repository, string, string, time.Duration) (*github.PullRequest, error) {
	if c.waitPull != nil {
		return c.waitPull()
	}

	return nil, nil
}

func (c githubClientStub) CreatePullRequest(context.Context, github.Repository, string, string, string, string, bool) (github.PullRequest, error) {
	if c.createPull != nil {
		return c.createPull()
	}

	return github.PullRequest{}, nil
}

type memoryRepository struct {
	records map[string]Record
	created func(Record)
}

func newMemoryRepository() *memoryRepository { return &memoryRepository{records: map[string]Record{}} }

func (r *memoryRepository) Create(_ context.Context, record Record) (Record, error) {
	if _, exists := r.records[record.Session]; exists {
		return Record{}, ErrConflict
	}

	record.Revision = 1
	r.records[record.Session] = record
	if r.created != nil {
		r.created(record)
	}

	return record, nil
}

func (r *memoryRepository) Get(_ context.Context, sessionName string) (Record, error) {
	record, exists := r.records[sessionName]
	if !exists {
		return Record{}, ErrNotFound
	}

	return record, nil
}

func (r *memoryRepository) Update(_ context.Context, record Record) (Record, error) {
	current, exists := r.records[record.Session]
	if !exists {
		return Record{}, ErrNotFound
	}

	if current.Revision != record.Revision {
		return Record{}, ErrConflict
	}

	record.Revision++
	r.records[record.Session] = record

	return record, nil
}
