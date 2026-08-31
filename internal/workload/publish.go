package workload

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
)

const publishIntentAnnotationKey = "coding-agent/publish-intent"

var publisherCommitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// RunPublisher ensures the correlated publisher Job exists and waits for its result.
func (c *Runtime) RunPublisher(ctx context.Context, request PublishRequest) (PublishResult, error) {
	if request.Timeout < time.Second {
		return PublishResult{}, errors.New("publisher timeout must be at least one second")
	}

	if strings.TrimSpace(request.RepositoryCredential) == "" {
		return PublishResult{}, errors.New("publisher repository credential is required")
	}

	if request.Change != nil {
		if !changeSHA256Pattern.MatchString(request.Change.SHA256) {
			return PublishResult{}, errors.New("publisher change sha256 must contain 64 lowercase hexadecimal characters")
		}

		if request.Change.Bytes <= 0 {
			return PublishResult{}, errors.New("publisher change bytes must be greater than zero")
		}
	}

	existing, err := c.typed.BatchV1().Jobs(c.namespace).Get(ctx, publisherJobName(request.Session), metav1.GetOptions{})
	if err == nil && existing.Annotations[publishIntentAnnotationKey] != request.IntentID {
		return PublishResult{}, errors.New("publisher Job belongs to a different publish intent")
	}

	if err != nil && !apierrors.IsNotFound(err) {
		return PublishResult{}, fmt.Errorf("get publisher Job: %w", err)
	}

	if err := c.ensurePublisherCredential(ctx, request); err != nil {
		return PublishResult{}, err
	}

	if _, err := c.ensurePublisherJob(ctx, request); err != nil {
		return PublishResult{}, err
	}

	return c.waitForPublisherJob(ctx, request.Session, request.IntentID, request.Timeout)
}

// DeletePublisher removes one disposable publisher Job and its credential Secret.
func (c *Runtime) DeletePublisher(ctx context.Context, sessionName string) error {
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

func (c *Runtime) ensurePublisherCredential(ctx context.Context, request PublishRequest) error {
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

func (c *Runtime) ensurePublisherJob(ctx context.Context, request PublishRequest) (*batchv1.Job, error) {
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

func (c *Runtime) createPublisherJob(ctx context.Context, request PublishRequest) (*batchv1.Job, error) {
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

func (c *Runtime) waitForPublisherJob(
	ctx context.Context, sessionName, intentID string, timeout time.Duration,
) (PublishResult, error) {
	var result PublishResult
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := c.typed.BatchV1().Jobs(c.namespace).Get(ctx, publisherJobName(sessionName), metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if job.Annotations[publishIntentAnnotationKey] != intentID {
			return false, errors.New("publisher Job intent changed while waiting")
		}

		if jobConditionTrue(job, batchv1.JobFailed) {
			result, _ = c.publisherJobResult(ctx, job.Name)

			return false, errors.Join(ErrExecutionFailed, c.publisherJobFailure(ctx, job.Name))
		}

		if !jobConditionTrue(job, batchv1.JobComplete) {
			return false, nil
		}

		result, err = c.publisherJobResult(ctx, job.Name)

		return err == nil, err
	})
	if err != nil {
		return result, fmt.Errorf("publish session %s: %w", sessionName, err)
	}

	return result, nil
}

func (c *Runtime) publisherJobResult(ctx context.Context, jobName string) (PublishResult, error) {
	pod, err := c.publisherJobPod(ctx, jobName)
	if err != nil {
		return PublishResult{}, err
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "publisher" || status.State.Terminated == nil {
			continue
		}

		result := parsePublisherJobResult(status.State.Terminated.Message)
		if result.Branch == "" || !publisherCommitSHAPattern.MatchString(result.CommitSHA) {
			return PublishResult{}, errors.New("publisher did not report a valid branch and commit")
		}

		return result, nil
	}

	return PublishResult{}, errors.New("publisher container has no termination result")
}

func (c *Runtime) publisherJobFailure(ctx context.Context, jobName string) error {
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

func (c *Runtime) publisherJobPod(ctx context.Context, jobName string) (*corev1.Pod, error) {
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

func (c *Runtime) deletePublisherJob(ctx context.Context, sessionName string) error {
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

func publisherJob(namespace string, request PublishRequest) *batchv1.Job {
	jobLabels := publisherLabels(request.Session)
	volumes := []corev1.Volume{
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
	}
	if request.Change == nil {
		volumes = append([]corev1.Volume{{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: sessionClaimName(request.Session), ReadOnly: true,
			}},
		}}, volumes...)
	}

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
					Volumes:    volumes,
				},
			},
		},
	}
}

func publisherContainer(request PublishRequest) corev1.Container {
	environment := []corev1.EnvVar{
		{Name: "PUBLISH_REPOSITORY", Value: request.Repository},
		{Name: "PUBLISH_BASE", Value: request.BaseRef},
		{Name: "PUBLISH_BRANCH", Value: request.Branch},
		{Name: "PUBLISH_COMMIT_MESSAGE", Value: request.CommitMessage},
		{Name: "PUBLISH_AUTHOR_NAME", Value: request.AuthorName},
		{Name: "PUBLISH_AUTHOR_EMAIL", Value: request.AuthorEmail},
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: "artifacts", MountPath: "/artifacts", SubPath: "artifacts", ReadOnly: true},
		{Name: "publish", MountPath: "/publish"},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "git-auth", MountPath: "/var/run/git-auth", ReadOnly: true},
	}
	if request.Change == nil {
		volumeMounts = append([]corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace", SubPath: "workspace", ReadOnly: true},
		}, volumeMounts...)
	} else {
		environment = append(environment,
			corev1.EnvVar{Name: "PUBLISH_CHANGE_SHA256", Value: request.Change.SHA256},
			corev1.EnvVar{Name: "PUBLISH_CHANGE_BYTES", Value: fmt.Sprintf("%d", request.Change.Bytes)},
		)
	}

	return corev1.Container{
		Name: "publisher", Image: request.Image, ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{"publish"}, WorkingDir: "/publish", Env: environment,
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
		VolumeMounts: volumeMounts,
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

func parsePublisherJobResult(message string) PublishResult {
	var result PublishResult
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
