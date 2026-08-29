package codingsession

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"coding-agent-k8s/internal/publish"
	"coding-agent-k8s/internal/sessionmanifest"
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
		check: func(context.Context, string, string, sessionmanifest.Mode) error {
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
	require.ErrorContains(t, err, "prompt is required for update sessions")
}

func TestStartPreservesExistingSessionWhenModeConflicts(t *testing.T) {
	conflict := errors.New("bounded session already owns this name")
	cluster := startCluster{
		check: func(_ context.Context, namespace, name string, mode sessionmanifest.Mode) error {
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", name)
			assert.Equal(t, sessionmanifest.ModeLong, mode)

			return conflict
		},
		apply: func(context.Context, []byte) error {
			require.FailNow(t, "applied a session after detecting a mode conflict")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	request := validUpdateStartRequest()
	request.Mode = ModeLong
	request.Prompt = ""
	err := control.Start(context.Background(), request)
	require.ErrorIs(t, err, conflict)
}

func TestStartChecksOwnershipBeforeApplyingSession(t *testing.T) {
	var operations []string
	cluster := startCluster{
		check: func(_ context.Context, namespace, name string, mode sessionmanifest.Mode) error {
			operations = append(operations, "check")
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", name)
			assert.Equal(t, sessionmanifest.ModeUpdate, mode)

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

func TestRunTaskSelectsNewOrLatestThread(t *testing.T) {
	cases := []struct {
		name        string
		resumeLast  bool
		wantCommand []string
	}{
		{
			name:        "new thread",
			wantCommand: []string{"/usr/local/bin/agent-entrypoint", "task", "review the failed checks"},
		},
		{
			name:        "latest thread",
			resumeLast:  true,
			wantCommand: []string{"/usr/local/bin/agent-entrypoint", "task", "--resume-last", "review the failed checks"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			executed := false
			cluster := taskCluster{
				wait: func(_ context.Context, namespace, name string, timeout time.Duration) (string, error) {
					assert.Equal(t, "coding-agents", namespace)
					assert.Equal(t, "review", name)
					assert.Positive(t, timeout)

					return "review-2", nil
				},
				exec: func(_ context.Context, namespace, pod, container string, command []string, interactive bool) error {
					executed = true
					assert.Equal(t, "coding-agents", namespace)
					assert.Equal(t, "review-2", pod)
					assert.Equal(t, "agent", container)
					assert.Equal(t, test.wantCommand, command)
					assert.False(t, interactive)

					return nil
				},
			}
			control := &Control{cluster: cluster}
			err := control.RunTask(context.Background(), TaskRequest{
				Target:     Target{Namespace: "coding-agents", Name: "review"},
				Prompt:     "review the failed checks",
				ResumeLast: test.resumeLast,
			})
			require.NoError(t, err)
			assert.True(t, executed, "task was not executed")
		})
	}
}

func TestRunTaskDoesNotExecWhenSessionIsNotReady(t *testing.T) {
	notReady := errors.New("session did not become ready")
	cluster := taskCluster{
		wait: func(context.Context, string, string, time.Duration) (string, error) {
			return "", notReady
		},
		exec: func(context.Context, string, string, string, []string, bool) error {
			require.FailNow(t, "executed a task without a ready Pod")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.RunTask(context.Background(), TaskRequest{
		Target: Target{Namespace: "coding-agents", Name: "review"},
		Prompt: "review the failed checks",
	})
	require.ErrorIs(t, err, notReady)
}

func TestStreamLogsUsesNewestSessionAttempt(t *testing.T) {
	streamed := false
	cluster := logCluster{
		newest: func(_ context.Context, namespace, name string) (string, error) {
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review", name)

			return "review-3", nil
		},
		stream: func(_ context.Context, namespace, pod, container string, follow bool) error {
			streamed = true
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review-3", pod)
			assert.Equal(t, "agent", container)
			assert.True(t, follow)

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.StreamLogs(context.Background(), LogRequest{
		Target: Target{Namespace: "coding-agents", Name: "review"},
		Follow: true,
	})
	require.NoError(t, err)
	assert.True(t, streamed, "logs were not streamed")
}

func TestStreamLogsDoesNotOpenStreamWhenNoAttemptExists(t *testing.T) {
	noAttempt := errors.New("session has no Pods")
	cluster := logCluster{
		newest: func(context.Context, string, string) (string, error) {
			return "", noAttempt
		},
		stream: func(context.Context, string, string, string, bool) error {
			require.FailNow(t, "opened a log stream without a session attempt")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.StreamLogs(context.Background(), LogRequest{
		Target: Target{Namespace: "coding-agents", Name: "review"},
	})
	require.ErrorIs(t, err, noAttempt)
}

func TestOpenShellUsesInteractiveBashInReadySession(t *testing.T) {
	executed := false
	cluster := taskCluster{
		wait: func(context.Context, string, string, time.Duration) (string, error) {
			return "review-4", nil
		},
		exec: func(_ context.Context, namespace, pod, container string, command []string, interactive bool) error {
			executed = true
			assert.Equal(t, "coding-agents", namespace)
			assert.Equal(t, "review-4", pod)
			assert.Equal(t, "agent", container)
			assert.Equal(t, []string{"bash"}, command)
			assert.True(t, interactive)

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.OpenShell(context.Background(), Target{Namespace: "coding-agents", Name: "review"})
	require.NoError(t, err)
	assert.True(t, executed, "interactive shell was not opened")
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
		Title:         "Review changes",
		Timeout:       time.Minute,
	})
	require.EqualError(t, err, "session name is required")
}

type startCluster struct {
	sessionCluster
	check func(context.Context, string, string, sessionmanifest.Mode) error
	apply func(context.Context, []byte) error
}

func (c startCluster) CheckSessionModeAvailable(ctx context.Context, namespace, name string, mode sessionmanifest.Mode) error {
	return c.check(ctx, namespace, name, mode)
}

func (c startCluster) Apply(ctx context.Context, manifest []byte) error {
	return c.apply(ctx, manifest)
}

type taskCluster struct {
	sessionCluster
	wait func(context.Context, string, string, time.Duration) (string, error)
	exec func(context.Context, string, string, string, []string, bool) error
}

func (c taskCluster) WaitForReadyPod(ctx context.Context, namespace, name string, timeout time.Duration) (string, error) {
	return c.wait(ctx, namespace, name, timeout)
}

func (c taskCluster) ExecPod(ctx context.Context, namespace, pod, container string, command []string, interactive bool) error {
	return c.exec(ctx, namespace, pod, container, command, interactive)
}

type logCluster struct {
	sessionCluster
	newest func(context.Context, string, string) (string, error)
	stream func(context.Context, string, string, string, bool) error
}

func (c logCluster) NewestPodName(ctx context.Context, namespace, name string) (string, error) {
	return c.newest(ctx, namespace, name)
}

func (c logCluster) StreamPodLogs(ctx context.Context, namespace, pod, container string, follow bool) error {
	return c.stream(ctx, namespace, pod, container, follow)
}

func validUpdateStartRequest() StartRequest {
	return StartRequest{
		Target:      Target{Namespace: "coding-agents", Name: "review"},
		Image:       "coding-agent:test",
		Mode:        ModeUpdate,
		InitialRef:  "main",
		Prompt:      "review the failed checks",
		CloneDepth:  1,
		StorageSize: "10Gi",
		Timeout:     time.Hour,
	}
}
