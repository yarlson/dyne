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

const maxResponseBodySize = 1 << 20

type Repository struct {
	Owner string
	Name  string
}

type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
}

type Client struct {
	api          *gh.Client
	pollInterval time.Duration
}

func New(token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("GitHub token is required")
	}
	httpClient := &http.Client{
		Transport: responseBodyLimitTransport{base: http.DefaultTransport},
		Timeout:   30 * time.Second,
	}
	api := gh.NewClient(httpClient).WithAuthToken(token)
	api.UserAgent = "agentctl"
	return &Client{
		api:          api,
		pollInterval: 2 * time.Second,
	}, nil
}

func ParseRepositoryURL(rawURL string) (Repository, error) {
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
	return Repository{Owner: parts[0], Name: name}, nil
}

func (c *Client) AuthenticatedUser(ctx context.Context) (User, error) {
	user, _, err := c.api.Users.Get(ctx, "")
	if err != nil {
		return User{}, fmt.Errorf("get authenticated GitHub user: %w", err)
	}
	if user.GetLogin() == "" || user.GetID() <= 0 {
		return User{}, errors.New("authenticated GitHub user has no login or numeric ID")
	}
	return User{Login: user.GetLogin(), ID: user.GetID()}, nil
}

func (c *Client) BranchCommit(ctx context.Context, repository Repository, branch string) (string, bool, error) {
	result, _, err := c.api.Git.GetRef(ctx, repository.Owner, repository.Name, "heads/"+branch)
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

func (c *Client) WaitBranchCommit(ctx context.Context, repository Repository, branch, expected string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		actual, exists, err := c.BranchCommit(waitCtx, repository, branch)
		if err != nil {
			return err
		}
		if exists {
			if actual != expected {
				return fmt.Errorf("GitHub branch %s points to %s instead of %s", branch, actual, expected)
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

func (c *Client) OpenPullRequest(ctx context.Context, repository Repository, base, branch string) (*PullRequest, error) {
	pulls, _, err := c.api.PullRequests.List(ctx, repository.Owner, repository.Name, &gh.PullRequestListOptions{
		State: "open",
		Head:  repository.Owner + ":" + branch,
		Base:  base,
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
	pull, err := pullRequest(pulls[0])
	if err != nil {
		return nil, err
	}
	return &pull, nil
}

func (c *Client) WaitOpenPullRequest(ctx context.Context, repository Repository, base, branch string, timeout time.Duration) (*PullRequest, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		pull, err := c.OpenPullRequest(waitCtx, repository, base, branch)
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

func (c *Client) CreatePullRequest(ctx context.Context, repository Repository, base, branch, title, body string, draft bool) (PullRequest, error) {
	result, _, err := c.api.PullRequests.Create(ctx, repository.Owner, repository.Name, &gh.NewPullRequest{
		Title: &title,
		Head:  &branch,
		Base:  &base,
		Body:  &body,
		Draft: &draft,
	})
	if err != nil {
		return PullRequest{}, fmt.Errorf("create GitHub pull request: %w", err)
	}
	return pullRequest(result)
}

func (u User) CommitName() string {
	return u.Login
}

func (u User) CommitEmail() string {
	return fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)
}

func pullRequest(result *gh.PullRequest) (PullRequest, error) {
	if result == nil || result.GetNumber() <= 0 || result.GetHTMLURL() == "" {
		return PullRequest{}, errors.New("GitHub returned a pull request without a number or URL")
	}
	return PullRequest{Number: result.GetNumber(), URL: result.GetHTMLURL()}, nil
}

func isNotFound(err error) bool {
	var responseErr *gh.ErrorResponse
	return errors.As(err, &responseErr) && responseErr.Response != nil && responseErr.Response.StatusCode == http.StatusNotFound
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
		}{Reader: io.LimitReader(response.Body, maxResponseBodySize), Closer: response.Body}
	}
	return response, nil
}
