//go:build integration

package kubernetes

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClientAppliesAndRemovesSessionResources(t *testing.T) {
	contextName := os.Getenv("KUBERNETES_INTEGRATION_CONTEXT")
	if contextName == "" {
		t.Skip("KUBERNETES_INTEGRATION_CONTEXT is required")
	}

	client, err := New(contextName, io.Discard)
	require.NoError(t, err)
	namespace := integrationNamespace(t)
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := client.typed.CoreV1().Namespaces().Delete(cleanupContext, namespace, metav1.DeleteOptions{})
		assert.True(t, err == nil || apierrors.IsNotFound(err), "delete integration namespace: %v", err)
		require.Eventually(t, func() bool {
			_, err := client.typed.CoreV1().Namespaces().Get(cleanupContext, namespace, metav1.GetOptions{})

			return apierrors.IsNotFound(err)
		}, 30*time.Second, 100*time.Millisecond, "integration namespace was not deleted")
	})

	testContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, client.Apply(testContext, integrationManifest(namespace)))

	require.NoError(t, client.DeleteSession(testContext, namespace, "session"))
	assertSessionWorkloadDeleted(t, testContext, client, namespace)
	_, err = client.typed.CoreV1().PersistentVolumeClaims(namespace).Get(testContext, "session-session", metav1.GetOptions{})
	assert.NoError(t, err, "persistent state was not retained")

	require.NoError(t, client.DestroySession(testContext, namespace, "session"))
	require.Eventually(t, func() bool {
		_, err := client.typed.CoreV1().PersistentVolumeClaims(namespace).Get(testContext, "session-session", metav1.GetOptions{})

		return apierrors.IsNotFound(err)
	}, 30*time.Second, 100*time.Millisecond, "persistent state was not destroyed")
}

func integrationNamespace(t *testing.T) string {
	t.Helper()
	var suffix [4]byte
	_, err := rand.Read(suffix[:])
	require.NoError(t, err)

	return fmt.Sprintf("airlock-integration-%x", suffix)
}

func integrationManifest(namespace string) []byte {
	return fmt.Appendf(nil, `{
  "apiVersion": "v1",
  "kind": "List",
  "items": [
    {
      "apiVersion": "v1",
      "kind": "Namespace",
      "metadata": {"name": %[1]q}
    },
    {
      "apiVersion": "v1",
      "kind": "PersistentVolumeClaim",
      "metadata": {"name": "session-session", "namespace": %[1]q, "labels": {"coding-agent/session": "session"}},
      "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}}
    },
    {
      "apiVersion": "batch/v1",
      "kind": "Job",
      "metadata": {"name": "session-task", "namespace": %[1]q, "labels": {"coding-agent/session": "session"}},
      "spec": {
        "template": {
          "metadata": {"labels": {"coding-agent/session": "session"}},
          "spec": {
            "restartPolicy": "Never",
            "containers": [{"name": "agent", "image": "registry.k8s.io/pause:3.10"}]
          }
        }
      }
    }
  ]
}`, namespace)
}

func assertSessionWorkloadDeleted(t *testing.T, ctx context.Context, client *Client, namespace string) {
	t.Helper()
	require.Eventually(t, func() bool {
		jobs, err := client.typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: "coding-agent/session=session"})

		return err == nil && len(jobs.Items) == 0
	}, 30*time.Second, 100*time.Millisecond, "Jobs were not deleted")
}
