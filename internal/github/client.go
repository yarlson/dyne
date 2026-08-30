package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v83/github"
)

const maxResponseBodyBytes = 1 << 20

// Repository identifies a parsed GitHub repository.
type Repository struct {
	owner string
	name  string
}

// PullRequest contains the GitHub identity of a pull request.
type PullRequest struct {
	// Number is the repository-local pull request number.
	Number int
	// URL is the pull request's GitHub web URL.
	URL string
}

// Client performs the GitHub operations required by publishing.
type Client struct {
	api          *gh.Client
	pollInterval time.Duration
}

// New returns an authenticated GitHub client with bounded requests and responses.
func New(token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("GitHub token is required")
	}

	httpClient := &http.Client{
		Transport: responseBodyLimitTransport{base: http.DefaultTransport},
		Timeout:   30 * time.Second,
	}
	api := gh.NewClient(httpClient).WithAuthToken(token)
	api.UserAgent = "dyne"

	return &Client{
		api:          api,
		pollInterval: 2 * time.Second,
	}, nil
}

// ParseRepository accepts an HTTPS github.com repository URL.
func ParseRepository(rawURL string) (Repository, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return Repository{}, fmt.Errorf("parse repository URL: %w", err)
	}

	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Repository{}, errors.New("publishing requires an HTTPS github.com repository URL")
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Repository{}, errors.New("repository URL must contain one owner and repository name")
	}

	name := strings.TrimSuffix(parts[1], ".git")
	if name == "" {
		return Repository{}, errors.New("repository URL has an empty repository name")
	}

	return Repository{owner: parts[0], name: name}, nil
}

// CommitAuthor returns the identity used for commits created by Dyne.
func CommitAuthor() (string, string) {
	return "dyne", "dyne@localhost"
}

// BranchCommitSHA returns a branch's commit SHA and whether the branch exists.
func (c *Client) BranchCommitSHA(ctx context.Context, repository Repository, branch string) (string, bool, error) {
	result, _, err := c.api.Git.GetRef(ctx, repository.owner, repository.name, "heads/"+branch)
	if isNotFound(err) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("get GitHub branch %s: %w", branch, err)
	}

	commit := result.GetObject().GetSHA()
	if commit == "" {
		return "", false, fmt.Errorf("GitHub branch %s has no commit", branch)
	}

	return commit, true, nil
}

// WaitForBranchCommit waits for a branch to appear and verifies that it points to the expected commit.
func (c *Client) WaitForBranchCommit(ctx context.Context, repository Repository, branch, expectedCommitSHA string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		actual, exists, err := c.BranchCommitSHA(waitCtx, repository, branch)
		if err != nil {
			return err
		}

		if exists {
			if actual != expectedCommitSHA {
				return fmt.Errorf("GitHub branch %s points to %s instead of %s", branch, actual, expectedCommitSHA)
			}

			return nil
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for GitHub branch %s: %w", branch, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// FindOpenPullRequest returns the matching open pull request, nil when none exists, and an error for multiple matches.
func (c *Client) FindOpenPullRequest(ctx context.Context, repository Repository, baseBranch, branch string) (*PullRequest, error) {
	pulls, _, err := c.api.PullRequests.List(ctx, repository.owner, repository.name, &gh.PullRequestListOptions{
		State: "open",
		Head:  repository.owner + ":" + branch,
		Base:  baseBranch,
		ListOptions: gh.ListOptions{
			PerPage: 2,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("find existing GitHub pull request: %w", err)
	}

	if len(pulls) == 0 {
		return nil, nil
	}

	if len(pulls) > 1 {
		return nil, errors.New("more than one open pull request uses the publish branch and base")
	}

	pull, err := pullRequestFromAPI(pulls[0])
	if err != nil {
		return nil, err
	}

	return &pull, nil
}

// WaitForOpenPullRequest waits for an open pull request and returns nil when the timeout expires.
func (c *Client) WaitForOpenPullRequest(ctx context.Context, repository Repository, baseBranch, branch string, timeout time.Duration) (*PullRequest, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		pull, err := c.FindOpenPullRequest(waitCtx, repository, baseBranch, branch)
		if err != nil {
			return nil, err
		}

		if pull != nil {
			return pull, nil
		}

		select {
		case <-waitCtx.Done():
			return nil, nil
		case <-ticker.C:
		}
	}
}

// CreatePullRequest opens a pull request from branch into the base branch.
func (c *Client) CreatePullRequest(ctx context.Context, repository Repository, baseBranch, branch, title, body string, draft bool) (PullRequest, error) {
	result, _, err := c.api.PullRequests.Create(ctx, repository.owner, repository.name, &gh.NewPullRequest{
		Title: &title,
		Head:  &branch,
		Base:  &baseBranch,
		Body:  &body,
		Draft: &draft,
	})
	if err != nil {
		return PullRequest{}, fmt.Errorf("create GitHub pull request: %w", err)
	}

	return pullRequestFromAPI(result)
}

func pullRequestFromAPI(result *gh.PullRequest) (PullRequest, error) {
	if result == nil || result.GetNumber() <= 0 || result.GetHTMLURL() == "" {
		return PullRequest{}, errors.New("GitHub returned a pull request without a number or URL")
	}

	return PullRequest{Number: result.GetNumber(), URL: result.GetHTMLURL()}, nil
}

func isNotFound(err error) bool {
	responseErr, ok := errors.AsType[*gh.ErrorResponse](err)

	return ok && responseErr.Response != nil && responseErr.Response.StatusCode == http.StatusNotFound
}

type responseBodyLimitTransport struct {
	base http.RoundTripper
}

func (t responseBodyLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}

	if response.Body != nil {
		response.Body = struct {
			io.Reader
			io.Closer
		}{Reader: io.LimitReader(response.Body, maxResponseBodyBytes), Closer: response.Body}
	}

	return response, nil
}
