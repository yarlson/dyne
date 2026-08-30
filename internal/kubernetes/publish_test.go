package kubernetes

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/yarlson/dyne/internal/publish"
)

func TestRunPublisherUsesExecutionScopedCredentialAndCompletedResult(t *testing.T) {
	request := validPublisherRequest()
	clientset := fake.NewClientset(completedPublisherJob(request.IntentID), completedPublisherPod())
	client := &Client{typed: clientset, stdout: io.Discard, namespace: "coding-agents"}

	result, err := client.RunPublisher(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, publish.RuntimeResult{Branch: "yar/review", CommitSHA: validPublisherCommit}, result)
	secret, err := clientset.CoreV1().Secrets("coding-agents").Get(context.Background(), "review-publish-git", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("installation-token"), secret.Data["token"])
}

func TestRunPublisherPreservesJobWithDifferentCorrelationIntent(t *testing.T) {
	clientset := fake.NewClientset(completedPublisherJob("another-intent"))
	client := &Client{
		typed:  clientset,
		stdout: io.Discard, namespace: "coding-agents",
	}

	_, err := client.RunPublisher(context.Background(), validPublisherRequest())
	require.EqualError(t, err, "publisher Job belongs to a different publish intent")
	_, err = clientset.CoreV1().Secrets("coding-agents").Get(context.Background(), "review-publish-git", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestPublisherJobUsesSessionPVCAndScopedSecret(t *testing.T) {
	job := publisherJob("coding-agents", validPublisherRequest())
	assert.Equal(t, "review-publish", job.Name)
	assert.Equal(t, "intent-123", job.Annotations[publishIntentAnnotationKey])

	workspace := volumeNamed(t, job.Spec.Template.Spec.Volumes, "workspace")
	require.NotNil(t, workspace.PersistentVolumeClaim)
	assert.Equal(t, "session-review", workspace.PersistentVolumeClaim.ClaimName)
	gitAuth := volumeNamed(t, job.Spec.Template.Spec.Volumes, "git-auth")
	require.NotNil(t, gitAuth.Secret)
	assert.Equal(t, "review-publish-git", gitAuth.Secret.SecretName)
}

func TestDeletePublisherRemovesJobAndCredential(t *testing.T) {
	clientset := fake.NewClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "review-publish", Namespace: "coding-agents"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "review-publish-git", Namespace: "coding-agents"}},
	)
	client := &Client{typed: clientset, namespace: "coding-agents"}
	require.NoError(t, client.DeletePublisher(context.Background(), "review"))

	_, err := clientset.BatchV1().Jobs("coding-agents").Get(context.Background(), "review-publish", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = clientset.CoreV1().Secrets("coding-agents").Get(context.Background(), "review-publish-git", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestParsePublisherJobResultReadsBranchAndCommit(t *testing.T) {
	result := parsePublisherJobResult("branch=yar/review\ncommit=" + validPublisherCommit + "\ntitle=ignored\n")
	assert.Equal(t, publish.RuntimeResult{Branch: "yar/review", CommitSHA: validPublisherCommit}, result)
}

const validPublisherCommit = "9a4484441215661904e02a807adf5034d13f5bbe"

func validPublisherRequest() publish.RuntimeRequest {
	return publish.RuntimeRequest{
		Session: "review", IntentID: "intent-123", Image: "coding-agent:test",
		Repository: "https://github.com/lokalise/ratchet-test-service", RepositoryCredential: "installation-token",
		BaseRef: "main", Branch: "yar/review", CommitMessage: "Fix link",
		AuthorName: "yar", AuthorEmail: "12345+yar@users.noreply.github.com", Timeout: 2 * time.Minute,
	}
}

func completedPublisherJob(intent string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-publish", Namespace: "coding-agents",
			Annotations: map[string]string{publishIntentAnnotationKey: intent},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
}

func completedPublisherPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-publish-pod", Namespace: "coding-agents", Labels: map[string]string{"job-name": "review-publish"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "publisher", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: "branch=yar/review\ncommit=" + validPublisherCommit + "\n",
			}},
		}}},
	}
}

func volumeNamed(t *testing.T, volumes []corev1.Volume, name string) corev1.VolumeSource {
	t.Helper()
	for _, volume := range volumes {
		if volume.Name == name {
			return volume.VolumeSource
		}
	}

	require.FailNowf(t, "publisher Pod is missing a volume", "volume %s", name)

	return corev1.VolumeSource{}
}
