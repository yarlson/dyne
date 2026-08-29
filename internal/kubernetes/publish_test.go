package kubernetes

import (
	"context"
	"io"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParsePublisherJobResultReadsTerminationContract(t *testing.T) {
	result := parsePublisherJobResult("branch=yar/KARGO-123-description\ncommit=7e79cf1ec3840a9340bc9fa07d2ca96c514142d4\n")
	if result.Branch != "yar/KARGO-123-description" || result.CommitSHA != "7e79cf1ec3840a9340bc9fa07d2ca96c514142d4" {
		t.Fatalf("got result %#v", result)
	}
}

func TestResumeSessionRefusesAnActivePublisherJob(t *testing.T) {
	replicas := int32(0)
	publisher := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "example-publish", Namespace: "coding-agents"}}
	session := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "coding-agents"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
	}
	clientset := fake.NewSimpleClientset(publisher, session)
	client := &Client{typed: clientset, stdout: io.Discard}
	err := client.ResumeSession(context.Background(), "coding-agents", "example")
	if err == nil || !strings.Contains(err.Error(), "cannot resume while publisher Job example-publish is active") {
		t.Fatalf("got error %v", err)
	}

	got, err := clientset.AppsV1().StatefulSets("coding-agents").Get(context.Background(), "example", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Fatalf("got replicas %v, want stopped session to remain at zero", got.Spec.Replicas)
	}
}
