package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/airlock/internal/kubernetes"
	"github.com/yarlson/airlock/internal/publish"
)

func TestConnectRejectsMissingOutputBeforeLoadingKubeconfig(t *testing.T) {
	_, err := Connect(context.Background(), Config{
		Connection:  Connection{ContextName: "unavailable-context"},
		Namespace:   "coding-agents",
		Image:       "coding-agent:test",
		TaskTimeout: time.Hour,
	}, nil, nil, nil)
	require.EqualError(t, err, "output stream is required")
}

func TestStartRejectsInvalidRequestWithoutChangingCluster(t *testing.T) {
	cluster := startCluster{
		check: func(context.Context, string, string) error {
			require.FailNow(t, "checked the cluster for an invalid request")

			return nil
		},
		create: func(context.Context, kubernetes.SessionRequest) error {
			require.FailNow(t, "applied an invalid request")

			return nil
		},
	}
	control := &sessionControl{cluster: cluster}
	request := validUpdateStartRequest()
	request.target.name = "missing-task"
	request.prompt = ""
	err := control.start(context.Background(), request)
	require.ErrorContains(t, err, "prompt is required")
}

func TestStartPreservesExistingSession(t *testing.T) {
	conflict := errors.New("session already owns this name")
	cluster := startCluster{
		check: func(_ context.Context, namespace, name string) error {
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", name)

			return conflict
		},
		create: func(context.Context, kubernetes.SessionRequest) error {
			require.FailNow(t, "applied a session after detecting a mode conflict")

			return nil
		},
	}
	control := &sessionControl{cluster: cluster}
	request := validUpdateStartRequest()
	err := control.start(context.Background(), request)
	require.ErrorIs(t, err, conflict)
}

func TestStartChecksOwnershipBeforeApplyingSession(t *testing.T) {
	var operations []string
	cluster := startCluster{
		check: func(_ context.Context, namespace, name string) error {
			operations = append(operations, "check")
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", name)

			return nil
		},
		create: func(_ context.Context, request kubernetes.SessionRequest) error {
			operations = append(operations, "apply")
			assert.Equal(t, "review", request.Name)
			assert.Equal(t, "coding-agents", request.Namespace)

			return nil
		},
	}
	control := &sessionControl{cluster: cluster}
	err := control.start(context.Background(), validUpdateStartRequest())
	require.NoError(t, err)
	assert.Equal(t, []string{"check", "apply"}, operations)
}

func TestContinueCreatesResumableJobAgainstPersistentSession(t *testing.T) {
	cluster := continuationCluster{
		continueTask: func(_ context.Context, request kubernetes.ContinuationRequest) error {
			assert.Equal(t, kubernetes.ContinuationRequest{
				Name: "review", TaskName: "review-followup", Namespace: "coding-agents",
				Prompt: "fix the remaining test", TimeoutSeconds: 3600,
			}, request)

			return nil
		},
	}
	control := &sessionControl{cluster: cluster}
	err := control.continueTask(context.Background(), sessionContinueRequest{
		target:  sessionTarget{namespace: "coding-agents", name: "review"},
		taskID:  "followup",
		prompt:  "fix the remaining test",
		timeout: time.Hour,
	})
	require.NoError(t, err)
}

func TestStreamLogsUsesNewestSessionAttempt(t *testing.T) {
	streamed := false
	cluster := logCluster{
		write: func(_ context.Context, namespace, name string, follow bool, _ io.Writer) error {
			streamed = true
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", name)
			assert.True(t, follow)

			return nil
		},
	}
	control := &sessionControl{cluster: cluster}
	err := control.writeLogs(context.Background(), sessionLogRequest{
		target: sessionTarget{namespace: "coding-agents", name: "review"},
		follow: true,
	}, io.Discard)
	require.NoError(t, err)
	assert.True(t, streamed, "logs were not streamed")
}

func TestStreamLogsDoesNotOpenStreamWhenNoAttemptExists(t *testing.T) {
	noAttempt := errors.New("session has no Pods")
	cluster := logCluster{
		write: func(context.Context, string, string, bool, io.Writer) error {
			return noAttempt
		},
	}
	control := &sessionControl{cluster: cluster}
	err := control.writeLogs(context.Background(), sessionLogRequest{
		target: sessionTarget{namespace: "coding-agents", name: "review"},
	}, io.Discard)
	require.ErrorIs(t, err, noAttempt)
}

func TestPublishRejectsInvalidTargetWithoutStartingPublication(t *testing.T) {
	control := &sessionControl{
		publishSession: func(context.Context, publish.Request) (publish.Result, error) {
			require.FailNow(t, "started publication with an invalid target")

			return publish.Result{}, nil
		},
	}
	_, err := control.publish(context.Background(), sessionPublishRequest{
		target:        sessionTarget{namespace: "coding-agents"},
		branch:        "yar/review",
		commitMessage: "Review changes",
		timeout:       time.Minute,
	})
	require.EqualError(t, err, "session name is required")
}

type startCluster struct {
	sessionCluster
	check  func(context.Context, string, string) error
	create func(context.Context, kubernetes.SessionRequest) error
}

func (c startCluster) CheckSessionAvailable(ctx context.Context, namespace, name string) error {
	return c.check(ctx, namespace, name)
}

func (c startCluster) CreateSession(ctx context.Context, request kubernetes.SessionRequest) error {
	return c.create(ctx, request)
}

type logCluster struct {
	sessionCluster
	write func(context.Context, string, string, bool, io.Writer) error
}

type continuationCluster struct {
	sessionCluster
	continueTask func(context.Context, kubernetes.ContinuationRequest) error
}

func (c continuationCluster) ContinueSession(ctx context.Context, request kubernetes.ContinuationRequest) error {
	return c.continueTask(ctx, request)
}

func (c logCluster) WriteSessionLogs(ctx context.Context, namespace, name string, follow bool, output io.Writer) error {
	return c.write(ctx, namespace, name, follow, output)
}

func validUpdateStartRequest() sessionStartRequest {
	return sessionStartRequest{
		target:      sessionTarget{namespace: "coding-agents", name: "review"},
		image:       "coding-agent:test",
		storage:     StoragePersistent,
		initialRef:  "main",
		prompt:      "review the failed checks",
		cloneDepth:  1,
		storageSize: "10Gi",
		timeout:     time.Hour,
	}
}
