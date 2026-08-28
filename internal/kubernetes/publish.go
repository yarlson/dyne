package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"coding-agent-k8s/internal/agent"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
)

const publishIntentAnnotation = "coding-agent/publish-intent"

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PublishSource describes an eligible session workspace that can be published.
type PublishSource struct {
	// Repository is the Git repository configured for the session.
	Repository string
	// Base is the Git ref from which the session started.
	Base string
	// Image is the agent image used to run the publisher.
	Image string
	// WorkspaceClaim is the bound claim containing the source workspace.
	WorkspaceClaim string
}

// PublishRequest defines one bounded publisher Job.
type PublishRequest struct {
	// Namespace owns the session and publisher resources.
	Namespace string
	// Session identifies the source session.
	Session string
	// Intent identifies an idempotent publish attempt.
	Intent string
	// Repository is the remote Git repository URL.
	Repository string
	// Base is the trusted remote ref cloned before copying workspace changes.
	Base string
	// Branch is the new remote branch created by the publisher.
	Branch string
	// CommitMessage is the message used for the workspace commit.
	CommitMessage string
	// AuthorName is the Git commit author's display name.
	AuthorName string
	// AuthorEmail is the Git commit author's email address.
	AuthorEmail string
	// Image contains the publisher entrypoint.
	Image string
	// Workspace is the claim containing the source workspace.
	Workspace string
	// Timeout bounds the publisher Job and the wait for its result.
	Timeout time.Duration
}

// PublishResult identifies the branch and commit pushed by a publisher Job.
type PublishResult struct {
	// Branch is the remote branch created by the publisher.
	Branch string
	// Commit is the commit SHA verified on the remote branch.
	Commit string
}

// PublishSource finds and validates a completed update session or stopped long session.
func (c *Client) PublishSource(ctx context.Context, namespace, session string) (PublishSource, error) {
	job, jobErr := c.typed.BatchV1().Jobs(namespace).Get(ctx, session, metav1.GetOptions{})
	set, setErr := c.typed.AppsV1().StatefulSets(namespace).Get(ctx, session, metav1.GetOptions{})
	jobFound := jobErr == nil
	setFound := setErr == nil
	if jobErr != nil && !apierrors.IsNotFound(jobErr) {
		return PublishSource{}, fmt.Errorf("get session Job: %w", jobErr)
	}
	if setErr != nil && !apierrors.IsNotFound(setErr) {
		return PublishSource{}, fmt.Errorf("get session StatefulSet: %w", setErr)
	}
	if jobFound == setFound {
		if jobFound {
			return PublishSource{}, errors.New("session has both a Job and StatefulSet")
		}
		return PublishSource{}, fmt.Errorf("session %s does not exist", session)
	}
	var podSpec corev1.PodSpec
	if jobFound {
		if job.Status.Succeeded == 0 || job.Status.Active != 0 {
			return PublishSource{}, errors.New("update session must complete successfully before publishing")
		}
		podSpec = job.Spec.Template.Spec
	} else {
		desired := int32(1)
		if set.Spec.Replicas != nil {
			desired = *set.Spec.Replicas
		}
		if desired != 0 || set.Status.Replicas != 0 {
			return PublishSource{}, errors.New("long session must be stopped before publishing")
		}
		podSpec = set.Spec.Template.Spec
	}
	agentContainer, err := namedContainer(podSpec.Containers, "agent")
	if err != nil {
		return PublishSource{}, err
	}
	environment := containerEnvironment(agentContainer)
	mode := environment["AGENT_MODE"]
	if jobFound && mode != "update" {
		return PublishSource{}, fmt.Errorf("%s sessions cannot be published", mode)
	}
	if !jobFound && mode != "long" {
		return PublishSource{}, fmt.Errorf("unexpected StatefulSet session mode %q", mode)
	}
	if environment["AGENT_REPOSITORY"] == "" || environment["AGENT_REF"] == "" {
		return PublishSource{}, errors.New("session repository and base ref are required for publishing")
	}
	workspaceClaim := agent.WorkspaceClaimName(session)
	claim, err := c.typed.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, workspaceClaim, metav1.GetOptions{})
	if err != nil {
		return PublishSource{}, fmt.Errorf("get workspace PersistentVolumeClaim: %w", err)
	}
	if claim.Status.Phase != corev1.ClaimBound {
		return PublishSource{}, fmt.Errorf("workspace PersistentVolumeClaim is %s, want Bound", claim.Status.Phase)
	}
	return PublishSource{
		Repository:     environment["AGENT_REPOSITORY"],
		Base:           environment["AGENT_REF"],
		Image:          agentContainer.Image,
		WorkspaceClaim: workspaceClaim,
	}, nil
}

// GitHubToken returns the token stored in the namespace's GitHub Secret.
func (c *Client) GitHubToken(ctx context.Context, namespace string) (string, error) {
	secret, err := c.typed.CoreV1().Secrets(namespace).Get(ctx, agent.GitSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get GitHub Secret: %w", err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", errors.New("GitHub Secret does not contain a token")
	}
	return token, nil
}

// PublisherIntent returns the recorded intent and whether the publisher Job exists.
func (c *Client) PublisherIntent(ctx context.Context, namespace, session string) (string, bool, error) {
	job, err := c.typed.BatchV1().Jobs(namespace).Get(ctx, publisherJobName(session), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get publisher Job: %w", err)
	}
	return job.Annotations[publishIntentAnnotation], true, nil
}

// RunPublisher creates or resumes the matching publisher Job and waits for its result.
func (c *Client) RunPublisher(ctx context.Context, request PublishRequest) (PublishResult, error) {
	if request.Timeout < time.Second {
		return PublishResult{}, errors.New("publisher timeout must be at least one second")
	}
	job, err := c.ensurePublisherJob(ctx, request)
	if err != nil {
		return PublishResult{}, err
	}
	if jobFailed(job) {
		if err := c.deletePublisherJob(ctx, request.Namespace, request.Session); err != nil {
			return PublishResult{}, err
		}
		if _, err := c.createPublisherJob(ctx, request); err != nil {
			return PublishResult{}, err
		}
	}
	return c.waitPublisher(ctx, request.Namespace, request.Session, request.Intent, request.Timeout)
}

// DeletePublisher removes a session's publisher Job and waits for its Pods to disappear.
func (c *Client) DeletePublisher(ctx context.Context, namespace, session string) error {
	return c.deletePublisherJob(ctx, namespace, session)
}

// WaitPublisher waits for an existing publisher Job with the expected intent.
func (c *Client) WaitPublisher(ctx context.Context, namespace, session, intent string, timeout time.Duration) (PublishResult, error) {
	return c.waitPublisher(ctx, namespace, session, intent, timeout)
}

func (c *Client) ensurePublisherJob(ctx context.Context, request PublishRequest) (*batchv1.Job, error) {
	job, err := c.typed.BatchV1().Jobs(request.Namespace).Get(ctx, publisherJobName(request.Session), metav1.GetOptions{})
	if err == nil {
		if job.Annotations[publishIntentAnnotation] != request.Intent {
			return nil, errors.New("publisher Job belongs to a different publish request")
		}
		return job, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get publisher Job: %w", err)
	}
	return c.createPublisherJob(ctx, request)
}

func (c *Client) createPublisherJob(ctx context.Context, request PublishRequest) (*batchv1.Job, error) {
	job := publisherJob(request)
	created, err := c.typed.BatchV1().Jobs(request.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return c.ensurePublisherJob(ctx, request)
	}
	if err != nil {
		return nil, fmt.Errorf("create publisher Job: %w", err)
	}
	fmt.Fprintf(c.stdout, "job/%s created\n", job.Name)
	return created, nil
}

func (c *Client) waitPublisher(ctx context.Context, namespace, session, intent string, timeout time.Duration) (PublishResult, error) {
	var result PublishResult
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := c.typed.BatchV1().Jobs(namespace).Get(ctx, publisherJobName(session), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if job.Annotations[publishIntentAnnotation] != intent {
			return false, errors.New("publisher Job intent changed while waiting")
		}
		if jobFailed(job) {
			return false, c.publisherFailure(ctx, namespace, job.Name)
		}
		if !jobComplete(job) {
			return false, nil
		}
		result, err = c.publisherResult(ctx, namespace, job.Name)
		return err == nil, err
	})
	if err != nil {
		return PublishResult{}, fmt.Errorf("publish session %s: %w", session, err)
	}
	return result, nil
}

func (c *Client) publisherResult(ctx context.Context, namespace, jobName string) (PublishResult, error) {
	pod, err := c.publisherPod(ctx, namespace, jobName)
	if err != nil {
		return PublishResult{}, err
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "publisher" || status.State.Terminated == nil {
			continue
		}
		result := parsePublishResult(status.State.Terminated.Message)
		if result.Branch == "" || !commitSHA.MatchString(result.Commit) {
			return PublishResult{}, errors.New("publisher did not report a valid branch and commit")
		}
		return result, nil
	}
	return PublishResult{}, errors.New("publisher container has no termination result")
}

func (c *Client) publisherFailure(ctx context.Context, namespace, jobName string) error {
	pod, err := c.publisherPod(ctx, namespace, jobName)
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

func (c *Client) publisherPod(ctx context.Context, namespace, jobName string) (*corev1.Pod, error) {
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
	propagation := metav1.DeletePropagationBackground
	err := c.typed.BatchV1().Jobs(namespace).Delete(ctx, publisherJobName(session), metav1.DeleteOptions{PropagationPolicy: &propagation})
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

func publisherJob(request PublishRequest) *batchv1.Job {
	backoffLimit := int32(0)
	deadline := int64(request.Timeout.Seconds())
	labels := map[string]string{
		"app.kubernetes.io/name":       "coding-agent",
		"app.kubernetes.io/managed-by": "agentctl",
		"coding-agent/session":         request.Session,
		"coding-agent/component":       "publisher",
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        publisherJobName(request.Session),
			Namespace:   request.Namespace,
			Labels:      labels,
			Annotations: map[string]string{publishIntentAnnotation: request.Intent},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: boolPointer(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPointer(true),
						RunAsUser:    int64Pointer(1000),
						RunAsGroup:   int64Pointer(1000),
						FSGroup:      int64Pointer(1000),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{publisherContainer(request)},
					Volumes: []corev1.Volume{
						{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: request.Workspace,
								ReadOnly:  true,
							}},
						},
						{Name: "publish", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resourceQuantityPointer("4Gi")}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resourceQuantityPointer("1Gi")}}},
						{
							Name: "git-auth",
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
								SecretName:  agent.GitSecretName,
								DefaultMode: int32Pointer(0o440),
							}},
						},
					},
				},
			},
		},
	}
}

func publisherContainer(request PublishRequest) corev1.Container {
	return corev1.Container{
		Name:            "publisher",
		Image:           request.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"publish"},
		WorkingDir:      "/workspace",
		Env: []corev1.EnvVar{
			{Name: "PUBLISH_REPOSITORY", Value: request.Repository},
			{Name: "PUBLISH_BASE", Value: request.Base},
			{Name: "PUBLISH_BRANCH", Value: request.Branch},
			{Name: "PUBLISH_COMMIT_MESSAGE", Value: request.CommitMessage},
			{Name: "PUBLISH_AUTHOR_NAME", Value: request.AuthorName},
			{Name: "PUBLISH_AUTHOR_EMAIL", Value: request.AuthorEmail},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPointer(false),
			ReadOnlyRootFilesystem:   boolPointer(true),
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
			{Name: "workspace", MountPath: "/workspace", ReadOnly: true},
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

func containerEnvironment(container corev1.Container) map[string]string {
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

func parsePublishResult(message string) PublishResult {
	var result PublishResult
	for _, line := range strings.Split(message, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "branch":
			result.Branch = value
		case "commit":
			result.Commit = value
		}
	}
	return result
}

func boolPointer(value bool) *bool {
	return &value
}

func int32Pointer(value int32) *int32 {
	return &value
}

func int64Pointer(value int64) *int64 {
	return &value
}

func resourceQuantity(value string) resource.Quantity {
	return resource.MustParse(value)
}

func resourceQuantityPointer(value string) *resource.Quantity {
	quantity := resourceQuantity(value)
	return &quantity
}
