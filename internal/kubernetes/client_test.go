package kubernetes

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"coding-agent-k8s/internal/sessionmanifest"
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

func TestSessionStatusReportsStoppedLongSession(t *testing.T) {
	replicas := int32(0)
	set := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "long-example",
			Namespace: "coding-agents",
			Labels:    map[string]string{"coding-agent/session": "long-example"},
		},
		Spec: appsv1.StatefulSetSpec{Replicas: &replicas},
	}
	client := &Client{typed: fake.NewClientset(set)}
	got, err := client.SessionStatus(context.Background(), "coding-agents", "long-example")
	require.NoError(t, err)

	want := []ResourceStatus{{Kind: "StatefulSet", Name: "long-example", Ready: "0/0", State: "Stopped"}}
	assert.Equal(t, want, got)
}

func TestCheckSessionModeAvailableRejectsConflictingWorkloadKind(t *testing.T) {
	tests := []struct {
		name     string
		mode     sessionmanifest.Mode
		workload runtime.Object
		message  string
	}{
		{
			name:     "long session with existing Job",
			mode:     sessionmanifest.ModeLong,
			workload: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"}},
			message:  "job example already exists",
		},
		{
			name:     "bounded session with existing StatefulSet",
			mode:     sessionmanifest.ModeUpdate,
			workload: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"}},
			message:  "StatefulSet example already exists",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{typed: fake.NewClientset(test.workload), stdout: io.Discard}
			err := client.CheckSessionModeAvailable(context.Background(), "coding-agents", "example", test.mode)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func TestCheckSessionModeAvailableAllowsExistingWorkloadOfSameKind(t *testing.T) {
	tests := []struct {
		name     string
		mode     sessionmanifest.Mode
		workload runtime.Object
	}{
		{
			name:     "long session",
			mode:     sessionmanifest.ModeLong,
			workload: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"}},
		},
		{
			name:     "bounded update",
			mode:     sessionmanifest.ModeUpdate,
			workload: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{typed: fake.NewClientset(test.workload), stdout: io.Discard}
			require.NoError(t, client.CheckSessionModeAvailable(context.Background(), "coding-agents", "example", test.mode))
		})
	}
}

func TestWaitForReadyPodReturnsNewestReadyAttempt(t *testing.T) {
	labels := map[string]string{"coding-agent/session": "example"}
	old := readyPod("example-old", labels, time.Date(2026, time.August, 29, 8, 0, 0, 0, time.UTC))
	newest := readyPod("example-new", labels, time.Date(2026, time.August, 29, 9, 0, 0, 0, time.UTC))
	unowned := readyPod("another-session", map[string]string{"coding-agent/session": "another"}, time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC))
	client := &Client{typed: fake.NewClientset(old, newest, unowned)}
	name, err := client.WaitForReadyPod(context.Background(), "coding-agents", "example", time.Second)
	require.NoError(t, err)
	assert.Equal(t, "example-new", name)
}

func TestWaitForReadyPodReportsNewestFailedAttempt(t *testing.T) {
	failed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "example-failed",
			Namespace:         "coding-agents",
			Labels:            map[string]string{"coding-agent/session": "example"},
			CreationTimestamp: metav1.NewTime(time.Date(2026, time.August, 29, 9, 0, 0, 0, time.UTC)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	client := &Client{typed: fake.NewClientset(failed)}
	_, err := client.WaitForReadyPod(context.Background(), "coding-agents", "example", time.Second)
	require.ErrorContains(t, err, "pod example-failed failed")
}

func TestNewestPodNameReportsMissingSession(t *testing.T) {
	client := &Client{typed: fake.NewClientset()}
	_, err := client.NewestPodName(context.Background(), "coding-agents", "missing")
	require.True(t, apierrors.IsNotFound(err), "got error %v, want missing Pod", err)
}

func TestDeleteSessionRemovesComputeAndRetainsPersistentState(t *testing.T) {
	claimNames := []string{"workspace-example", "home-example", "codex-example"}
	client, clientset := sessionClientWithPersistentState(claimNames)
	require.NoError(t, client.DeleteSession(context.Background(), "coding-agents", "example"))

	assertSessionComputeRemoved(t, clientset)

	for _, name := range claimNames {
		_, err := clientset.CoreV1().PersistentVolumeClaims("coding-agents").Get(context.Background(), name, metav1.GetOptions{})
		assert.NoError(t, err, "persistent state %s was not retained", name)
	}
}

func TestDestroySessionRemovesComputeAndPersistentState(t *testing.T) {
	claimNames := []string{"workspace-example", "home-example", "codex-example"}
	client, clientset := sessionClientWithPersistentState(claimNames)
	require.NoError(t, client.DestroySession(context.Background(), "coding-agents", "example"))

	assertSessionComputeRemoved(t, clientset)

	for _, name := range claimNames {
		_, err := clientset.CoreV1().PersistentVolumeClaims("coding-agents").Get(context.Background(), name, metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "got PersistentVolumeClaim %s lookup error %v, want deleted claim", name, err)
	}
}

func sessionClientWithPersistentState(claimNames []string) (*Client, *fake.Clientset) {
	objects := []runtime.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"}},
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
	_, err := clientset.BatchV1().Jobs("coding-agents").Get(context.Background(), "example", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "got Job lookup error %v, want deleted Job", err)

	_, err = clientset.AppsV1().StatefulSets("coding-agents").Get(context.Background(), "example", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "got StatefulSet lookup error %v, want deleted StatefulSet", err)

	_, err = clientset.CoreV1().Services("coding-agents").Get(context.Background(), "example", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "got Service lookup error %v, want deleted Service", err)
}

func readyPod(name string, labels map[string]string, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "coding-agents",
			Labels:            labels,
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}
