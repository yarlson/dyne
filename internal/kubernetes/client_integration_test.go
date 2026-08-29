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

func TestClientAppliesScalesAndRemovesSessionResources(t *testing.T) {
	contextName := os.Getenv("KUBERNETES_INTEGRATION_CONTEXT")
	if contextName == "" {
		t.Skip("KUBERNETES_INTEGRATION_CONTEXT is required")
	}

	client, err := New(contextName, nil, io.Discard, io.Discard)
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

	set, err := client.typed.AppsV1().StatefulSets(namespace).Get(testContext, "session", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, set.Spec.Replicas)
	assert.Zero(t, *set.Spec.Replicas)

	require.NoError(t, client.ResumeSession(testContext, namespace, "session"))
	require.Eventually(t, func() bool {
		set, err := client.typed.AppsV1().StatefulSets(namespace).Get(testContext, "session", metav1.GetOptions{})

		return err == nil && set.Spec.Replicas != nil && *set.Spec.Replicas == 1
	}, 30*time.Second, 100*time.Millisecond, "session was not resumed")

	require.NoError(t, client.StopSession(testContext, namespace, "session"))
	require.Eventually(t, func() bool {
		set, err := client.typed.AppsV1().StatefulSets(namespace).Get(testContext, "session", metav1.GetOptions{})

		return err == nil && set.Spec.Replicas != nil && *set.Spec.Replicas == 0
	}, 30*time.Second, 100*time.Millisecond, "session was not stopped")

	require.NoError(t, client.DeleteSession(testContext, namespace, "session"))
	assertSessionWorkloadDeleted(t, testContext, client, namespace)
	for _, name := range []string{"workspace-session", "home-session", "codex-session"} {
		_, err := client.typed.CoreV1().PersistentVolumeClaims(namespace).Get(testContext, name, metav1.GetOptions{})
		assert.NoError(t, err, "persistent state %s was not retained", name)
	}

	require.NoError(t, client.DestroySession(testContext, namespace, "session"))
	for _, name := range []string{"workspace-session", "home-session", "codex-session"} {
		require.Eventually(t, func() bool {
			_, err := client.typed.CoreV1().PersistentVolumeClaims(namespace).Get(testContext, name, metav1.GetOptions{})

			return apierrors.IsNotFound(err)
		}, 30*time.Second, 100*time.Millisecond, "persistent state %s was not destroyed", name)
	}
}

func integrationNamespace(t *testing.T) string {
	t.Helper()
	var suffix [4]byte
	_, err := rand.Read(suffix[:])
	require.NoError(t, err)

	return fmt.Sprintf("agentctl-integration-%x", suffix)
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
      "kind": "Service",
      "metadata": {"name": "session", "namespace": %[1]q},
      "spec": {"clusterIP": "None", "selector": {"app": "session"}}
    },
    {
      "apiVersion": "v1",
      "kind": "PersistentVolumeClaim",
      "metadata": {"name": "workspace-session", "namespace": %[1]q},
      "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}}
    },
    {
      "apiVersion": "v1",
      "kind": "PersistentVolumeClaim",
      "metadata": {"name": "home-session", "namespace": %[1]q},
      "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}}
    },
    {
      "apiVersion": "v1",
      "kind": "PersistentVolumeClaim",
      "metadata": {"name": "codex-session", "namespace": %[1]q},
      "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}}
    },
    {
      "apiVersion": "apps/v1",
      "kind": "StatefulSet",
      "metadata": {"name": "session", "namespace": %[1]q},
      "spec": {
        "serviceName": "session",
        "replicas": 0,
        "selector": {"matchLabels": {"app": "session"}},
        "template": {
          "metadata": {"labels": {"app": "session"}},
          "spec": {
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
		_, err := client.typed.AppsV1().StatefulSets(namespace).Get(ctx, "session", metav1.GetOptions{})

		return apierrors.IsNotFound(err)
	}, 30*time.Second, 100*time.Millisecond, "StatefulSet was not deleted")

	_, err := client.typed.CoreV1().Services(namespace).Get(ctx, "session", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "got Service lookup error %v, want deleted Service", err)
}
