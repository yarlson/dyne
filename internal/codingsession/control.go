package codingsession

import (
	"context"
	"errors"
	"time"

	"coding-agent-k8s/internal/kubernetes"
	"coding-agent-k8s/internal/publish"
	"coding-agent-k8s/internal/sessionmanifest"
)

const sessionReadyTimeout = 2 * time.Minute

type sessionCluster interface {
	Apply(context.Context, []byte) error
	CheckSessionModeAvailable(context.Context, string, string, sessionmanifest.Mode) error
	SessionStatus(context.Context, string, string) ([]kubernetes.ResourceStatus, error)
	StreamPodLogs(context.Context, string, string, string, bool) error
	ExecPod(context.Context, string, string, string, []string, bool) error
	StopSession(context.Context, string, string) error
	ResumeSession(context.Context, string, string) error
	DeleteSession(context.Context, string, string) error
	DestroySession(context.Context, string, string) error
	WaitForReadyPod(context.Context, string, string, time.Duration) (string, error)
	NewestPodName(context.Context, string, string) (string, error)
}

type sessionPublisher func(context.Context, publish.Request) (publish.Result, error)

// Control performs complete coding-session operations.
type Control struct {
	cluster        sessionCluster
	publishSession sessionPublisher
}

// New returns coding-session control for one Kubernetes context.
func New(contextName string, streams Streams) (*Control, error) {
	if streams.Input == nil || streams.Output == nil || streams.ErrorOutput == nil {
		return nil, errors.New("input, output, and error output streams are required")
	}

	cluster, err := kubernetes.New(contextName, streams.Input, streams.Output, streams.ErrorOutput)
	if err != nil {
		return nil, err
	}

	return &Control{
		cluster: cluster,
		publishSession: func(ctx context.Context, request publish.Request) (publish.Result, error) {
			return publish.Session(ctx, cluster, request)
		},
	}, nil
}

// Bootstrap prepares a namespace and its requested credentials for coding sessions.
func (c *Control) Bootstrap(ctx context.Context, request BootstrapRequest) error {
	manifest, err := request.manifest()
	if err != nil {
		return err
	}

	return c.cluster.Apply(ctx, manifest)
}

// Start creates a coding session with the requested lifecycle and storage behavior.
func (c *Control) Start(ctx context.Context, request StartRequest) error {
	manifest, mode, err := request.manifest()
	if err != nil {
		return err
	}

	if err := c.cluster.CheckSessionModeAvailable(ctx, request.Target.Namespace, request.Target.Name, mode); err != nil {
		return err
	}

	return c.cluster.Apply(ctx, manifest)
}

// Status returns the resources owned by a coding session.
func (c *Control) Status(ctx context.Context, target Target) (Status, error) {
	if err := target.validate(); err != nil {
		return Status{}, err
	}

	resources, err := c.cluster.SessionStatus(ctx, target.Namespace, target.Name)
	if err != nil {
		return Status{}, err
	}

	status := Status{Resources: make([]ResourceStatus, len(resources))}
	for i, resource := range resources {
		status.Resources[i] = ResourceStatus{
			Kind:  resource.Kind,
			Name:  resource.Name,
			Ready: resource.Ready,
			State: resource.State,
		}
	}

	return status, nil
}

// StreamLogs writes logs from a coding session's current execution attempt.
func (c *Control) StreamLogs(ctx context.Context, request LogRequest) error {
	if err := request.Target.validate(); err != nil {
		return err
	}

	pod, err := c.cluster.NewestPodName(ctx, request.Target.Namespace, request.Target.Name)
	if err != nil {
		return err
	}

	return c.cluster.StreamPodLogs(ctx, request.Target.Namespace, pod, "agent", request.Follow)
}

// RunTask submits one task to a ready coding session.
func (c *Control) RunTask(ctx context.Context, request TaskRequest) error {
	if err := request.Target.validate(); err != nil {
		return err
	}

	pod, err := c.cluster.WaitForReadyPod(ctx, request.Target.Namespace, request.Target.Name, sessionReadyTimeout)
	if err != nil {
		return err
	}

	command := []string{"/usr/local/bin/agent-entrypoint", "task"}
	if request.ResumeLast {
		command = append(command, "--resume-last")
	}

	command = append(command, request.Prompt)

	return c.cluster.ExecPod(ctx, request.Target.Namespace, pod, "agent", command, false)
}

// OpenShell opens an interactive shell in a ready coding session.
func (c *Control) OpenShell(ctx context.Context, target Target) error {
	if err := target.validate(); err != nil {
		return err
	}

	pod, err := c.cluster.WaitForReadyPod(ctx, target.Namespace, target.Name, sessionReadyTimeout)
	if err != nil {
		return err
	}

	return c.cluster.ExecPod(ctx, target.Namespace, pod, "agent", []string{"bash"}, true)
}

// Stop stops a long session while retaining its persistent state.
func (c *Control) Stop(ctx context.Context, target Target) error {
	if err := target.validate(); err != nil {
		return err
	}

	return c.cluster.StopSession(ctx, target.Namespace, target.Name)
}

// Resume starts a stopped long session against its retained state.
func (c *Control) Resume(ctx context.Context, target Target) error {
	if err := target.validate(); err != nil {
		return err
	}

	return c.cluster.ResumeSession(ctx, target.Namespace, target.Name)
}

// Delete removes a session's compute resources and retains its persistent state.
func (c *Control) Delete(ctx context.Context, target Target) error {
	if err := target.validate(); err != nil {
		return err
	}

	return c.cluster.DeleteSession(ctx, target.Namespace, target.Name)
}

// Destroy removes a session's compute resources and persistent state.
func (c *Control) Destroy(ctx context.Context, target Target) error {
	if err := target.validate(); err != nil {
		return err
	}

	return c.cluster.DestroySession(ctx, target.Namespace, target.Name)
}

// Publish commits a retained session workspace and opens or recovers its pull request.
func (c *Control) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	if err := request.Target.validate(); err != nil {
		return PublishResult{}, err
	}

	result, err := c.publishSession(ctx, request.publishRequest())
	if err != nil {
		return PublishResult{}, err
	}

	return PublishResult{
		PullRequestNumber: result.PullRequestNumber,
		PullRequestURL:    result.PullRequestURL,
		Branch:            result.Branch,
		CommitSHA:         result.CommitSHA,
	}, nil
}

// Validate checks whether a bootstrap request can describe a shared session environment.
func (request BootstrapRequest) Validate() error {
	_, err := request.manifest()

	return err
}

// Validate checks whether a start request can describe a coding session.
func (request StartRequest) Validate() error {
	_, _, err := request.manifest()

	return err
}

// Validate checks whether a publish request contains a complete publishing intent.
func (request PublishRequest) Validate() error {
	return request.publishRequest().Validate()
}

func (request BootstrapRequest) manifest() ([]byte, error) {
	return sessionmanifest.RenderBootstrap(
		request.Namespace,
		request.CodexAuthJSON,
		request.CodexAPIKey,
		request.GitHubToken,
	)
}

func (request StartRequest) manifest() ([]byte, sessionmanifest.Mode, error) {
	mode := sessionmanifest.Mode(request.Mode)
	manifest, err := sessionmanifest.Render(sessionmanifest.Spec{
		Name:           request.Target.Name,
		Namespace:      request.Target.Namespace,
		Image:          request.Image,
		Mode:           mode,
		Repository:     request.Repository,
		InitialRef:     request.InitialRef,
		SetupCommand:   request.SetupCommand,
		Prompt:         request.Prompt,
		CloneDepth:     request.CloneDepth,
		StorageSize:    request.StorageSize,
		TimeoutSeconds: int64(request.Timeout.Seconds()),
	})

	return manifest, mode, err
}

func (request PublishRequest) publishRequest() publish.Request {
	return publish.Request{
		Namespace:     request.Target.Namespace,
		Session:       request.Target.Name,
		Branch:        request.Branch,
		BaseBranch:    request.BaseBranch,
		CommitMessage: request.CommitMessage,
		Title:         request.Title,
		Body:          request.Body,
		Draft:         request.Draft,
		Timeout:       request.Timeout,
	}
}

func (target Target) validate() error {
	if target.Namespace == "" {
		return errors.New("session namespace is required")
	}

	if target.Name == "" {
		return errors.New("session name is required")
	}

	return nil
}
