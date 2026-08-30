package kubernetes

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/yarlson/dyne/internal/session"
)

func TestObserveReturnsCompletedTaskArtifacts(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "review", Namespace: "coding-agents"},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-pod", Namespace: "coding-agents",
			Labels: map[string]string{"coding-agent/session": "review", "coding-agent/task": "review"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: "outcome=eyJzdGF0dXMiOiJjb21wbGV0ZWQifQ==\npull-request=eyJ0aXRsZSI6IkZpeCIsImJvZHkiOiJCb2R5In0=\n",
			}},
		}}},
	}
	client := &Client{typed: fake.NewClientset(job, pod), namespace: "coding-agents"}

	observation, err := client.Observe(context.Background(), "review", "review")
	require.NoError(t, err)
	assert.Equal(t, session.TaskCompleted, observation.State)
	assert.JSONEq(t, `{"status":"completed"}`, string(observation.Artifacts.Outcome))
	assert.JSONEq(t, `{"title":"Fix","body":"Body"}`, string(observation.Artifacts.PullRequest))
}

func TestObserveReturnsRunningWithoutInspectingOtherResources(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "review-next", Namespace: "coding-agents"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	client := &Client{typed: fake.NewClientset(job), namespace: "coding-agents"}

	observation, err := client.Observe(context.Background(), "review", "review-next")
	require.NoError(t, err)
	assert.Equal(t, session.TaskRunning, observation.State)
}

func TestDeleteRemovesDisposableProjectionsAndRetainsPVC(t *testing.T) {
	resourceLabels := map[string]string{"coding-agent/session": "review"}
	clientset := fake.NewClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "review", Namespace: "coding-agents", Labels: resourceLabels}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "session-review-agent", Namespace: "coding-agents", Labels: resourceLabels}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "review-git", Namespace: "coding-agents", Labels: resourceLabels}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "session-review", Namespace: "coding-agents", Labels: resourceLabels}},
	)
	client := &Client{typed: clientset, stdout: io.Discard, namespace: "coding-agents"}

	require.NoError(t, client.Delete(context.Background(), "review", false))
	_, err := clientset.BatchV1().Jobs("coding-agents").Get(context.Background(), "review", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = clientset.CoreV1().ConfigMaps("coding-agents").Get(context.Background(), "session-review-agent", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = clientset.CoreV1().Secrets("coding-agents").Get(context.Background(), "review-git", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = clientset.CoreV1().PersistentVolumeClaims("coding-agents").Get(context.Background(), "session-review", metav1.GetOptions{})
	assert.NoError(t, err)
}

func TestDeleteStorageReportsPVCFailure(t *testing.T) {
	clientset := fake.NewClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "session-review", Namespace: "coding-agents"},
	})
	clientset.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})
	client := &Client{typed: clientset, stdout: io.Discard, namespace: "coding-agents"}

	err := client.Delete(context.Background(), "review", true)
	require.ErrorIs(t, err, assert.AnError)
	assert.ErrorContains(t, err, "delete PersistentVolumeClaim session-review")
}

func TestParseTaskArtifactsReturnsWorkflowOutput(t *testing.T) {
	artifacts, err := parseTaskArtifacts("outcome=eyJzdGF0dXMiOiJjb21wbGV0ZWQifQ==\nworkflow-output=eyJmaW5kaW5ncyI6WyJvbmUiXX0=\n")
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"completed"}`, string(artifacts.Outcome))
	assert.JSONEq(t, `{"findings":["one"]}`, string(artifacts.WorkflowOutput))
}
