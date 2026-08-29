package agentsandbox

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/yarlson/airlock/internal/kubernetes"
	"github.com/yarlson/airlock/internal/publish"
	"github.com/yarlson/airlock/internal/sessionmanifest"
)

type sessionCluster interface {
	Apply(context.Context, []byte) error
	SetGitHubToken(context.Context, string, string) error
	CheckSessionAvailable(context.Context, string, string) error
	PersistentSessionDefinition(context.Context, string, string) (kubernetes.SessionDefinition, error)
	SessionStatus(context.Context, string, string) ([]kubernetes.ResourceStatus, error)
	SessionArtifacts(context.Context, string, string) (kubernetes.TaskArtifacts, error)
	StreamPodLogs(context.Context, string, string, string, bool, io.Writer) error
	DeleteSession(context.Context, string, string) error
	DestroySession(context.Context, string, string) error
	NewestPodName(context.Context, string, string) (string, error)
}

// Continue creates one resumable Job against a persistent session's retained state.
func (c *Control) Continue(ctx context.Context, request ContinueRequest) error {
	unlock := c.lockSession(request.Target)
	defer unlock()

	if err := request.Target.validate(); err != nil {
		return err
	}

	if request.TaskID == "" {
		return errors.New("task ID is required")
	}

	definition, err := c.cluster.PersistentSessionDefinition(ctx, request.Target.Namespace, request.Target.Name)
	if err != nil {
		return err
	}

	manifest, err := sessionmanifest.RenderContinuation(sessionmanifest.Spec{
		Name:           request.Target.Name,
		TaskName:       request.Target.Name + "-" + request.TaskID,
		Namespace:      request.Target.Namespace,
		Image:          definition.Image,
		Storage:        sessionmanifest.StoragePersistent,
		Repository:     definition.Repository,
		InitialRef:     definition.InitialRef,
		SetupCommand:   definition.SetupCommand,
		Prompt:         request.Prompt,
		AgentName:      definition.AgentName,
		Skills:         definition.Skills,
		CloneDepth:     definition.CloneDepth,
		TimeoutSeconds: int64(request.Timeout.Seconds()),
		Resume:         true,
	})
	if err != nil {
		return err
	}

	return c.cluster.Apply(ctx, manifest)
}

type sessionPublisher func(context.Context, publish.Request) (publish.Result, error)

// Control performs complete coding-session operations.
type Control struct {
	cluster        sessionCluster
	publishSession sessionPublisher
	repositoryAuth RepositoryTokenProvider
	locksMu        sync.Mutex
	locks          map[string]*sessionLock
}

type sessionLock struct {
	mutex sync.Mutex
	users int
}

// New returns coding-session control for one Kubernetes context.
func New(contextName string, streams Streams) (*Control, error) {
	if streams.Input == nil || streams.Output == nil || streams.ErrorOutput == nil {
		return nil, errors.New("input, output, and error output streams are required")
	}

	cluster, err := kubernetes.New(contextName, streams.Output)
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

// Connect returns control-plane operations using server-owned cluster and repository credentials.
func Connect(ctx context.Context, connection Connection, streams Streams, repositoryAuth RepositoryTokenProvider) (*Control, error) {
	if streams.Input == nil || streams.Output == nil || streams.ErrorOutput == nil {
		return nil, errors.New("input, output, and error output streams are required")
	}

	config, err := kubernetes.LoadConnectionConfig(ctx, kubernetes.ConnectionConfig{
		KubeconfigPath: connection.KubeconfigPath,
		ContextName:    connection.ContextName,
		EKSCluster:     connection.EKSCluster,
		AWSRegion:      connection.AWSRegion,
		AWSRoleARN:     connection.AWSRoleARN,
	})
	if err != nil {
		return nil, err
	}

	cluster, err := kubernetes.NewForConfig(config, streams.Output)
	if err != nil {
		return nil, err
	}

	return &Control{
		cluster:        cluster,
		repositoryAuth: repositoryAuth,
		publishSession: func(ctx context.Context, request publish.Request) (publish.Result, error) {
			return publish.Session(ctx, cluster, request)
		},
	}, nil
}

// Start creates a coding session with the requested lifecycle and storage behavior.
func (c *Control) Start(ctx context.Context, request StartRequest) error {
	unlock := c.lockSession(request.Target)
	defer unlock()

	manifest, err := request.manifest()
	if err != nil {
		return err
	}

	if err := c.cluster.CheckSessionAvailable(ctx, request.Target.Namespace, request.Target.Name); err != nil {
		return err
	}

	if err := c.refreshRepositoryCredential(ctx, request.Target.Namespace); err != nil {
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

// Artifacts returns the latest task's validated outcome and pull request metadata.
func (c *Control) Artifacts(ctx context.Context, target Target) (Artifacts, error) {
	if err := target.validate(); err != nil {
		return Artifacts{}, err
	}

	result, err := c.cluster.SessionArtifacts(ctx, target.Namespace, target.Name)
	if err != nil {
		return Artifacts{}, err
	}

	return Artifacts{Outcome: result.Outcome, PullRequest: result.PullRequest}, nil
}

// WriteLogs writes logs from a coding session's current execution attempt.
func (c *Control) WriteLogs(ctx context.Context, request LogRequest, output io.Writer) error {
	if err := request.Target.validate(); err != nil {
		return err
	}

	pod, err := c.cluster.NewestPodName(ctx, request.Target.Namespace, request.Target.Name)
	if err != nil {
		return err
	}

	return c.cluster.StreamPodLogs(ctx, request.Target.Namespace, pod, "agent", request.Follow, output)
}

// Delete removes a session's compute resources and retains its persistent state.
func (c *Control) Delete(ctx context.Context, target Target) error {
	unlock := c.lockSession(target)
	defer unlock()

	if err := target.validate(); err != nil {
		return err
	}

	return c.cluster.DeleteSession(ctx, target.Namespace, target.Name)
}

// Destroy removes a session's compute resources and persistent state.
func (c *Control) Destroy(ctx context.Context, target Target) error {
	unlock := c.lockSession(target)
	defer unlock()

	if err := target.validate(); err != nil {
		return err
	}

	return c.cluster.DestroySession(ctx, target.Namespace, target.Name)
}

// Publish commits a retained session workspace and opens or recovers its pull request.
func (c *Control) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	unlock := c.lockSession(request.Target)
	defer unlock()

	if err := request.Target.validate(); err != nil {
		return PublishResult{}, err
	}

	if err := c.refreshRepositoryCredential(ctx, request.Target.Namespace); err != nil {
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

func (c *Control) refreshRepositoryCredential(ctx context.Context, namespace string) error {
	if c.repositoryAuth == nil {
		return nil
	}

	token, err := c.repositoryAuth.InstallationToken(ctx)
	if err != nil {
		return err
	}

	return c.cluster.SetGitHubToken(ctx, namespace, token)
}

func (c *Control) lockSession(target Target) func() {
	key := target.Namespace + "/" + target.Name
	c.locksMu.Lock()
	if c.locks == nil {
		c.locks = make(map[string]*sessionLock)
	}

	lock := c.locks[key]
	if lock == nil {
		lock = &sessionLock{}
		c.locks[key] = lock
	}

	lock.users++
	c.locksMu.Unlock()
	lock.mutex.Lock()

	return func() {
		lock.mutex.Unlock()
		c.locksMu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(c.locks, key)
		}

		c.locksMu.Unlock()
	}
}

// Validate checks whether a start request can describe a coding session.
func (request StartRequest) Validate() error {
	_, err := request.manifest()

	return err
}

// Validate checks whether a publish request contains a complete publishing intent.
func (request PublishRequest) Validate() error {
	return request.publishRequest().Validate()
}

func (request StartRequest) manifest() ([]byte, error) {
	return sessionmanifest.Render(sessionmanifest.Spec{
		Name:           request.Target.Name,
		Namespace:      request.Target.Namespace,
		Image:          request.Image,
		Storage:        sessionmanifest.Storage(request.Storage),
		Repository:     request.Repository,
		InitialRef:     request.InitialRef,
		SetupCommand:   request.SetupCommand,
		Prompt:         request.Prompt,
		AgentName:      request.AgentName,
		Instructions:   request.Instructions,
		Skills:         manifestSkills(request.Skills),
		CloneDepth:     request.CloneDepth,
		StorageSize:    request.StorageSize,
		TimeoutSeconds: int64(request.Timeout.Seconds()),
	})
}

func manifestSkills(skills []AgentSkill) []sessionmanifest.AgentSkill {
	if len(skills) == 0 {
		return nil
	}

	result := make([]sessionmanifest.AgentSkill, len(skills))
	for i, skill := range skills {
		result[i] = sessionmanifest.AgentSkill{Name: skill.Name, Contents: skill.Contents}
	}

	return result
}

func (request PublishRequest) publishRequest() publish.Request {
	return publish.Request{
		Namespace:     request.Target.Namespace,
		Session:       request.Target.Name,
		Branch:        request.Branch,
		BaseBranch:    request.BaseBranch,
		CommitMessage: request.CommitMessage,
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
