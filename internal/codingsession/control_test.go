package codingsession

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

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
			if err == nil || err.Error() != "input, output, and error output streams are required" {
				t.Fatalf("got error %v", err)
			}
		})
	}
}

func TestStartRejectsInvalidRequestWithoutChangingCluster(t *testing.T) {
	cluster := startCluster{
		check: func(context.Context, string, string, sessionmanifest.Mode) error {
			t.Fatal("checked the cluster for an invalid request")

			return nil
		},
		apply: func(context.Context, []byte) error {
			t.Fatal("applied an invalid request")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	request := validUpdateStartRequest()
	request.Target.Name = "missing-task"
	request.Prompt = ""
	err := control.Start(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "prompt is required for update sessions") {
		t.Fatalf("got error %v", err)
	}
}

func TestStartPreservesExistingSessionWhenModeConflicts(t *testing.T) {
	conflict := errors.New("bounded session already owns this name")
	cluster := startCluster{
		check: func(_ context.Context, namespace, name string, mode sessionmanifest.Mode) error {
			if namespace != "coding-agents" || name != "review" || mode != sessionmanifest.ModeLong {
				t.Fatalf("checked %s/%s in mode %q", namespace, name, mode)
			}

			return conflict
		},
		apply: func(context.Context, []byte) error {
			t.Fatal("applied a session after detecting a mode conflict")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	request := validUpdateStartRequest()
	request.Mode = ModeLong
	request.Prompt = ""
	err := control.Start(context.Background(), request)
	if !errors.Is(err, conflict) {
		t.Fatalf("got error %v, want mode conflict", err)
	}
}

func TestStartChecksOwnershipBeforeApplyingSession(t *testing.T) {
	var operations []string
	cluster := startCluster{
		check: func(_ context.Context, namespace, name string, mode sessionmanifest.Mode) error {
			operations = append(operations, "check")
			if namespace != "coding-agents" || name != "review" || mode != sessionmanifest.ModeUpdate {
				t.Fatalf("checked %s/%s in mode %q", namespace, name, mode)
			}

			return nil
		},
		apply: func(_ context.Context, manifest []byte) error {
			operations = append(operations, "apply")
			if len(manifest) == 0 {
				t.Fatal("applied an empty session manifest")
			}

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.Start(context.Background(), validUpdateStartRequest())
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(operations, []string{"check", "apply"}) {
		t.Fatalf("got operations %q, want ownership check before apply", operations)
	}
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
					if namespace != "coding-agents" || name != "review" || timeout <= 0 {
						t.Fatalf("waited for %s/%s with timeout %s", namespace, name, timeout)
					}

					return "review-2", nil
				},
				exec: func(_ context.Context, namespace, pod, container string, command []string, interactive bool) error {
					executed = true
					if namespace != "coding-agents" || pod != "review-2" || container != "agent" || interactive || !slices.Equal(command, test.wantCommand) {
						t.Fatalf("executed namespace=%q pod=%q container=%q command=%q interactive=%t", namespace, pod, container, command, interactive)
					}

					return nil
				},
			}
			control := &Control{cluster: cluster}
			err := control.RunTask(context.Background(), TaskRequest{
				Target:     Target{Namespace: "coding-agents", Name: "review"},
				Prompt:     "review the failed checks",
				ResumeLast: test.resumeLast,
			})
			if err != nil {
				t.Fatal(err)
			}

			if !executed {
				t.Fatal("task was not executed")
			}
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
			t.Fatal("executed a task without a ready Pod")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.RunTask(context.Background(), TaskRequest{
		Target: Target{Namespace: "coding-agents", Name: "review"},
		Prompt: "review the failed checks",
	})
	if !errors.Is(err, notReady) {
		t.Fatalf("got error %v, want readiness error", err)
	}
}

func TestStreamLogsUsesNewestSessionAttempt(t *testing.T) {
	streamed := false
	cluster := logCluster{
		newest: func(_ context.Context, namespace, name string) (string, error) {
			if namespace != "coding-agents" || name != "review" {
				t.Fatalf("selected newest Pod for %s/%s", namespace, name)
			}

			return "review-3", nil
		},
		stream: func(_ context.Context, namespace, pod, container string, follow bool) error {
			streamed = true
			if namespace != "coding-agents" || pod != "review-3" || container != "agent" || !follow {
				t.Fatalf("streamed namespace=%q pod=%q container=%q follow=%t", namespace, pod, container, follow)
			}

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.StreamLogs(context.Background(), LogRequest{
		Target: Target{Namespace: "coding-agents", Name: "review"},
		Follow: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !streamed {
		t.Fatal("logs were not streamed")
	}
}

func TestStreamLogsDoesNotOpenStreamWhenNoAttemptExists(t *testing.T) {
	noAttempt := errors.New("session has no Pods")
	cluster := logCluster{
		newest: func(context.Context, string, string) (string, error) {
			return "", noAttempt
		},
		stream: func(context.Context, string, string, string, bool) error {
			t.Fatal("opened a log stream without a session attempt")

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.StreamLogs(context.Background(), LogRequest{
		Target: Target{Namespace: "coding-agents", Name: "review"},
	})
	if !errors.Is(err, noAttempt) {
		t.Fatalf("got error %v, want missing-attempt error", err)
	}
}

func TestOpenShellUsesInteractiveBashInReadySession(t *testing.T) {
	executed := false
	cluster := taskCluster{
		wait: func(context.Context, string, string, time.Duration) (string, error) {
			return "review-4", nil
		},
		exec: func(_ context.Context, namespace, pod, container string, command []string, interactive bool) error {
			executed = true
			if namespace != "coding-agents" || pod != "review-4" || container != "agent" || !interactive || !slices.Equal(command, []string{"bash"}) {
				t.Fatalf("executed namespace=%q pod=%q container=%q command=%q interactive=%t", namespace, pod, container, command, interactive)
			}

			return nil
		},
	}
	control := &Control{cluster: cluster}
	err := control.OpenShell(context.Background(), Target{Namespace: "coding-agents", Name: "review"})
	if err != nil {
		t.Fatal(err)
	}

	if !executed {
		t.Fatal("interactive shell was not opened")
	}
}

func TestPublishRejectsInvalidTargetWithoutStartingPublication(t *testing.T) {
	control := &Control{
		publishSession: func(context.Context, publish.Request) (publish.Result, error) {
			t.Fatal("started publication with an invalid target")

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
	if err == nil || err.Error() != "session name is required" {
		t.Fatalf("got error %v, want missing session name", err)
	}
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
