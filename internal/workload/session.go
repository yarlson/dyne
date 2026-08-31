package workload

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Start ensures one complete task projection exists in Kubernetes.
func (c *Runtime) Start(ctx context.Context, request TaskRequest) error {
	spec := manifestSpec(request, c.namespace)
	var (
		manifest []byte
		err      error
	)
	if request.Resume {
		manifest, err = renderContinuationManifest(spec)
	} else {
		manifest, err = renderSessionManifest(spec)
	}

	if err != nil {
		return err
	}

	return c.apply(ctx, manifest)
}

func manifestSpec(request TaskRequest, namespace string) sessionManifestSpec {
	return sessionManifestSpec{
		Name: request.SessionName, TaskName: request.TaskName, Namespace: namespace, Image: request.Image,
		Storage: request.Storage, Repository: request.Repository, InitialRef: request.InitialRef,
		SetupCommand: request.SetupCommand, Prompt: request.Prompt,
		AgentName: request.AgentName, Instructions: request.Instructions, Skills: request.Skills,
		CloneDepth: request.CloneDepth, StorageSize: request.StorageSize,
		TimeoutSeconds: int64(request.Timeout.Seconds()), ResultKind: request.ResultKind,
		WorkflowRun: request.WorkflowRun, WorkflowStep: request.WorkflowStep,
		Resume: request.Resume, GitCredential: request.RepositoryCredential,
	}
}

// Observe returns the current runtime evidence for one task.
func (c *Runtime) Observe(ctx context.Context, sessionName, taskID string) (TaskObservation, error) {
	job, err := c.typed.BatchV1().Jobs(c.namespace).Get(ctx, taskID, metav1.GetOptions{})
	if err != nil {
		return TaskObservation{}, fmt.Errorf("get session %s task %s Job: %w", sessionName, taskID, err)
	}

	if job.Status.Succeeded > 0 || jobConditionTrue(job, batchv1.JobComplete) {
		artifacts, err := c.taskArtifacts(ctx, sessionName, taskID)
		if err != nil {
			return TaskObservation{}, err
		}

		return TaskObservation{Phase: TaskSucceeded, Artifacts: artifacts}, nil
	}

	if job.Status.Failed > 0 || jobConditionTrue(job, batchv1.JobFailed) {
		artifacts, _ := c.taskArtifacts(ctx, sessionName, taskID)

		return TaskObservation{Phase: TaskFailed, Artifacts: artifacts, Failure: jobFailure(job)}, nil
	}

	if job.Status.Active > 0 {
		return TaskObservation{Phase: TaskRunning}, nil
	}

	return TaskObservation{Phase: TaskPending}, nil
}

// WriteLogs streams logs from one task's newest agent Pod.
func (c *Runtime) WriteLogs(
	ctx context.Context, sessionName, taskID string, follow bool, output io.Writer,
) (result error) {
	pod, err := c.taskPod(ctx, sessionName, taskID)
	if err != nil {
		return err
	}

	stream, err := c.typed.CoreV1().Pods(c.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: "agent", Follow: follow,
	}).Stream(ctx)
	if err != nil {
		return fmt.Errorf("open logs for Pod %s: %w", pod.Name, err)
	}

	defer func() {
		if err := stream.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close logs for Pod %s: %w", pod.Name, err))
		}
	}()

	if _, err := io.Copy(output, stream); err != nil {
		return fmt.Errorf("stream logs for Pod %s: %w", pod.Name, err)
	}

	return nil
}

// Delete removes disposable projections and optionally the session PVC.
func (c *Runtime) Delete(ctx context.Context, name string, deleteStorage bool) error {
	selector := sessionSelector(name)
	var deleteErrors []error

	jobs, err := c.typed.BatchV1().Jobs(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		deleteErrors = append(deleteErrors, fmt.Errorf("list session Jobs for deletion: %w", err))
	} else {
		for i := range jobs.Items {
			if err := c.typed.BatchV1().Jobs(c.namespace).Delete(ctx, jobs.Items[i].Name, metav1.DeleteOptions{
				PropagationPolicy: new(metav1.DeletePropagationBackground),
			}); err != nil && !apierrors.IsNotFound(err) {
				deleteErrors = append(deleteErrors, fmt.Errorf("delete Job %s: %w", jobs.Items[i].Name, err))
			}
		}
	}

	configMaps, err := c.typed.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		deleteErrors = append(deleteErrors, fmt.Errorf("list session ConfigMaps for deletion: %w", err))
	} else {
		for i := range configMaps.Items {
			if err := c.typed.CoreV1().ConfigMaps(c.namespace).Delete(ctx, configMaps.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				deleteErrors = append(deleteErrors, fmt.Errorf("delete ConfigMap %s: %w", configMaps.Items[i].Name, err))
			}
		}
	}

	secrets, err := c.typed.CoreV1().Secrets(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		deleteErrors = append(deleteErrors, fmt.Errorf("list session Secrets for deletion: %w", err))
	} else {
		for i := range secrets.Items {
			if err := c.typed.CoreV1().Secrets(c.namespace).Delete(ctx, secrets.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				deleteErrors = append(deleteErrors, fmt.Errorf("delete Secret %s: %w", secrets.Items[i].Name, err))
			}
		}
	}

	if deleteStorage {
		claim := sessionClaimName(name)
		if err := c.typed.CoreV1().PersistentVolumeClaims(c.namespace).Delete(ctx, claim, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			deleteErrors = append(deleteErrors, fmt.Errorf("delete PersistentVolumeClaim %s: %w", claim, err))
		}
	}

	return errors.Join(deleteErrors...)
}

func (c *Runtime) taskArtifacts(ctx context.Context, sessionName, taskID string) (TaskArtifacts, error) {
	pod, err := c.taskPod(ctx, sessionName, taskID)
	if err != nil {
		return TaskArtifacts{}, err
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "agent" && status.State.Terminated != nil {
			return parseTaskArtifacts(status.State.Terminated.Message)
		}
	}

	return TaskArtifacts{}, fmt.Errorf("session Pod %s has no terminated agent result", pod.Name)
}

func (c *Runtime) taskPod(ctx context.Context, sessionName, taskID string) (*corev1.Pod, error) {
	selector := labels.Set{
		"coding-agent/session": sessionName,
		"coding-agent/task":    taskID,
	}.AsSelector().String()
	pods, err := c.typed.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list session %s task %s Pods: %w", sessionName, taskID, err)
	}

	if len(pods.Items) == 0 {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, taskID)
	}

	newest := &pods.Items[0]
	for i := 1; i < len(pods.Items); i++ {
		candidate := &pods.Items[i]
		if candidate.CreationTimestamp.After(newest.CreationTimestamp.Time) ||
			(candidate.CreationTimestamp.Equal(&newest.CreationTimestamp) && candidate.Name > newest.Name) {
			newest = candidate
		}
	}

	return newest, nil
}

func parseTaskArtifacts(message string) (TaskArtifacts, error) {
	var result TaskArtifacts
	for line := range strings.SplitSeq(message, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		contents, err := base64.StdEncoding.DecodeString(value)
		if err != nil || !json.Valid(contents) {
			return TaskArtifacts{}, fmt.Errorf("task artifact %s is invalid", key)
		}

		switch key {
		case "outcome":
			result.Outcome = contents
		case "pull-request":
			result.PullRequest = contents
		case "workflow-output":
			result.WorkflowOutput = contents
		}
	}

	if len(result.Outcome) == 0 {
		return TaskArtifacts{}, errors.New("task did not report an outcome artifact")
	}

	return result, nil
}

func sessionSelector(name string) string {
	return labels.Set{"coding-agent/session": name}.AsSelector().String()
}

func jobFailure(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			if strings.TrimSpace(condition.Message) != "" {
				return condition.Message
			}

			return condition.Reason
		}
	}

	return "session task failed"
}

func jobConditionTrue(job *batchv1.Job, conditionType batchv1.JobConditionType) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}
