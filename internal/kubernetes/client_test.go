package kubernetes

import (
	"context"
	"io"
	"strings"
	"testing"

	"coding-agent-k8s/internal/agent"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckSessionModeAvailableRejectsConflictingWorkloadKind(t *testing.T) {
	tests := []struct {
		name     string
		mode     agent.Mode
		workload runtime.Object
		message  string
	}{
		{
			name:     "long session with existing Job",
			mode:     agent.ModeLong,
			workload: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"}},
			message:  "job example already exists",
		},
		{
			name:     "bounded session with existing StatefulSet",
			mode:     agent.ModeUpdate,
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
