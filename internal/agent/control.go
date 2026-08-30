package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/yarlson/dyne/internal/kubernetes"
	"github.com/yarlson/dyne/internal/publish"
)

type sessionTarget struct {
	namespace string
	name      string
}

type sessionStartRequest struct {
	target       sessionTarget
	image        string
	storage      Storage
	repository   string
	initialRef   string
	setupCommand string
	prompt       string
	agentName    string
	instructions string
	skills       []AgentSkill
	cloneDepth   int
	storageSize  string
	timeout      time.Duration
	resultKind   ResultKind
	workflowRun  string
	workflowStep string
}

type sessionContinueRequest struct {
	target  sessionTarget
	taskID  string
	prompt  string
	timeout time.Duration
}

type sessionLogRequest struct {
	target sessionTarget
	follow bool
}

type sessionPublishRequest struct {
	target        sessionTarget
	branch        string
	baseBranch    string
	commitMessage string
	draft         bool
	timeout       time.Duration
}

type sessionCluster interface {
	CreateSession(context.Context, kubernetes.SessionRequest) error
	ContinueSession(context.Context, kubernetes.ContinuationRequest) error
	SetGitHubToken(context.Context, string, string) error
	CheckSessionAvailable(context.Context, string, string) error
	CheckWorkflowSessionOwnership(context.Context, string, string, string, string) error
	SessionStatus(context.Context, string, string) ([]kubernetes.ResourceStatus, error)
	SessionArtifacts(context.Context, string, string) (kubernetes.TaskArtifacts, error)
	WriteSessionLogs(context.Context, string, string, bool, io.Writer) error
	DeleteSession(context.Context, string, string) error
	DestroySession(context.Context, string, string) error
}

type sessionOperations interface {
	start(context.Context, sessionStartRequest) error
	continueTask(context.Context, sessionContinueRequest) error
	status(context.Context, sessionTarget) (Status, error)
	artifacts(context.Context, sessionTarget) (Artifacts, error)
	writeLogs(context.Context, sessionLogRequest, io.Writer) error
	delete(context.Context, sessionTarget) error
	destroy(context.Context, sessionTarget) error
	publish(context.Context, sessionPublishRequest) (PublishResult, error)
}

type sessionPublisher func(context.Context, publish.Request) (publish.Result, error)

type sessionControl struct {
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

func connectSessions(ctx context.Context, connection Connection, output io.Writer, repositoryAuth RepositoryTokenProvider) (*sessionControl, error) {
	if output == nil {
		return nil, errors.New("output stream is required")
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

	cluster, err := kubernetes.NewForConfig(config, output)
	if err != nil {
		return nil, err
	}

	return &sessionControl{
		cluster:        cluster,
		repositoryAuth: repositoryAuth,
		publishSession: func(ctx context.Context, request publish.Request) (publish.Result, error) {
			return publish.Session(ctx, cluster, request)
		},
	}, nil
}

func (c *sessionControl) start(ctx context.Context, request sessionStartRequest) error {
	unlock := c.lockSession(request.target)
	defer unlock()

	if err := request.validate(); err != nil {
		return err
	}

	if err := c.cluster.CheckSessionAvailable(ctx, request.target.namespace, request.target.name); err != nil {
		if request.workflowRun == "" {
			return err
		}

		if ownershipErr := c.cluster.CheckWorkflowSessionOwnership(
			ctx, request.target.namespace, request.target.name, request.workflowRun, request.workflowStep,
		); ownershipErr != nil {
			return errors.Join(err, ownershipErr)
		}
	}

	if err := c.refreshRepositoryCredential(ctx, request.target.namespace); err != nil {
		return err
	}

	return c.cluster.CreateSession(ctx, request.kubernetesRequest())
}

func (c *sessionControl) continueTask(ctx context.Context, request sessionContinueRequest) error {
	unlock := c.lockSession(request.target)
	defer unlock()

	if err := request.target.validate(); err != nil {
		return err
	}

	if request.taskID == "" {
		return errors.New("task ID is required")
	}

	return c.cluster.ContinueSession(ctx, kubernetes.ContinuationRequest{
		Name:           request.target.name,
		TaskName:       request.target.name + "-" + request.taskID,
		Namespace:      request.target.namespace,
		Prompt:         request.prompt,
		TimeoutSeconds: int64(request.timeout.Seconds()),
	})
}

func (c *sessionControl) status(ctx context.Context, target sessionTarget) (Status, error) {
	if err := target.validate(); err != nil {
		return Status{}, err
	}

	resources, err := c.cluster.SessionStatus(ctx, target.namespace, target.name)
	if err != nil {
		return Status{}, err
	}

	status := Status{Resources: make([]ResourceStatus, len(resources))}
	for i, resource := range resources {
		status.Resources[i] = ResourceStatus{
			Kind: resource.Kind, Name: resource.Name, Ready: resource.Ready, State: resource.State,
		}
	}

	return status, nil
}

func (c *sessionControl) artifacts(ctx context.Context, target sessionTarget) (Artifacts, error) {
	if err := target.validate(); err != nil {
		return Artifacts{}, err
	}

	result, err := c.cluster.SessionArtifacts(ctx, target.namespace, target.name)
	if err != nil {
		return Artifacts{}, err
	}

	return Artifacts{
		Outcome: result.Outcome, PullRequest: result.PullRequest, WorkflowOutput: result.WorkflowOutput,
	}, nil
}

func (c *sessionControl) writeLogs(ctx context.Context, request sessionLogRequest, output io.Writer) error {
	if err := request.target.validate(); err != nil {
		return err
	}

	return c.cluster.WriteSessionLogs(ctx, request.target.namespace, request.target.name, request.follow, output)
}

func (c *sessionControl) delete(ctx context.Context, target sessionTarget) error {
	unlock := c.lockSession(target)
	defer unlock()

	if err := target.validate(); err != nil {
		return err
	}

	return c.cluster.DeleteSession(ctx, target.namespace, target.name)
}

func (c *sessionControl) destroy(ctx context.Context, target sessionTarget) error {
	unlock := c.lockSession(target)
	defer unlock()

	if err := target.validate(); err != nil {
		return err
	}

	return c.cluster.DestroySession(ctx, target.namespace, target.name)
}

func (c *sessionControl) publish(ctx context.Context, request sessionPublishRequest) (PublishResult, error) {
	unlock := c.lockSession(request.target)
	defer unlock()

	if err := request.target.validate(); err != nil {
		return PublishResult{}, err
	}

	if err := c.refreshRepositoryCredential(ctx, request.target.namespace); err != nil {
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

func (c *sessionControl) refreshRepositoryCredential(ctx context.Context, namespace string) error {
	if c.repositoryAuth == nil {
		return nil
	}

	token, err := c.repositoryAuth.InstallationToken(ctx)
	if err != nil {
		return err
	}

	return c.cluster.SetGitHubToken(ctx, namespace, token)
}

func (c *sessionControl) lockSession(target sessionTarget) func() {
	key := target.namespace + "/" + target.name
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

func (request sessionStartRequest) validate() error {
	return request.kubernetesRequest().Validate()
}

func (request sessionStartRequest) kubernetesRequest() kubernetes.SessionRequest {
	return kubernetes.SessionRequest{
		Name:           request.target.name,
		Namespace:      request.target.namespace,
		Image:          request.image,
		Storage:        kubernetes.SessionStorage(request.storage),
		Repository:     request.repository,
		InitialRef:     request.initialRef,
		SetupCommand:   request.setupCommand,
		Prompt:         request.prompt,
		AgentName:      request.agentName,
		Instructions:   request.instructions,
		Skills:         kubernetesSkills(request.skills),
		CloneDepth:     request.cloneDepth,
		StorageSize:    request.storageSize,
		TimeoutSeconds: int64(request.timeout.Seconds()),
		ResultKind:     kubernetes.ResultKind(request.resultKind),
		WorkflowRun:    request.workflowRun,
		WorkflowStep:   request.workflowStep,
	}
}

func kubernetesSkills(skills []AgentSkill) []kubernetes.SessionSkill {
	if len(skills) == 0 {
		return nil
	}

	result := make([]kubernetes.SessionSkill, len(skills))
	for i, skill := range skills {
		result[i] = kubernetes.SessionSkill{Name: skill.Name, Contents: skill.Contents}
	}

	return result
}

func (request sessionPublishRequest) validate() error {
	return request.publishRequest().Validate()
}

func (request sessionPublishRequest) publishRequest() publish.Request {
	return publish.Request{
		Namespace:     request.target.namespace,
		Session:       request.target.name,
		Branch:        request.branch,
		BaseBranch:    request.baseBranch,
		CommitMessage: request.commitMessage,
		Draft:         request.draft,
		Timeout:       request.timeout,
	}
}

func (target sessionTarget) validate() error {
	if target.namespace == "" {
		return errors.New("session namespace is required")
	}

	if target.name == "" {
		return errors.New("session name is required")
	}

	return nil
}
