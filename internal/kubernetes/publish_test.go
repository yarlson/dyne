package kubernetes

import (
	"context"
	"io"
	"strings"
	"testing"

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
	publisher := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "example-publish", Namespace: "coding-agents"}}
	client := &Client{typed: fake.NewSimpleClientset(publisher), stdout: io.Discard}
	err := client.ResumeSession(context.Background(), "coding-agents", "example")
	if err == nil || !strings.Contains(err.Error(), "cannot resume while publisher Job example-publish is active") {
		t.Fatalf("got error %v", err)
	}
}
