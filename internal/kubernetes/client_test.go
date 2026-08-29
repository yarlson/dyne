package kubernetes

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	if err != nil {
		t.Fatal(err)
	}

	want := []ResourceStatus{
		{Kind: "Job", Name: "example", Ready: "1/1", State: "Complete"},
		{Kind: "Pod", Name: "example-pod", Ready: "1/1", State: "Running"},
		{Kind: "PersistentVolumeClaim", Name: "workspace-example", Ready: "-", State: "Bound"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got status %#v, want %#v", got, want)
	}
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
	if err != nil {
		t.Fatal(err)
	}

	want := []ResourceStatus{{Kind: "StatefulSet", Name: "long-example", Ready: "0/0", State: "Stopped"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got status %#v, want %#v", got, want)
	}
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
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("got error %v, want message containing %q", err, test.message)
			}
		})
	}
}
