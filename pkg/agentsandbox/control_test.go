package agentsandbox

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/airlock/internal/kubernetes"
	"github.com/yarlson/airlock/internal/publish"
)

func TestNewRejectsMissingStreamsBeforeLoadingKubeconfig(t *testing.T) {
	input := strings.NewReader("")
	cases := []struct {
		name    string
		streams Streams
	}{
		{name: "input", streams: Streams{Output: io.Discard, ErrorOutput: io.Discard}},
		{name: "output", streams: Streams{Input: input, ErrorOutput: io.Discard}},
		{name: "error output", streams: Streams{Input: input, Output: io.Discard}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := New("unavailable-context", test.streams)
			require.EqualError(t, err, "input, output, and error output streams are required")
		})
	}
}

func TestStartRejectsInvalidRequestWithoutChangingCluster(t *testing.T) {
	cluster := startCluster{
		check: func(context.Context, string, string) error {
			require.FailNow(t, "checked the cluster for an invalid request")

			return nil
		},
		apply: func(context.Context, []byte) error {
			require.FailNow(t, "applied an invalid request")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	request := validUpdateStartRequest()
	request.Target.Name = "missing-task"
	request.Prompt = ""
	err := control.Start(context.Background(), request)
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
		apply: func(context.Context, []byte) error {
			require.FailNow(t, "applied a session after detecting a mode conflict")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	request := validUpdateStartRequest()
	err := control.Start(context.Background(), request)
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
		apply: func(_ context.Context, manifest []byte) error {
			operations = append(operations, "apply")
			assert.NotEmpty(t, manifest, "applied an empty session manifest")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.Start(context.Background(), validUpdateStartRequest())
	require.NoError(t, err)
	assert.Equal(t, []string{"check", "apply"}, operations)
}

func TestContinueCreatesResumableJobAgainstPersistentSession(t *testing.T) {
	cluster := continuationCluster{
		definition: func(_ context.Context, namespace, name string) (kubernetes.SessionDefinition, error) {
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", name)

			return kubernetes.SessionDefinition{
				Image:        "coding-agent:test",
				Repository:   "https://github.com/lokalise/kargo.git",
				InitialRef:   "main",
				CloneDepth:   1,
				StorageSize:  "10Gi",
				SetupCommand: "make tools",
			}, nil
		},
		apply: func(_ context.Context, manifest []byte) error {
			assert.Contains(t, string(manifest), `"name": "review-followup"`)
			assert.Contains(t, string(manifest), `"name": "AGENT_RESUME"`)
			assert.NotContains(t, string(manifest), `"kind": "PersistentVolumeClaim"`)

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.Continue(context.Background(), ContinueRequest{
		Target:  Target{Namespace: "coding-agents", Name: "review"},
		TaskID:  "followup",
		Prompt:  "fix the remaining test",
		Timeout: time.Hour,
	})
	require.NoError(t, err)
}

func TestStreamLogsUsesNewestSessionAttempt(t *testing.T) {
	streamed := false
	cluster := logCluster{
		newest: func(_ context.Context, namespace, name string) (string, error) {
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", name)

			return "review-3", nil
		},
		stream: func(_ context.Context, namespace, pod, container string, follow bool, _ io.Writer) error {
			streamed = true
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review-3", pod)
			assert.Equal(t, "agent", container)
			assert.True(t, follow)

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.WriteLogs(context.Background(), LogRequest{
		Target: Target{Namespace: "coding-agents", Name: "review"},
		Follow: true,
	}, io.Discard)
	require.NoError(t, err)
	assert.True(t, streamed, "logs were not streamed")
}

func TestStreamLogsDoesNotOpenStreamWhenNoAttemptExists(t *testing.T) {
	noAttempt := errors.New("session has no Pods")
	cluster := logCluster{
		newest: func(context.Context, string, string) (string, error) {
			return "", noAttempt
		},
		stream: func(context.Context, string, string, string, bool, io.Writer) error {
			require.FailNow(t, "opened a log stream without a session attempt")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.WriteLogs(context.Background(), LogRequest{
		Target: Target{Namespace: "coding-agents", Name: "review"},
	}, io.Discard)
	require.ErrorIs(t, err, noAttempt)
}

func TestPublishRejectsInvalidTargetWithoutStartingPublication(t *testing.T) {
	control := &Control{
		publishSession: func(context.Context, publish.Request) (publish.Result, error) {
			require.FailNow(t, "started publication with an invalid target")

			return publish.Result{}, nil
		},
	}
	_, err := control.Publish(context.Background(), PublishRequest{
		Target:        Target{Namespace: "coding-agents"},
		Branch:        "yar/review",
		CommitMessage: "Review changes",
		Timeout:       time.Minute,
	})
	require.EqualError(t, err, "session name is required")
}

type startCluster struct {
	sessionCluster
	check func(context.Context, string, string) error
	apply func(context.Context, []byte) error
}

func (c startCluster) CheckSessionAvailable(ctx context.Context, namespace, name string) error {
	return c.check(ctx, namespace, name)
}

func (c startCluster) Apply(ctx context.Context, manifest []byte) error {
	return c.apply(ctx, manifest)
}

type logCluster struct {
	sessionCluster
	newest func(context.Context, string, string) (string, error)
	stream func(context.Context, string, string, string, bool, io.Writer) error
}

type continuationCluster struct {
	sessionCluster
	definition func(context.Context, string, string) (kubernetes.SessionDefinition, error)
	apply      func(context.Context, []byte) error
}

func (c continuationCluster) PersistentSessionDefinition(ctx context.Context, namespace, name string) (kubernetes.SessionDefinition, error) {
	return c.definition(ctx, namespace, name)
}

func (c continuationCluster) Apply(ctx context.Context, manifest []byte) error {
	return c.apply(ctx, manifest)
}

func (c logCluster) NewestPodName(ctx context.Context, namespace, name string) (string, error) {
	return c.newest(ctx, namespace, name)
}

func (c logCluster) StreamPodLogs(ctx context.Context, namespace, pod, container string, follow bool, output io.Writer) error {
	return c.stream(ctx, namespace, pod, container, follow, output)
}

func validUpdateStartRequest() StartRequest {
	return StartRequest{
		Target:      Target{Namespace: "coding-agents", Name: "review"},
		Image:       "coding-agent:test",
		Storage:     StoragePersistent,
		InitialRef:  "main",
		Prompt:      "review the failed checks",
		CloneDepth:  1,
		StorageSize: "10Gi",
		Timeout:     time.Hour,
	}
}
