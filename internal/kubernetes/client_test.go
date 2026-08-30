package kubernetes

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/yarlson/airlock/internal/sessionmanifest"
)

func TestSessionStatusDescribesOwnedResources(t *testing.T) {
	labels := map[string]string{"coding-agent/session": "example"}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents", Labels: labels},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "example-pod", Namespace: "coding-agents", Labels: labels},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-example", Namespace: "coding-agents", Labels: labels},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	client := &Client{typed: fake.NewClientset(job, pod, claim)}
	got, err := client.SessionStatus(context.Background(), "coding-agents", "example")
	require.NoError(t, err)

	want := []ResourceStatus{
		{Kind: "Job", Name: "example", Ready: "1/1", State: "Complete"},
		{Kind: "Pod", Name: "example-pod", Ready: "1/1", State: "Running"},
		{Kind: "PersistentVolumeClaim", Name: "workspace-example", Ready: "-", State: "Bound"},
	}
	assert.Equal(t, want, got)
}

func TestCheckSessionAvailableRejectsExistingSessionResources(t *testing.T) {
	labels := map[string]string{"coding-agent/session": "example"}
	client := &Client{typed: fake.NewClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "example-task", Namespace: "coding-agents", Labels: labels},
	}), stdout: io.Discard}
	err := client.CheckSessionAvailable(context.Background(), "coding-agents", "example")
	require.ErrorContains(t, err, "session example already exists")
}

func TestCheckSessionAvailableRejectsRetainedAgentConfiguration(t *testing.T) {
	labels := map[string]string{"coding-agent/session": "example"}
	client := &Client{typed: fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "session-example-agent", Namespace: "coding-agents", Labels: labels},
	}), stdout: io.Discard}

	err := client.CheckSessionAvailable(context.Background(), "coding-agents", "example")
	require.ErrorContains(t, err, "session example already exists")
}

func TestSetGitHubTokenReplacesOnlyRepositoryCredential(t *testing.T) {
	clientset := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sessionmanifest.GitHubTokenSecretName, Namespace: "coding-agents"},
		Data:       map[string][]byte{"token": []byte("old"), "unrelated": []byte("keep")},
	})
	client := &Client{typed: clientset}
	require.NoError(t, client.SetGitHubToken(context.Background(), "coding-agents", "short-lived"))

	secret, err := clientset.CoreV1().Secrets("coding-agents").Get(context.Background(), sessionmanifest.GitHubTokenSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("short-lived"), secret.Data["token"])
	assert.Equal(t, []byte("keep"), secret.Data["unrelated"])
}

func TestPersistentSessionDefinitionSurvivesWorkloadDeletion(t *testing.T) {
	labels := map[string]string{"coding-agent/session": "example"}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-example",
			Namespace: "coding-agents",
			Labels:    labels,
			Annotations: map[string]string{
				"airlock.yarlson.dev/image":       "coding-agent:test",
				"airlock.yarlson.dev/repository":  "https://github.com/lokalise/kargo.git",
				"airlock.yarlson.dev/initial-ref": "main",
				"airlock.yarlson.dev/setup":       "make tools",
				"airlock.yarlson.dev/clone-depth": "1",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	client := &Client{typed: fake.NewClientset(claim)}
	definition, err := client.persistentSessionDefinition(context.Background(), "coding-agents", "example")
	require.NoError(t, err)
	assert.Equal(t, sessionDefinition{
		Image:        "coding-agent:test",
		Repository:   "https://github.com/lokalise/kargo.git",
		InitialRef:   "main",
		SetupCommand: "make tools",
		CloneDepth:   1,
	}, definition)
}

func TestPersistentAgentSessionDefinitionRequiresRetainedConfiguration(t *testing.T) {
	labels := map[string]string{"coding-agent/session": "example"}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-example",
			Namespace: "coding-agents",
			Labels:    labels,
			Annotations: map[string]string{
				"airlock.yarlson.dev/image":       "coding-agent:test",
				"airlock.yarlson.dev/initial-ref": "main",
				"airlock.yarlson.dev/clone-depth": "1",
				"airlock.yarlson.dev/agent":       "reviewer",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	client := &Client{typed: fake.NewClientset(claim)}

	_, err := client.persistentSessionDefinition(context.Background(), "coding-agents", "example")
	require.ErrorContains(t, err, "get agent ConfigMap")

	immutable := true
	client.typed = fake.NewClientset(claim, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sessionmanifest.SessionAgentConfigName("example"),
			Namespace:   "coding-agents",
			Labels:      labels,
			Annotations: map[string]string{"airlock.yarlson.dev/agent": "reviewer"},
		},
		Immutable: &immutable,
		Data: map[string]string{
			"instructions":      "Review correctness and tests.",
			"skill-code-review": "retained skill",
		},
	})
	definition, err := client.persistentSessionDefinition(context.Background(), "coding-agents", "example")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", definition.AgentName)
	assert.Equal(t, []sessionmanifest.AgentSkill{{Name: "code-review", Contents: "retained skill"}}, definition.Skills)
}

func TestWriteSessionLogsReportsMissingSession(t *testing.T) {
	client := &Client{typed: fake.NewClientset()}
	err := client.WriteSessionLogs(context.Background(), "coding-agents", "missing", false, io.Discard)
	require.True(t, apierrors.IsNotFound(err), "got error %v, want missing Pod", err)
}

func TestSessionArtifactsReturnsNewestTerminatedAgentResult(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "example-pod", Namespace: "coding-agents", Labels: map[string]string{"coding-agent/session": "example"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "agent",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: "outcome=eyJzdGF0dXMiOiJibG9ja2VkIiwic3VtbWFyeSI6Im5lZWRzIGlucHV0IiwiYmxvY2tlciI6Im1pc3NpbmcgQVBJIGNvbnRyYWN0In0=\n",
			}},
		}}},
	}
	client := &Client{typed: fake.NewClientset(pod)}
	artifacts, err := client.SessionArtifacts(context.Background(), "coding-agents", "example")
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"blocked","summary":"needs input","blocker":"missing API contract"}`, string(artifacts.Outcome))
	assert.Empty(t, artifacts.PullRequest)
}

func TestDeleteSessionRemovesComputeAndRetainsPersistentState(t *testing.T) {
	claimNames := []string{"session-example"}
	client, clientset := sessionClientWithPersistentState(claimNames)
	require.NoError(t, client.DeleteSession(context.Background(), "coding-agents", "example"))

	assertSessionComputeRemoved(t, clientset)

	for _, name := range claimNames {
		_, err := clientset.CoreV1().PersistentVolumeClaims("coding-agents").Get(context.Background(), name, metav1.GetOptions{})
		assert.NoError(t, err, "persistent state %s was not retained", name)
	}
}

func TestDeleteSessionRetainsPersistentAgentConfiguration(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "session-example", Namespace: "coding-agents"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "session-example-agent", Namespace: "coding-agents"}},
	)
	client := &Client{typed: clientset, stdout: io.Discard}

	require.NoError(t, client.DeleteSession(context.Background(), "coding-agents", "example"))
	_, err := clientset.CoreV1().ConfigMaps("coding-agents").Get(context.Background(), "session-example-agent", metav1.GetOptions{})
	assert.NoError(t, err)
}

func TestDeleteSessionRemovesEphemeralAgentConfiguration(t *testing.T) {
	clientset := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "session-example-agent", Namespace: "coding-agents"},
	})
	client := &Client{typed: clientset, stdout: io.Discard}

	require.NoError(t, client.DeleteSession(context.Background(), "coding-agents", "example"))
	_, err := clientset.CoreV1().ConfigMaps("coding-agents").Get(context.Background(), "session-example-agent", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestDestroySessionRemovesComputeAndPersistentState(t *testing.T) {
	claimNames := []string{"session-example"}
	client, clientset := sessionClientWithPersistentState(claimNames)
	require.NoError(t, client.DestroySession(context.Background(), "coding-agents", "example"))

	assertSessionComputeRemoved(t, clientset)

	for _, name := range claimNames {
		_, err := clientset.CoreV1().PersistentVolumeClaims("coding-agents").Get(context.Background(), name, metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "got PersistentVolumeClaim %s lookup error %v, want deleted claim", name, err)
	}
}

func TestDestroySessionRemovesPersistentAgentConfiguration(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "session-example", Namespace: "coding-agents"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "session-example-agent", Namespace: "coding-agents"}},
	)
	client := &Client{typed: clientset, stdout: io.Discard}

	require.NoError(t, client.DestroySession(context.Background(), "coding-agents", "example"))
	_, err := clientset.CoreV1().ConfigMaps("coding-agents").Get(context.Background(), "session-example-agent", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestDestroySessionIgnoresMissingPersistentState(t *testing.T) {
	client := &Client{typed: fake.NewClientset(), stdout: io.Discard}

	require.NoError(t, client.DestroySession(context.Background(), "coding-agents", "example"))
}

func TestDestroySessionReturnsPersistentStorageDeletionFailure(t *testing.T) {
	deleteFailure := errors.New("storage unavailable")
	client, clientset := sessionClientWithPersistentState([]string{"session-example"})
	clientset.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, deleteFailure
	})

	err := client.DestroySession(context.Background(), "coding-agents", "example")
	require.ErrorIs(t, err, deleteFailure)
	assert.ErrorContains(t, err, "delete PersistentVolumeClaim session-example")

	_, err = clientset.CoreV1().PersistentVolumeClaims("coding-agents").Get(context.Background(), "session-example", metav1.GetOptions{})
	require.NoError(t, err)
}

func sessionClientWithPersistentState(claimNames []string) (*Client, *fake.Clientset) {
	objects := []runtime.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "example-first", Namespace: "coding-agents", Labels: map[string]string{"coding-agent/session": "example"}}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "example-second", Namespace: "coding-agents", Labels: map[string]string{"coding-agent/session": "example"}}},
	}
	for _, name := range claimNames {
		objects = append(objects, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "coding-agents"},
		})
	}

	clientset := fake.NewClientset(objects...)

	return &Client{typed: clientset, stdout: io.Discard}, clientset
}

func assertSessionComputeRemoved(t *testing.T, clientset *fake.Clientset) {
	t.Helper()
	jobs, err := clientset.BatchV1().Jobs("coding-agents").List(context.Background(), metav1.ListOptions{LabelSelector: "coding-agent/session=example"})
	require.NoError(t, err)
	assert.Empty(t, jobs.Items)
}
