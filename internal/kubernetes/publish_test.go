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

	"github.com/yarlson/dyne/internal/sessionmanifest"
)

func TestSessionPublishSourceReturnsCompletedUpdateWorkspace(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "review-first", Namespace: "coding-agents", Labels: map[string]string{"coding-agent/session": "review"}},
		Spec:       batchv1.JobSpec{Template: publishableSessionPod("persistent")},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	claim := boundWorkspaceClaim()
	client := &Client{typed: fake.NewClientset(job, claim, completedTaskPod())}
	source, err := client.SessionPublishSource(context.Background(), "coding-agents", "review")
	require.NoError(t, err)

	want := PublishSource{
		Repository:     "https://github.com/lokalise/kargo.git",
		InitialRef:     "main",
		Image:          "coding-agent:test",
		WorkspaceClaim: "session-review",
	}
	assert.Equal(t, want, source)
}

func TestSessionPublishSourceUsesRetainedDefinitionInsteadOfWorkloadEnvironment(t *testing.T) {
	pod := publishableSessionPod("persistent")
	pod.Spec.Containers[0].Image = "stale-image"
	pod.Spec.Containers[0].Env[1].Value = "https://github.com/example/stale.git"
	pod.Spec.Containers[0].Env[2].Value = "stale-ref"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "review-first", Namespace: "coding-agents", Labels: map[string]string{"coding-agent/session": "review"}},
		Spec:       batchv1.JobSpec{Template: pod},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	client := &Client{typed: fake.NewClientset(job, boundWorkspaceClaim(), completedTaskPod())}

	source, err := client.SessionPublishSource(context.Background(), "coding-agents", "review")
	require.NoError(t, err)
	assert.Equal(t, PublishSource{
		Repository:     "https://github.com/lokalise/kargo.git",
		InitialRef:     "main",
		Image:          "coding-agent:test",
		WorkspaceClaim: "session-review",
	}, source)
}

func TestSessionPublishSourceReportsMissingRetainedAgentDefinition(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "review-first", Namespace: "coding-agents", Labels: map[string]string{"coding-agent/session": "review"}},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	claim := boundWorkspaceClaim()
	claim.Annotations[sessionmanifest.SessionAgentAnnotation] = "implementer"
	client := &Client{typed: fake.NewClientset(job, claim, completedTaskPod())}

	_, err := client.SessionPublishSource(context.Background(), "coding-agents", "review")
	require.ErrorContains(t, err, "get agent ConfigMap")
	assert.NotContains(t, err.Error(), "ephemeral")
}

func TestSessionPublishSourceRejectsActiveTask(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "review-first", Namespace: "coding-agents", Labels: map[string]string{"coding-agent/session": "review"}},
		Status:     batchv1.JobStatus{Active: 1},
	}
	client := &Client{typed: fake.NewClientset(job)}
	_, err := client.SessionPublishSource(context.Background(), "coding-agents", "review")
	require.ErrorContains(t, err, "active task")
}

func TestGitHubTokenReturnsTrimmedSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "coding-agent-git-auth", Namespace: "coding-agents"},
		Data:       map[string][]byte{"token": []byte("  github-token\n")},
	}
	client := &Client{typed: fake.NewClientset(secret)}
	token, err := client.GitHubToken(context.Background(), "coding-agents")
	require.NoError(t, err)
	assert.Equal(t, "github-token", token)
}

func TestGitHubTokenRejectsSecretWithoutToken(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "coding-agent-git-auth", Namespace: "coding-agents"}}
	client := &Client{typed: fake.NewClientset(secret)}
	_, err := client.GitHubToken(context.Background(), "coding-agents")
	require.EqualError(t, err, "GitHub Secret does not contain a token")
}

func TestPublisherJobIntentDistinguishesMissingAndOwnedJob(t *testing.T) {
	clientset := fake.NewClientset()
	client := &Client{typed: clientset}
	intent, exists, err := client.PublisherJobIntent(context.Background(), "coding-agents", "review")
	require.NoError(t, err)
	assert.False(t, exists)
	assert.Empty(t, intent)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "review-publish",
			Namespace:   "coding-agents",
			Annotations: map[string]string{"coding-agent/publish-intent": "intent-123"},
		},
	}
	_, err = clientset.BatchV1().Jobs("coding-agents").Create(context.Background(), job, metav1.CreateOptions{})
	require.NoError(t, err)

	intent, exists, err = client.PublisherJobIntent(context.Background(), "coding-agents", "review")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, "intent-123", intent)
}

func TestRunPublisherJobReturnsCompletedTerminationResult(t *testing.T) {
	request := validPublisherJobRequest()
	job := completedPublisherJob(request.IntentID)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "review-publish-pod",
			Namespace: "coding-agents",
			Labels:    map[string]string{"job-name": "review-publish"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "publisher",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: "branch=yar/review\ncommit=9a4484441215661904e02a807adf5034d13f5bbe\ntitle=UmV2aWV3IGNoYW5nZXM=\nbody=UmVhZHkgZm9yIHJldmlldw==\n",
			}},
		}}},
	}
	client := &Client{typed: fake.NewClientset(job, pod), stdout: io.Discard}
	result, err := client.RunPublisherJob(context.Background(), request)
	require.NoError(t, err)

	want := PublisherJobResult{Branch: "yar/review", CommitSHA: "9a4484441215661904e02a807adf5034d13f5bbe", Title: "Review changes", Body: "Ready for review"}
	assert.Equal(t, want, result)
}

func TestRunPublisherJobPreservesJobOwnedByDifferentRequest(t *testing.T) {
	job := completedPublisherJob("another-intent")
	clientset := fake.NewClientset(job)
	client := &Client{typed: clientset, stdout: io.Discard}
	_, err := client.RunPublisherJob(context.Background(), validPublisherJobRequest())
	require.EqualError(t, err, "publisher Job belongs to a different publish request")

	stored, err := clientset.BatchV1().Jobs("coding-agents").Get(context.Background(), "review-publish", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "another-intent", stored.Annotations["coding-agent/publish-intent"])
}

func TestCreatePublisherJobRestrictsWorkspaceAndExecution(t *testing.T) {
	clientset := fake.NewClientset()
	client := &Client{typed: clientset, stdout: io.Discard}
	job, err := client.createPublisherJob(context.Background(), validPublisherJobRequest())
	require.NoError(t, err)
	assert.Equal(t, "review-publish", job.Name)
	assert.Equal(t, "coding-agents", job.Namespace)
	assert.Equal(t, "intent-123", job.Annotations["coding-agent/publish-intent"])
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Zero(t, *job.Spec.BackoffLimit)
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.EqualValues(t, 120, *job.Spec.ActiveDeadlineSeconds)

	pod := job.Spec.Template.Spec
	require.NotNil(t, pod.AutomountServiceAccountToken)
	assert.False(t, *pod.AutomountServiceAccountToken)
	assert.Equal(t, corev1.RestartPolicyNever, pod.RestartPolicy)
	require.Len(t, pod.Containers, 1)

	publisher := pod.Containers[0]
	assert.Equal(t, "publisher", publisher.Name)
	assert.Equal(t, "coding-agent:test", publisher.Image)
	assert.Equal(t, []string{"publish"}, publisher.Args)

	environment := make(map[string]string, len(publisher.Env))
	for _, variable := range publisher.Env {
		environment[variable.Name] = variable.Value
	}

	wantEnvironment := map[string]string{
		"PUBLISH_REPOSITORY":     "https://github.com/lokalise/kargo.git",
		"PUBLISH_BASE":           "main",
		"PUBLISH_BRANCH":         "yar/review",
		"PUBLISH_COMMIT_MESSAGE": "Review changes",
		"PUBLISH_AUTHOR_NAME":    "yar",
		"PUBLISH_AUTHOR_EMAIL":   "12345+yar@users.noreply.github.com",
	}
	assert.Equal(t, wantEnvironment, environment)

	workspace := volumeNamed(t, pod.Volumes, "workspace")
	require.NotNil(t, workspace.PersistentVolumeClaim)
	assert.Equal(t, "session-review", workspace.PersistentVolumeClaim.ClaimName)
	assert.True(t, workspace.PersistentVolumeClaim.ReadOnly)

	tmp := volumeNamed(t, pod.Volumes, "tmp")
	require.NotNil(t, tmp.EmptyDir)
	assert.Equal(t, corev1.StorageMediumMemory, tmp.EmptyDir.Medium)

	gitAuth := volumeNamed(t, pod.Volumes, "git-auth")
	require.NotNil(t, gitAuth.Secret)
	assert.Equal(t, "coding-agent-git-auth", gitAuth.Secret.SecretName)
	require.NotNil(t, publisher.SecurityContext)
	require.NotNil(t, publisher.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *publisher.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, publisher.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *publisher.SecurityContext.AllowPrivilegeEscalation)
}

func TestDeletePublisherJobRemovesExistingJob(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "review-publish", Namespace: "coding-agents"}}
	clientset := fake.NewClientset(job)
	client := &Client{typed: clientset}
	require.NoError(t, client.DeletePublisherJob(context.Background(), "coding-agents", "review"))

	_, err := clientset.BatchV1().Jobs("coding-agents").Get(context.Background(), "review-publish", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "got lookup error %v, want deleted publisher Job", err)
}

func TestParsePublisherJobResultReadsTerminationContract(t *testing.T) {
	result := parsePublisherJobResult("branch=yar/KARGO-123-description\ncommit=7e79cf1ec3840a9340bc9fa07d2ca96c514142d4\ntitle=S0FSR08tMTIzOiBmaXg=\nbody=Rml4ZXMgdGhlIGJ1Zw==\n")
	assert.Equal(t, PublisherJobResult{
		Branch:    "yar/KARGO-123-description",
		CommitSHA: "7e79cf1ec3840a9340bc9fa07d2ca96c514142d4",
		Title:     "KARGO-123: fix",
		Body:      "Fixes the bug",
	}, result)
}

func publishableSessionPod(storage string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name:  "agent",
		Image: "coding-agent:test",
		Env: []corev1.EnvVar{
			{Name: "AGENT_STORAGE", Value: storage},
			{Name: "AGENT_REPOSITORY", Value: "https://github.com/lokalise/kargo.git"},
			{Name: "AGENT_REF", Value: "main"},
		},
	}}}}
}

func boundWorkspaceClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "session-review", Namespace: "coding-agents",
			Annotations: map[string]string{
				"dyne.yarlson.dev/image":       "coding-agent:test",
				"dyne.yarlson.dev/repository":  "https://github.com/lokalise/kargo.git",
				"dyne.yarlson.dev/initial-ref": "main",
				"dyne.yarlson.dev/clone-depth": "1",
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func completedTaskPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "review-pod", Namespace: "coding-agents", Labels: map[string]string{"coding-agent/session": "review"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "agent",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: "outcome=eyJzdGF0dXMiOiJjb21wbGV0ZWQiLCJzdW1tYXJ5IjoiZG9uZSIsImJsb2NrZXIiOiIifQ==\n",
			}},
		}}},
	}
}

func validPublisherJobRequest() PublisherJobRequest {
	return PublisherJobRequest{
		Namespace:      "coding-agents",
		Session:        "review",
		IntentID:       "intent-123",
		Repository:     "https://github.com/lokalise/kargo.git",
		BaseRef:        "main",
		Branch:         "yar/review",
		CommitMessage:  "Review changes",
		AuthorName:     "yar",
		AuthorEmail:    "12345+yar@users.noreply.github.com",
		Image:          "coding-agent:test",
		WorkspaceClaim: "session-review",
		Timeout:        2 * time.Minute,
	}
}

func completedPublisherJob(intent string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "review-publish",
			Namespace:   "coding-agents",
			Annotations: map[string]string{"coding-agent/publish-intent": intent},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
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
