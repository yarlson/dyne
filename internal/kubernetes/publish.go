package kubernetes

import (
	"context"
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

	"github.com/yarlson/dyne/internal/publish"
)

const publishIntentAnnotationKey = "coding-agent/publish-intent"

var publisherCommitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// RunPublisher ensures the correlated publisher Job exists and waits for its result.
func (c *Client) RunPublisher(ctx context.Context, request publish.RuntimeRequest) (publish.RuntimeResult, error) {
	if request.Timeout < time.Second {
		return publish.RuntimeResult{}, errors.New("publisher timeout must be at least one second")
	}

	if strings.TrimSpace(request.RepositoryCredential) == "" {
		return publish.RuntimeResult{}, errors.New("publisher repository credential is required")
	}

	existing, err := c.typed.BatchV1().Jobs(c.namespace).Get(ctx, publisherJobName(request.Session), metav1.GetOptions{})
	if err == nil && existing.Annotations[publishIntentAnnotationKey] != request.IntentID {
		return publish.RuntimeResult{}, errors.New("publisher Job belongs to a different publish intent")
	}

	if err != nil && !apierrors.IsNotFound(err) {
		return publish.RuntimeResult{}, fmt.Errorf("get publisher Job: %w", err)
	}

	if err := c.ensurePublisherCredential(ctx, request); err != nil {
		return publish.RuntimeResult{}, err
	}

	job, err := c.ensurePublisherJob(ctx, request)
	if err != nil {
		return publish.RuntimeResult{}, err
	}

	if jobConditionTrue(job, batchv1.JobFailed) {
		if err := c.deletePublisherJob(ctx, request.Session); err != nil {
			return publish.RuntimeResult{}, err
		}

		if _, err := c.createPublisherJob(ctx, request); err != nil {
			return publish.RuntimeResult{}, err
		}
	}

	return c.waitForPublisherJob(ctx, request.Session, request.IntentID, request.Timeout)
}

// DeletePublisher removes one disposable publisher Job and its credential Secret.
func (c *Client) DeletePublisher(ctx context.Context, sessionName string) error {
	jobErr := c.deletePublisherJob(ctx, sessionName)
	secretErr := c.typed.CoreV1().Secrets(c.namespace).Delete(
		ctx, publisherCredentialName(sessionName), metav1.DeleteOptions{},
	)
	if apierrors.IsNotFound(secretErr) {
		secretErr = nil
	}

	if secretErr != nil {
		secretErr = fmt.Errorf("delete publisher credential Secret: %w", secretErr)
	}

	return errors.Join(jobErr, secretErr)
}

func (c *Client) ensurePublisherCredential(ctx context.Context, request publish.RuntimeRequest) error {
	secrets := c.typed.CoreV1().Secrets(c.namespace)
	name := publisherCredentialName(request.Session)
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace, Labels: publisherLabels(request.Session)},
			Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{"token": []byte(request.RepositoryCredential)},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create publisher credential Secret: %w", err)
		}

		return nil
	}

	if err != nil {
		return fmt.Errorf("get publisher credential Secret: %w", err)
	}

	secret.Data = map[string][]byte{"token": []byte(request.RepositoryCredential)}
	secret.Labels = publisherLabels(request.Session)
	if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update publisher credential Secret: %w", err)
	}

	return nil
}

func (c *Client) ensurePublisherJob(ctx context.Context, request publish.RuntimeRequest) (*batchv1.Job, error) {
	job, err := c.typed.BatchV1().Jobs(c.namespace).Get(ctx, publisherJobName(request.Session), metav1.GetOptions{})
	if err == nil {
		if job.Annotations[publishIntentAnnotationKey] != request.IntentID {
			return nil, errors.New("publisher Job belongs to a different publish intent")
		}

		return job, nil
	}

	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get publisher Job: %w", err)
	}

	return c.createPublisherJob(ctx, request)
}

func (c *Client) createPublisherJob(ctx context.Context, request publish.RuntimeRequest) (*batchv1.Job, error) {
	job := publisherJob(c.namespace, request)
	created, err := c.typed.BatchV1().Jobs(c.namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return c.ensurePublisherJob(ctx, request)
	}

	if err != nil {
		return nil, fmt.Errorf("create publisher Job: %w", err)
	}

	_, _ = fmt.Fprintf(c.stdout, "job/%s created\n", job.Name)

	return created, nil
}

func (c *Client) waitForPublisherJob(
	ctx context.Context, sessionName, intentID string, timeout time.Duration,
) (publish.RuntimeResult, error) {
	var result publish.RuntimeResult
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := c.typed.BatchV1().Jobs(c.namespace).Get(ctx, publisherJobName(sessionName), metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if job.Annotations[publishIntentAnnotationKey] != intentID {
			return false, errors.New("publisher Job intent changed while waiting")
		}

		if jobConditionTrue(job, batchv1.JobFailed) {
			return false, c.publisherJobFailure(ctx, job.Name)
		}

		if !jobConditionTrue(job, batchv1.JobComplete) {
			return false, nil
		}

		result, err = c.publisherJobResult(ctx, job.Name)

		return err == nil, err
	})
	if err != nil {
		return publish.RuntimeResult{}, fmt.Errorf("publish session %s: %w", sessionName, err)
	}

	return result, nil
}

func (c *Client) publisherJobResult(ctx context.Context, jobName string) (publish.RuntimeResult, error) {
	pod, err := c.publisherJobPod(ctx, jobName)
	if err != nil {
		return publish.RuntimeResult{}, err
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "publisher" || status.State.Terminated == nil {
			continue
		}

		result := parsePublisherJobResult(status.State.Terminated.Message)
		if result.Branch == "" || !publisherCommitSHAPattern.MatchString(result.CommitSHA) {
			return publish.RuntimeResult{}, errors.New("publisher did not report a valid branch and commit")
		}

		return result, nil
	}

	return publish.RuntimeResult{}, errors.New("publisher container has no termination result")
}

func (c *Client) publisherJobFailure(ctx context.Context, jobName string) error {
	pod, err := c.publisherJobPod(ctx, jobName)
	if err != nil {
		return err
	}

	contents, logErr := c.typed.CoreV1().Pods(c.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "publisher"}).Do(ctx).Raw()
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

func (c *Client) publisherJobPod(ctx context.Context, jobName string) (*corev1.Pod, error) {
	pods, err := c.typed.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
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

func (c *Client) deletePublisherJob(ctx context.Context, sessionName string) error {
	err := c.typed.BatchV1().Jobs(c.namespace).Delete(ctx, publisherJobName(sessionName), metav1.DeleteOptions{
		PropagationPolicy: new(metav1.DeletePropagationBackground),
	})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("delete publisher Job: %w", err)
	}

	return wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		_, jobErr := c.typed.BatchV1().Jobs(c.namespace).Get(ctx, publisherJobName(sessionName), metav1.GetOptions{})
		if jobErr != nil && !apierrors.IsNotFound(jobErr) {
			return false, jobErr
		}

		pods, podErr := c.typed.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.Set{"job-name": publisherJobName(sessionName)}.AsSelector().String(),
		})
		if podErr != nil {
			return false, podErr
		}

		return apierrors.IsNotFound(jobErr) && len(pods.Items) == 0, nil
	})
}

func publisherJob(namespace string, request publish.RuntimeRequest) *batchv1.Job {
	jobLabels := publisherLabels(request.Session)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: publisherJobName(request.Session), Namespace: namespace, Labels: jobLabels,
			Annotations: map[string]string{publishIntentAnnotationKey: request.IntentID},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: new(int32(0)), ActiveDeadlineSeconds: new(int64(request.Timeout.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false), RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true), RunAsUser: new(int64(1000)), RunAsGroup: new(int64(1000)),
						FSGroup:        new(int64(1000)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{publisherContainer(request)},
					Volumes: []corev1.Volume{
						{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: sessionClaimName(request.Session), ReadOnly: true,
							}},
						},
						{
							Name: "artifacts",
							VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: sessionClaimName(request.Session), ReadOnly: true,
							}},
						},
						{Name: "publish", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resourceQuantity("4Gi"))}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: new(resourceQuantity("1Gi"))}}},
						{
							Name: "git-auth",
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
								SecretName: publisherCredentialName(request.Session), DefaultMode: new(int32(0o440)),
							}},
						},
					},
				},
			},
		},
	}
}

func publisherContainer(request publish.RuntimeRequest) corev1.Container {
	return corev1.Container{
		Name: "publisher", Image: request.Image, ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{"publish"}, WorkingDir: "/workspace",
		Env: []corev1.EnvVar{
			{Name: "PUBLISH_REPOSITORY", Value: request.Repository},
			{Name: "PUBLISH_BASE", Value: request.BaseRef},
			{Name: "PUBLISH_BRANCH", Value: request.Branch},
			{Name: "PUBLISH_COMMIT_MESSAGE", Value: request.CommitMessage},
			{Name: "PUBLISH_AUTHOR_NAME", Value: request.AuthorName},
			{Name: "PUBLISH_AUTHOR_EMAIL", Value: request.AuthorEmail},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false), ReadOnlyRootFilesystem: new(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resourceQuantity("250m"), corev1.ResourceMemory: resourceQuantity("512Mi"),
				corev1.ResourceEphemeralStorage: resourceQuantity("512Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: resourceQuantity("2"), corev1.ResourceMemory: resourceQuantity("4Gi"),
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

func publisherLabels(sessionName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": "coding-agent", "app.kubernetes.io/managed-by": "dyne",
		"coding-agent/session": sessionName, "coding-agent/component": "publisher",
	}
}

func publisherJobName(sessionName string) string        { return sessionName + "-publish" }
func publisherCredentialName(sessionName string) string { return sessionName + "-publish-git" }

func parsePublisherJobResult(message string) publish.RuntimeResult {
	var result publish.RuntimeResult
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
		}
	}

	return result
}

func resourceQuantity(value string) resource.Quantity { return resource.MustParse(value) }
