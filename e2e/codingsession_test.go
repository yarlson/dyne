//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	codingsession "github.com/yarlson/airlock/pkg/agentsandbox"
)

const (
	integrationContextEnvironment = "KUBERNETES_INTEGRATION_CONTEXT"
	e2eImageEnvironment           = "E2E_IMAGE"
	sessionTimeout                = 8 * time.Minute
	waitInterval                  = time.Second
)

const gitFixtureCommand = `set -euo pipefail
mkdir -p /tmp/seed /srv/project.git
git init --initial-branch=main /tmp/seed
printf 'fixture repository\n' > /tmp/seed/README.md
printf '{"name":"coding-session-e2e","version":"1.0.0","private":true}\n' > /tmp/seed/package.json
git -C /tmp/seed add README.md package.json
git -C /tmp/seed -c user.name='E2E Fixture' -c user.email='fixture@example.invalid' commit -m 'Create fixture'
git clone --bare /tmp/seed /srv/project.git
touch /srv/project.git/git-daemon-export-ok
exec git daemon --reuseaddr --base-path=/srv --export-all --verbose --listen=0.0.0.0 --port=9418 /srv`

const setupCommand = `set -euo pipefail
mise install
mise --version > "${HOME}/mise-version"
npm install --ignore-scripts --no-audit --no-fund
mkdir -p "${HOME}/.local/bin"
cat > "${HOME}/.local/bin/codex" <<'CODEX'
#!/usr/bin/env bash
set -euo pipefail
prompt="${!#}"
prompt="${prompt%%$'\n'*}"
verify_agent_configuration() {
  [[ " $* " == *' -c developer_instructions="Follow the E2E agent contract." '* ]]
  test "$(cat /home/agent/.agents/skills/e2e-contract/SKILL.md)" = "$(printf '%s' '---
name: e2e-contract
description: Verify the E2E agent contract.
---

Use the configured E2E behavior.')"
}
case "${prompt}" in
  explore-repository)
    test "$(cat /workspace/README.md)" = "fixture repository"
    test -s "${HOME}/mise-version"
    test -f /workspace/package-lock.json
    if touch /workspace/exploration-must-not-write 2>/dev/null; then
      exit 20
    fi
    printf 'exploration completed with a read-only workspace\n'
    ;;
  apply-update)
	verify_agent_configuration "$@"
    test "$(cat /workspace/README.md)" = "fixture repository"
    test -s "${HOME}/mise-version"
    test -f /workspace/package-lock.json
    printf 'workspace state retained\n' > /workspace/update-marker
    printf 'tool state retained\n' > "${HOME}/tool-marker"
    printf 'Codex state retained\n' > "${CODEX_HOME}/update-marker"
    printf 'temporary update state\n' > /tmp/update-marker
    ;;
  verify-update-state)
	verify_agent_configuration "$@"
    test "$(cat /workspace/update-marker)" = "workspace state retained"
    test "$(cat "${HOME}/tool-marker")" = "tool state retained"
    test "$(cat "${CODEX_HOME}/update-marker")" = "Codex state retained"
    test ! -e /tmp/update-marker
    printf 'long workspace state\n' > /workspace/long-marker
    printf 'long Codex state\n' > "${CODEX_HOME}/long-marker"
    printf 'temporary long state\n' > /tmp/long-marker
    ;;
  verify-stop-resume)
	verify_agent_configuration "$@"
    test "$(cat /workspace/long-marker)" = "long workspace state"
    test "$(cat "${CODEX_HOME}/long-marker")" = "long Codex state"
    test ! -e /tmp/long-marker
    [[ " $* " == *" resume --last "* ]]
    printf 'resume state retained\n' > "${HOME}/resume-marker"
    printf 'temporary resumed state\n' > /tmp/resume-marker
    ;;
  verify-delete-recreate)
	verify_agent_configuration "$@"
    test "$(cat /workspace/long-marker)" = "long workspace state"
    test "$(cat "${HOME}/resume-marker")" = "resume state retained"
    test "$(cat "${CODEX_HOME}/long-marker")" = "long Codex state"
    test ! -e /tmp/resume-marker
    ;;
  *)
    printf 'unsupported E2E prompt: %s\n' "${prompt}" >&2
    exit 64
    ;;
esac
mkdir -p /artifacts
printf '{"status":"completed","summary":"E2E task completed","blocker":""}\n' > /artifacts/outcome.json
printf '{"title":"E2E change","body":"E2E verification"}\n' > /artifacts/pull-request.json
CODEX
chmod 0755 "${HOME}/.local/bin/codex"`

func TestExplorationClonesAndPreparesAReadOnlyTemporaryWorkspace(t *testing.T) {
	environment := newTestEnvironment(t)
	target := codingsession.Target{Namespace: environment.sessionNamespace, Name: "exploration"}

	require.NoError(t, environment.control.Start(environment.context, startRequest(environment, target, codingsession.StorageEphemeral, "explore-repository")))
	environment.requireJobSucceeded(t, target.Name)

	status, err := environment.control.Status(environment.context, target)
	require.NoError(t, err)
	assert.Empty(t, resourceNames(status, "PersistentVolumeClaim"))

	require.NoError(t, environment.control.WriteLogs(environment.context, codingsession.LogRequest{Target: target}, environment.output))
	assert.Contains(t, environment.output.String(), "exploration completed with a read-only workspace")

	require.NoError(t, environment.control.Destroy(environment.context, target))
	environment.requireSessionAbsent(t, target)
}

func TestPersistentSessionRetainsStateAndReplacesTemporaryStorage(t *testing.T) {
	environment := newTestEnvironment(t)
	target := codingsession.Target{Namespace: environment.sessionNamespace, Name: "persistent"}

	request := startRequest(environment, target, codingsession.StoragePersistent, "apply-update")
	request.AgentName = "e2e-agent"
	request.Instructions = "Follow the E2E agent contract."
	request.Skills = []codingsession.AgentSkill{{
		Name: "e2e-contract",
		Contents: `---
name: e2e-contract
description: Verify the E2E agent contract.
---

Use the configured E2E behavior.`,
	}}
	require.NoError(t, environment.control.Start(environment.context, request))
	environment.requireJobSucceeded(t, target.Name)
	environment.requirePersistentClaims(t, target)

	require.NoError(t, environment.control.Continue(environment.context, codingsession.ContinueRequest{Target: target, TaskID: "verify-update", Prompt: "verify-update-state", Timeout: 5 * time.Minute}))
	environment.requireJobSucceeded(t, "persistent-verify-update")
	require.NoError(t, environment.control.Continue(environment.context, codingsession.ContinueRequest{Target: target, TaskID: "verify-resume", Prompt: "verify-stop-resume", Timeout: 5 * time.Minute}))
	environment.requireJobSucceeded(t, "persistent-verify-resume")

	require.NoError(t, environment.control.Delete(environment.context, target))
	environment.requireOnlyPersistentClaims(t, target)
	_, err := environment.client.CoreV1().ConfigMaps(environment.sessionNamespace).Get(
		environment.context, "session-persistent-agent", metav1.GetOptions{},
	)
	require.NoError(t, err)
	require.NoError(t, environment.control.Continue(environment.context, codingsession.ContinueRequest{Target: target, TaskID: "verify-delete", Prompt: "verify-delete-recreate", Timeout: 5 * time.Minute}))
	environment.requireJobSucceeded(t, "persistent-verify-delete")

	require.NoError(t, environment.control.Destroy(environment.context, target))
	_, err = environment.client.CoreV1().ConfigMaps(environment.sessionNamespace).Get(
		environment.context, "session-persistent-agent", metav1.GetOptions{},
	)
	assert.True(t, apierrors.IsNotFound(err), "agent configuration was not destroyed")
	environment.requireSessionAbsent(t, target)
}

type testEnvironment struct {
	context          context.Context
	control          *codingsession.Control
	client           clientset.Interface
	output           *bytes.Buffer
	image            string
	repository       string
	sessionNamespace string
}

func newTestEnvironment(t *testing.T) testEnvironment {
	t.Helper()
	contextName := os.Getenv(integrationContextEnvironment)
	if contextName == "" {
		t.Skip(integrationContextEnvironment + " is required")
	}

	image := os.Getenv(e2eImageEnvironment)
	if image == "" {
		t.Skip(e2eImageEnvironment + " is required")
	}

	testContext, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	t.Cleanup(cancel)
	client := newKubernetesClient(t, contextName)
	suffix := randomSuffix(t)
	sessionNamespace := "airlock-e2e-" + suffix
	fixtureNamespace := "airlock-git-" + suffix
	registerNamespaceCleanup(t, client, sessionNamespace, fixtureNamespace)
	_, err := client.CoreV1().Namespaces().Create(testContext, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: sessionNamespace,
		Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": "restricted",
			"pod-security.kubernetes.io/audit":   "restricted",
			"pod-security.kubernetes.io/warn":    "restricted",
		},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	repository := createGitFixture(t, testContext, client, fixtureNamespace, image)

	output := &bytes.Buffer{}
	control, err := codingsession.New(contextName, codingsession.Streams{
		Input:       strings.NewReader(""),
		Output:      output,
		ErrorOutput: output,
	})
	require.NoError(t, err)
	return testEnvironment{
		context:          testContext,
		control:          control,
		client:           client,
		output:           output,
		image:            image,
		repository:       repository,
		sessionNamespace: sessionNamespace,
	}
}

func newKubernetesClient(t *testing.T, contextName string) clientset.Interface {
	t.Helper()
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	require.NoError(t, err)

	client, err := clientset.NewForConfig(config)
	require.NoError(t, err)

	return client
}

func createGitFixture(t *testing.T, ctx context.Context, client clientset.Interface, namespace, image string) string {
	t.Helper()
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	require.NoError(t, err)

	runAsNonRoot := true
	runAsUser := int64(1000)
	automountServiceAccountToken := false
	allowPrivilegeEscalation := false
	readOnlyRootFilesystem := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: namespace, Labels: map[string]string{"app": "git-fixture"}},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &automountServiceAccountToken,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				RunAsUser:    &runAsUser,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{
				{
					Name:    "git",
					Image:   image,
					Command: []string{"bash", "-lc"},
					Args:    []string{gitFixtureCommand},
					Ports:   []corev1.ContainerPort{{Name: "git", ContainerPort: 9418}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("git")}},
						PeriodSeconds: 1,
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivilegeEscalation,
						ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "repository", MountPath: "/srv"},
						{Name: "tmp", MountPath: "/tmp"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "repository", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
	_, err = client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "git-fixture"},
			Ports:    []corev1.ServicePort{{Name: "git", Port: 9418, TargetPort: intstr.FromString("git")}},
		},
	}
	_, err = client.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, wait.PollUntilContextTimeout(ctx, waitInterval, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		current, err := client.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		return podReady(current), nil
	}))

	return fmt.Sprintf("git://git.%s.svc.cluster.local/project.git", namespace)
}

func startRequest(environment testEnvironment, target codingsession.Target, storage codingsession.Storage, prompt string) codingsession.StartRequest {
	return codingsession.StartRequest{
		Target:       target,
		Image:        environment.image,
		Storage:      storage,
		Repository:   environment.repository,
		InitialRef:   "main",
		SetupCommand: setupCommand,
		Prompt:       prompt,
		CloneDepth:   1,
		StorageSize:  "64Mi",
		Timeout:      5 * time.Minute,
	}
}

func (environment testEnvironment) requireJobSucceeded(t *testing.T, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(environment.context, waitInterval, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		job, err := environment.client.BatchV1().Jobs(environment.sessionNamespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		if err != nil {
			return false, err
		}

		if job.Status.Failed > 0 {
			return false, fmt.Errorf("job %s failed", name)
		}

		return job.Status.Succeeded == 1, nil
	})
	if err != nil {
		t.Logf("coding-session output:\n%s", environment.output.String())
	}

	require.NoError(t, err)
}

func (environment testEnvironment) requirePersistentClaims(t *testing.T, target codingsession.Target) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, err := environment.control.Status(environment.context, target)

		return err == nil && resourceNamesEqual(status, "PersistentVolumeClaim", []string{"session-persistent"})
	}, 2*time.Minute, waitInterval)
}

func (environment testEnvironment) requireOnlyPersistentClaims(t *testing.T, target codingsession.Target) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, err := environment.control.Status(environment.context, target)
		if err != nil {
			return false
		}

		return len(status.Resources) == 1 && resourceNamesEqual(status, "PersistentVolumeClaim", []string{"session-persistent"})
	}, 2*time.Minute, waitInterval)
}

func (environment testEnvironment) requireSessionAbsent(t *testing.T, target codingsession.Target) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, err := environment.control.Status(environment.context, target)

		return err == nil && len(status.Resources) == 0
	}, 2*time.Minute, waitInterval)
}

func resourceNames(status codingsession.Status, kind string) []string {
	var names []string
	for _, resource := range status.Resources {
		if resource.Kind == kind {
			names = append(names, resource.Name)
		}
	}

	return names
}

func resourceNamesEqual(status codingsession.Status, kind string, expected []string) bool {
	names := resourceNames(status, kind)
	slices.Sort(names)

	return slices.Equal(names, expected)
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func registerNamespaceCleanup(t *testing.T, client clientset.Interface, namespaces ...string) {
	t.Helper()
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, namespace := range namespaces {
			err := client.CoreV1().Namespaces().Delete(cleanupContext, namespace, metav1.DeleteOptions{})
			assert.True(t, err == nil || apierrors.IsNotFound(err), "delete namespace %s: %v", namespace, err)
		}

		for _, namespace := range namespaces {
			err := wait.PollUntilContextTimeout(cleanupContext, waitInterval, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
				_, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					return false, err
				}

				return apierrors.IsNotFound(err), nil
			})
			assert.NoError(t, err, "namespace %s was not deleted", namespace)
		}
	})
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var suffix [4]byte
	_, err := io.ReadFull(rand.Reader, suffix[:])
	require.NoError(t, err)

	return fmt.Sprintf("%x", suffix)
}
