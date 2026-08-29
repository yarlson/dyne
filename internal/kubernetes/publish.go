package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/yarlson/airlock/internal/sessionmanifest"
)

const publishIntentAnnotationKey = "coding-agent/publish-intent"

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PublishSource describes an eligible session workspace that can be published.
type PublishSource struct {
	// Repository is the Git repository configured for the session.
	Repository string
	// InitialRef is the Git ref from which the session started.
	InitialRef string
	// Image is the agent image used to run the publisher.
	Image string
	// WorkspaceClaim is the name of the bound claim containing the source workspace.
	WorkspaceClaim string
}

// PublisherJobRequest defines one bounded publisher Job.
type PublisherJobRequest struct {
	// Namespace owns the session and publisher resources.
	Namespace string
	// Session identifies the source session.
	Session string
	// IntentID identifies an idempotent publish attempt.
	IntentID string
	// Repository is the remote Git repository URL.
	Repository string
	// BaseRef is the trusted remote ref cloned before copying workspace changes.
	BaseRef string
	// Branch is the new remote branch created by the publisher.
	Branch string
	// CommitMessage is the message used for the workspace commit.
	CommitMessage string
	// AuthorName is the Git commit author's name.
	AuthorName string
	// AuthorEmail is the Git commit author's email address.
	AuthorEmail string
	// Image contains the publisher entrypoint.
	Image string
	// WorkspaceClaim is the name of the claim containing the source workspace.
	WorkspaceClaim string
	// Timeout bounds the publisher Job and the wait for its result.
	Timeout time.Duration
}

// PublisherJobResult identifies the branch and commit pushed by a publisher Job.
type PublisherJobResult struct {
	// Branch is the remote branch created by the publisher.
	Branch string
	// CommitSHA is the commit verified on the remote branch.
	CommitSHA string
	// Title is the validated pull request title written by the coding session.
	Title string
	// Body is the validated pull request description written by the coding session.
	Body string
}

// SessionPublishSource returns publishing inputs for a completed persistent session.
func (c *Client) SessionPublishSource(ctx context.Context, namespace, session string) (PublishSource, error) {
	jobs, err := c.typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: sessionTaskSelector(session)})
	if err != nil {
		return PublishSource{}, fmt.Errorf("list session Jobs: %w", err)
	}

	if len(jobs.Items) == 0 {
		return PublishSource{}, fmt.Errorf("session %s does not exist", session)
	}

	var latest *batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Status.Active != 0 {
			return PublishSource{}, errors.New("session has an active task")
		}

		if latest == nil || job.CreationTimestamp.After(latest.CreationTimestamp.Time) ||
			(job.CreationTimestamp.Equal(&latest.CreationTimestamp) && job.Name > latest.Name) {
			latest = job
		}
	}

	if latest.Status.Succeeded == 0 {
		return PublishSource{}, errors.New("latest session task must complete successfully before publishing")
	}

	artifacts, err := c.SessionArtifacts(ctx, namespace, session)
	if err != nil {
		return PublishSource{}, err
	}

	var outcome struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(artifacts.Outcome, &outcome); err != nil {
		return PublishSource{}, fmt.Errorf("decode session outcome: %w", err)
	}

	if outcome.Status != "completed" {
		return PublishSource{}, fmt.Errorf("session outcome is %q, want completed", outcome.Status)
	}

	agentContainer, err := namedContainer(latest.Spec.Template.Spec.Containers, "agent")
	if err != nil {
		return PublishSource{}, err
	}

	environment := literalContainerEnvironment(agentContainer)
	if environment["AGENT_STORAGE"] != "persistent" {
		return PublishSource{}, errors.New("ephemeral sessions cannot be published")
	}

	if environment["AGENT_REPOSITORY"] == "" || environment["AGENT_REF"] == "" {
		return PublishSource{}, errors.New("session repository and base ref are required for publishing")
	}

	workspaceClaim := sessionmanifest.SessionClaimName(session)
	claim, err := c.typed.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, workspaceClaim, metav1.GetOptions{})
	if err != nil {
		return PublishSource{}, fmt.Errorf("get session PersistentVolumeClaim: %w", err)
	}

	if claim.Status.Phase != corev1.ClaimBound {
		return PublishSource{}, fmt.Errorf("session PersistentVolumeClaim is %s, want Bound", claim.Status.Phase)
	}

	return PublishSource{
		Repository:     environment["AGENT_REPOSITORY"],
		InitialRef:     environment["AGENT_REF"],
		Image:          agentContainer.Image,
		WorkspaceClaim: workspaceClaim,
	}, nil
}

// GitHubToken returns the token stored in the namespace's GitHub Secret.
func (c *Client) GitHubToken(ctx context.Context, namespace string) (string, error) {
	secret, err := c.typed.CoreV1().Secrets(namespace).Get(ctx, sessionmanifest.GitHubTokenSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get GitHub Secret: %w", err)
	}

	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", errors.New("GitHub Secret does not contain a token")
	}

	return token, nil
}

// PublisherJobIntent returns the recorded intent ID and whether the publisher Job exists.
func (c *Client) PublisherJobIntent(ctx context.Context, namespace, session string) (string, bool, error) {
	job, err := c.typed.BatchV1().Jobs(namespace).Get(ctx, publisherJobName(session), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("get publisher Job: %w", err)
	}

	return job.Annotations[publishIntentAnnotationKey], true, nil
}

// RunPublisherJob creates or resumes the matching publisher Job and waits for its result.
func (c *Client) RunPublisherJob(ctx context.Context, request PublisherJobRequest) (PublisherJobResult, error) {
	if request.Timeout < time.Second {
		return PublisherJobResult{}, errors.New("publisher timeout must be at least one second")
	}

	job, err := c.ensurePublisherJob(ctx, request)
	if err != nil {
		return PublisherJobResult{}, err
	}

	if jobFailed(job) {
		if err := c.deletePublisherJob(ctx, request.Namespace, request.Session); err != nil {
			return PublisherJobResult{}, err
		}

		if _, err := c.createPublisherJob(ctx, request); err != nil {
			return PublisherJobResult{}, err
		}
	}

	return c.waitForPublisherJob(ctx, request.Namespace, request.Session, request.IntentID, request.Timeout)
}

// DeletePublisherJob removes a session's publisher Job and waits for its Pods to disappear.
func (c *Client) DeletePublisherJob(ctx context.Context, namespace, session string) error {
	return c.deletePublisherJob(ctx, namespace, session)
}

// WaitForPublisherJob waits for an existing publisher Job with the expected intent ID and returns its result.
func (c *Client) WaitForPublisherJob(ctx context.Context, namespace, session, intentID string, timeout time.Duration) (PublisherJobResult, error) {
	return c.waitForPublisherJob(ctx, namespace, session, intentID, timeout)
}

func (c *Client) ensurePublisherJob(ctx context.Context, request PublisherJobRequest) (*batchv1.Job, error) {
	job, err := c.typed.BatchV1().Jobs(request.Namespace).Get(ctx, publisherJobName(request.Session), metav1.GetOptions{})
	if err == nil {
		if job.Annotations[publishIntentAnnotationKey] != request.IntentID {
			return nil, errors.New("publisher Job belongs to a different publish request")
		}

		return job, nil
	}

	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get publisher Job: %w", err)
	}

	return c.createPublisherJob(ctx, request)
}

func (c *Client) createPublisherJob(ctx context.Context, request PublisherJobRequest) (*batchv1.Job, error) {
	job := publisherJob(request)
	created, err := c.typed.BatchV1().Jobs(request.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return c.ensurePublisherJob(ctx, request)
	}

	if err != nil {
		return nil, fmt.Errorf("create publisher Job: %w", err)
	}

	_, _ = fmt.Fprintf(c.stdout, "job/%s created\n", job.Name)

	return created, nil
}

func (c *Client) waitForPublisherJob(ctx context.Context, namespace, session, intentID string, timeout time.Duration) (PublisherJobResult, error) {
	var result PublisherJobResult
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := c.typed.BatchV1().Jobs(namespace).Get(ctx, publisherJobName(session), metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if job.Annotations[publishIntentAnnotationKey] != intentID {
			return false, errors.New("publisher Job intent changed while waiting")
		}

		if jobFailed(job) {
			return false, c.publisherJobFailure(ctx, namespace, job.Name)
		}

		if !jobComplete(job) {
			return false, nil
		}

		result, err = c.publisherJobResult(ctx, namespace, job.Name)

		return err == nil, err
	})
	if err != nil {
		return PublisherJobResult{}, fmt.Errorf("publish session %s: %w", session, err)
	}

	return result, nil
}

func (c *Client) publisherJobResult(ctx context.Context, namespace, jobName string) (PublisherJobResult, error) {
	pod, err := c.publisherJobPod(ctx, namespace, jobName)
	if err != nil {
		return PublisherJobResult{}, err
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "publisher" || status.State.Terminated == nil {
			continue
		}

		result := parsePublisherJobResult(status.State.Terminated.Message)
		if result.Branch == "" || !commitSHAPattern.MatchString(result.CommitSHA) || result.Title == "" || result.Body == "" {
			return PublisherJobResult{}, errors.New("publisher did not report a valid branch, commit, and pull request artifact")
		}

		return result, nil
	}

	return PublisherJobResult{}, errors.New("publisher container has no termination result")
}

func (c *Client) publisherJobFailure(ctx context.Context, namespace, jobName string) error {
	pod, err := c.publisherJobPod(ctx, namespace, jobName)
	if err != nil {
		return err
	}

	contents, logErr := c.typed.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "publisher"}).Do(ctx).Raw()
	if logErr != nil {
		return fmt.Errorf("publisher Job failed; read logs: %w", logErr)
	}

	message := strings.TrimSpace(string(contents))
	if len(message) > 4000 {
		message = message[len(message)-4000:]
	}

	if message == "" {
		message = "publisher container exited without an error message"
	}

	return errors.New(message)
}

func (c *Client) publisherJobPod(ctx context.Context, namespace, jobName string) (*corev1.Pod, error) {
	pods, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{"job-name": jobName}.AsSelector().String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list publisher Pods: %w", err)
	}

	if len(pods.Items) != 1 {
		return nil, fmt.Errorf("publisher Job has %d Pods, want 1", len(pods.Items))
	}

	return &pods.Items[0], nil
}

func (c *Client) deletePublisherJob(ctx context.Context, namespace, session string) error {
	err := c.typed.BatchV1().Jobs(namespace).Delete(ctx, publisherJobName(session), metav1.DeleteOptions{
		PropagationPolicy: new(metav1.DeletePropagationBackground),
	})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("delete publisher Job: %w", err)
	}

	return wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		jobName := publisherJobName(session)
		_, jobErr := c.typed.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
		if jobErr != nil && !apierrors.IsNotFound(jobErr) {
			return false, jobErr
		}

		pods, podErr := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.Set{"job-name": jobName}.AsSelector().String(),
		})
		if podErr != nil {
			return false, podErr
		}

		return apierrors.IsNotFound(jobErr) && len(pods.Items) == 0, nil
	})
}

func publisherJob(request PublisherJobRequest) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name":       "coding-agent",
		"app.kubernetes.io/managed-by": "airlock",
		"coding-agent/session":         request.Session,
		"coding-agent/component":       "publisher",
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        publisherJobName(request.Session),
			Namespace:   request.Namespace,
			Labels:      labels,
			Annotations: map[string]string{publishIntentAnnotationKey: request.IntentID},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          new(int32(0)),
			ActiveDeadlineSeconds: new(int64(request.Timeout.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    new(int64(1000)),
						RunAsGroup:   new(int64(1000)),
						FSGroup:      new(int64(1000)),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{publisherContainer(request)},
					Volumes: []corev1.Volume{
						{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: request.WorkspaceClaim,
								ReadOnly:  true,
							}},
						},
						{
							Name: "artifacts",
							VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: request.WorkspaceClaim,
								ReadOnly:  true,
							}},
						},
						{Name: "publish", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resourceQuantity("4Gi"))}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: new(resourceQuantity("1Gi"))}}},
						{
							Name: "git-auth",
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
								SecretName:  sessionmanifest.GitHubTokenSecretName,
								DefaultMode: new(int32(0o440)),
							}},
						},
					},
				},
			},
		},
	}
}

func publisherContainer(request PublisherJobRequest) corev1.Container {
	return corev1.Container{
		Name:            "publisher",
		Image:           request.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"publish"},
		WorkingDir:      "/workspace",
		Env: []corev1.EnvVar{
			{Name: "PUBLISH_REPOSITORY", Value: request.Repository},
			{Name: "PUBLISH_BASE", Value: request.BaseRef},
			{Name: "PUBLISH_BRANCH", Value: request.Branch},
			{Name: "PUBLISH_COMMIT_MESSAGE", Value: request.CommitMessage},
			{Name: "PUBLISH_AUTHOR_NAME", Value: request.AuthorName},
			{Name: "PUBLISH_AUTHOR_EMAIL", Value: request.AuthorEmail},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resourceQuantity("250m"),
				corev1.ResourceMemory:           resourceQuantity("512Mi"),
				corev1.ResourceEphemeralStorage: resourceQuantity("512Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resourceQuantity("2"),
				corev1.ResourceMemory:           resourceQuantity("4Gi"),
				corev1.ResourceEphemeralStorage: resourceQuantity("6Gi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace", SubPath: "workspace", ReadOnly: true},
			{Name: "artifacts", MountPath: "/artifacts", SubPath: "artifacts", ReadOnly: true},
			{Name: "publish", MountPath: "/publish"},
			{Name: "tmp", MountPath: "/tmp"},
			{Name: "git-auth", MountPath: "/var/run/git-auth", ReadOnly: true},
		},
	}
}

func namedContainer(containers []corev1.Container, name string) (corev1.Container, error) {
	for _, container := range containers {
		if container.Name == name {
			return container, nil
		}
	}

	return corev1.Container{}, fmt.Errorf("session Pod has no %s container", name)
}

func literalContainerEnvironment(container corev1.Container) map[string]string {
	environment := make(map[string]string, len(container.Env))
	for _, variable := range container.Env {
		environment[variable.Name] = variable.Value
	}

	return environment
}

func publisherJobName(session string) string {
	return session + "-publish"
}

func jobComplete(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func parsePublisherJobResult(message string) PublisherJobResult {
	var result PublisherJobResult
	for line := range strings.SplitSeq(message, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "branch":
			result.Branch = value
		case "commit":
			result.CommitSHA = value
		case "title":
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err == nil {
				result.Title = string(decoded)
			}
		case "body":
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err == nil {
				result.Body = string(decoded)
			}
		}
	}

	return result
}

func resourceQuantity(value string) resource.Quantity {
	return resource.MustParse(value)
}
