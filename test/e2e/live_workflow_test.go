//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v83/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/yarlson/dyne/internal/agent"
	dynegithub "github.com/yarlson/dyne/internal/github"
	"github.com/yarlson/dyne/internal/workflow"
	workflowstorage "github.com/yarlson/dyne/internal/workflow/storage"
)

const (
	integrationContextEnvironment      = "KUBERNETES_INTEGRATION_CONTEXT"
	e2eImageEnvironment                = "E2E_IMAGE"
	e2eCodexAuthFileEnvironment        = "E2E_CODEX_AUTH_FILE"
	e2eGitHubAppIDEnvironment          = "E2E_GITHUB_APP_ID"
	e2eGitHubInstallationIDEnvironment = "E2E_GITHUB_INSTALLATION_ID"
	e2eGitHubPrivateKeyFileEnvironment = "E2E_GITHUB_PRIVATE_KEY_FILE"
	testRepository                     = "https://github.com/lokalise/ratchet-test-service.git"
	testRepositoryOwner                = "lokalise"
	testRepositoryName                 = "ratchet-test-service"
	testRepositoryBase                 = "main"
	brokenDocumentationLink            = "[environment variable configuration]()"
	fixedDocumentationLink             = "[environment variable configuration](./docs/environment-variables.md)"
	sessionTimeout                     = 35 * time.Minute
	taskTimeout                        = 20 * time.Minute
	publishTimeout                     = 10 * time.Minute
	cleanupTimeout                     = 3 * time.Minute
	waitInterval                       = 2 * time.Second
)

const liveSetupCommand = `set -euo pipefail
mise use --global --pin node@24.17.0
mkdir -p "${HOME}/.local/bin"
export PATH="${MISE_DATA_DIR}/shims:${HOME}/.local/bin:${PATH}"
corepack enable --install-directory "${HOME}/.local/bin" pnpm
corepack install --global pnpm@11.9.0
printf 'export PATH="%s/shims:%s/.local/bin:$PATH"\n' "${MISE_DATA_DIR}" "${HOME}" > "${HOME}/.bash_profile"
pnpm install --frozen-lockfile --ignore-scripts`

const fixDocumentationLinkPrompt = `Fix only the empty Markdown link labeled "environment variable configuration" in README.md. Change it from [environment variable configuration]() to [environment variable configuration](./docs/environment-variables.md). Do not modify package versions, CHANGELOG.md, dependencies, production code, or any other line. Run pnpm run build, pnpm run lint, and git diff --check. If any required check cannot pass, report the exact blocker instead of claiming completion.`

const inspectDocumentationPrompt = `Inspect README.md and docs/environment-variables.md. Identify the exact correction needed for the empty link labeled "environment variable configuration". Do not modify files. Return a workflow output object with the current link, required link, and evidence that the target file exists.`

const inspectValidationPrompt = `Inspect package.json and the repository structure. Identify the exact commands that should validate a one-line README link fix and any constraint that keeps the change scoped. Do not modify files. Return a workflow output object with a checks array and scope constraint.`

type taskOutcome struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Blocker string `json:"blocker"`
}

type pullRequestArtifact struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func TestWorkflowUsesParallelReviewersThenPublishesImplementation(t *testing.T) {
	environment := newLiveTestEnvironment(t)
	environment.requireBrokenLinkOnMain(t)

	_, err := environment.workflows.Start(environment.context, workflow.StartRequest{
		Workflow:   "documentation-fix",
		Name:       environment.run,
		Repository: testRepository,
		Ref:        testRepositoryBase,
		Prompt:     fixDocumentationLinkPrompt,
	})
	require.NoError(t, err)
	run := environment.requireWorkflowCompleted(t)
	assert.Equal(t, workflow.StepCompleted, run.Steps["inspect-documentation"].State)
	assert.Equal(t, workflow.StepCompleted, run.Steps["inspect-validation"].State)
	assert.Equal(t, workflow.StepCompleted, run.Steps["implement"].State)
	assert.NotEmpty(t, run.Steps["inspect-documentation"].Output)
	assert.NotEmpty(t, run.Steps["inspect-validation"].Output)
	require.NotNil(t, run.Steps["implement"].StartedAt)
	require.NotNil(t, run.Steps["inspect-documentation"].FinishedAt)
	require.NotNil(t, run.Steps["inspect-validation"].FinishedAt)
	assert.False(t, run.Steps["implement"].StartedAt.Before(*run.Steps["inspect-documentation"].FinishedAt))
	assert.False(t, run.Steps["implement"].StartedAt.Before(*run.Steps["inspect-validation"].FinishedAt))

	workflowArtifacts, err := environment.workflows.Artifacts(environment.context, environment.run)
	require.NoError(t, err)
	require.NotEmpty(t, workflowArtifacts.PublishableSession)
	artifacts, err := environment.agents.Artifacts(environment.context, workflowArtifacts.PublishableSession)
	require.NoError(t, err)
	var outcome taskOutcome
	require.NoError(t, json.Unmarshal(artifacts.Outcome, &outcome))
	if outcome.Status != "completed" {
		environment.logWorkflowLogs(t, run)
	}
	require.Equal(t, "completed", outcome.Status, "summary=%q blocker=%q", outcome.Summary, outcome.Blocker)
	assert.NotEmpty(t, outcome.Summary)
	assert.Empty(t, outcome.Blocker)
	var pullArtifact pullRequestArtifact
	require.NoError(t, json.Unmarshal(artifacts.PullRequest, &pullArtifact))
	require.NotEmpty(t, pullArtifact.Title)
	require.NotEmpty(t, pullArtifact.Body)

	result, err := environment.agents.Publish(environment.context, agent.PublishRequest{
		Name:          workflowArtifacts.PublishableSession,
		Branch:        environment.branch,
		BaseBranch:    testRepositoryBase,
		CommitMessage: "docs: fix environment documentation link",
		Draft:         true,
		Timeout:       publishTimeout,
	})
	require.NoError(t, err)

	environment.requireDraftPullRequest(t, result, pullArtifact)
}

type liveTestEnvironment struct {
	context    context.Context
	agents     *agent.Control
	workflows  *workflow.Control
	kubernetes clientset.Interface
	github     *gh.Client
	output     *bytes.Buffer
	namespace  string
	run        string
	branch     string
}

type liveTestConfig struct {
	contextName string
	image       string
	codexAuth   []byte
	githubApp   *dynegithub.App
}

type agentCatalog map[string]agent.AgentDefinition

func newLiveTestEnvironment(t *testing.T) liveTestEnvironment {
	t.Helper()
	config := loadLiveTestConfig(t)
	testContext, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	t.Cleanup(cancel)

	kubernetesClient := newKubernetesClient(t, config.contextName)
	suffix := randomSuffix(t)
	namespace := "dyne-e2e-" + suffix
	run := "ratchet-" + suffix
	branch := "dyne/e2e-readme-link-" + suffix
	createSessionNamespace(t, testContext, kubernetesClient, namespace)
	registerNamespaceCleanup(t, kubernetesClient, namespace)
	createCodexSecret(t, testContext, kubernetesClient, namespace, config.codexAuth)

	githubClient, err := newGitHubClient(testContext, config.githubApp)
	require.NoError(t, err)
	registerGitHubCleanup(t, config.githubApp, branch)

	output := &bytes.Buffer{}
	agentControl, err := agent.Connect(testContext, agent.Config{
		Connection:  agent.Connection{ContextName: config.contextName},
		Namespace:   namespace,
		Image:       config.image,
		TaskTimeout: taskTimeout,
	}, output, config.githubApp, liveAgentCatalog())
	require.NoError(t, err)
	repository, err := workflowstorage.Open(testContext, "sqlite:"+t.TempDir()+"/workflows.db")
	require.NoError(t, err)
	workflowControl, err := workflow.New(workflow.Config{
		Repository: repository, ErrorOutput: output,
	}, agentControl, liveWorkflowCatalog())
	require.NoError(t, err)
	t.Cleanup(func() {
		current, getErr := workflowControl.Get(context.Background(), run)
		if getErr == nil && current.State != workflow.StateCompleted && current.State != workflow.StateBlocked && current.State != workflow.StateFailed && current.State != workflow.StateCanceled {
			_ = workflowControl.Cancel(context.Background(), run)
			_ = workflowControl.Reconcile(context.Background(), run)
		}

		_ = workflowControl.Delete(context.Background(), run)
		require.NoError(t, repository.Close())
	})

	return liveTestEnvironment{
		context: testContext, agents: agentControl, workflows: workflowControl,
		kubernetes: kubernetesClient, github: githubClient, output: output,
		namespace: namespace, run: run, branch: branch,
	}
}

func loadLiveTestConfig(t *testing.T) liveTestConfig {
	t.Helper()
	contextName := requiredEnvironment(t, integrationContextEnvironment)
	image := requiredEnvironment(t, e2eImageEnvironment)
	authFile := requiredEnvironment(t, e2eCodexAuthFileEnvironment)
	appID := requiredPositiveIntegerEnvironment(t, e2eGitHubAppIDEnvironment)
	installationID := requiredPositiveIntegerEnvironment(t, e2eGitHubInstallationIDEnvironment)
	privateKeyFile := requiredEnvironment(t, e2eGitHubPrivateKeyFileEnvironment)

	codexAuth, err := os.ReadFile(authFile)
	require.NoError(t, err, "read %s", e2eCodexAuthFileEnvironment)
	require.True(t, json.Valid(codexAuth), "%s must contain JSON", e2eCodexAuthFileEnvironment)
	privateKey, err := os.ReadFile(privateKeyFile)
	require.NoError(t, err, "read %s", e2eGitHubPrivateKeyFileEnvironment)
	githubApp, err := dynegithub.NewApp(appID, installationID, privateKey)
	require.NoError(t, err)

	return liveTestConfig{contextName: contextName, image: image, codexAuth: codexAuth, githubApp: githubApp}
}

func requiredEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Skip(name + " is required")
	}

	return value
}

func requiredPositiveIntegerEnvironment(t *testing.T, name string) int64 {
	t.Helper()
	value := requiredEnvironment(t, name)
	parsed, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err, "%s must be a positive integer", name)
	require.Positive(t, parsed, "%s must be a positive integer", name)

	return parsed
}

func liveAgentCatalog() agentCatalog {
	return agentCatalog{
		"reviewer": {
			Name: "reviewer", Description: "Inspects one bounded concern and returns structured findings.", Storage: agent.StorageEphemeral,
			Instructions: "Follow the repository AGENTS.md. Inspect only the requested concern, do not modify files, and return concise evidence.",
			SetupCommand: liveSetupCommand, CloneDepth: 1, StorageSize: "5Gi", Timeout: taskTimeout,
		},
		"implementer": {
			Name: "implementer", Description: "Implements and publishes one focused change.", Storage: agent.StoragePersistent,
			Instructions: "Follow the repository AGENTS.md. Keep the change limited to the requested behavior and report failed checks honestly.",
			SetupCommand: liveSetupCommand, CloneDepth: 1, StorageSize: "5Gi", Timeout: taskTimeout,
		},
	}
}

type workflowCatalog map[string]workflow.Definition

func liveWorkflowCatalog() workflowCatalog {
	reviewer, _ := liveAgentCatalog().Find("reviewer")
	implementer, _ := liveAgentCatalog().Find("implementer")

	return workflowCatalog{"documentation-fix": {
		Name: "documentation-fix", Description: "Inspect and implement one documentation fix.", MaxParallelism: 2,
		Steps: map[string]workflow.StepDefinition{
			"inspect-documentation": {
				Name: "inspect-documentation", Agent: "reviewer", Prompt: inspectDocumentationPrompt, ResolvedAgent: reviewer,
			},
			"inspect-validation": {
				Name: "inspect-validation", Agent: "reviewer", Prompt: inspectValidationPrompt, ResolvedAgent: reviewer,
			},
			"implement": {
				Name: "implement", Agent: "implementer", Prompt: fixDocumentationLinkPrompt,
				After: []string{"inspect-documentation", "inspect-validation"}, Publishable: true, ResolvedAgent: implementer,
			},
		},
	}}
}

func (c workflowCatalog) List() []workflow.Summary { return []workflow.Summary{} }

func (c workflowCatalog) Find(name string) (workflow.Definition, bool) {
	definition, found := c[name]

	return workflow.CloneDefinition(definition), found
}

func (c agentCatalog) List() []agent.AgentSummary {
	return []agent.AgentSummary{}
}

func (c agentCatalog) Find(name string) (agent.AgentDefinition, bool) {
	definition, found := c[name]

	return definition, found
}

func createSessionNamespace(t *testing.T, ctx context.Context, client clientset.Interface, namespace string) {
	t.Helper()
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespace,
		Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": "restricted",
			"pod-security.kubernetes.io/audit":   "restricted",
			"pod-security.kubernetes.io/warn":    "restricted",
		},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func createCodexSecret(t *testing.T, ctx context.Context, client clientset.Interface, namespace string, auth []byte) {
	t.Helper()
	_, err := client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "coding-agent-auth", Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"auth.json": auth},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
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

func newGitHubClient(ctx context.Context, app *dynegithub.App) (*gh.Client, error) {
	token, err := app.InstallationToken(ctx)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	return gh.NewClient(httpClient).WithAuthToken(token), nil
}

func (environment liveTestEnvironment) requireBrokenLinkOnMain(t *testing.T) {
	t.Helper()
	contents := repositoryFile(t, environment.context, environment.github, testRepositoryBase, "README.md")
	require.Contains(t, contents, brokenDocumentationLink)
	assert.NotContains(t, contents, fixedDocumentationLink)
}

func (environment liveTestEnvironment) requireWorkflowCompleted(t *testing.T) workflow.Run {
	t.Helper()
	var run workflow.Run
	err := wait.PollUntilContextTimeout(environment.context, waitInterval, taskTimeout, true, func(ctx context.Context) (bool, error) {
		if err := environment.workflows.Reconcile(ctx, environment.run); err != nil {
			return false, err
		}

		var err error
		run, err = environment.workflows.Get(ctx, environment.run)
		if err != nil {
			return false, err
		}

		switch run.State {
		case workflow.StateCompleted:
			return true, nil
		case workflow.StateBlocked, workflow.StateFailed, workflow.StateCanceled:
			return false, fmt.Errorf("workflow run %s finished as %s", environment.run, run.State)
		default:
			return false, nil
		}
	})
	if err != nil {
		environment.logPodDiagnostics(t)
		environment.logWorkflowLogs(t, run)
		t.Logf("coding-session output:\n%s", environment.output.String())
	}

	require.NoError(t, err)

	return run
}

func (environment liveTestEnvironment) logWorkflowLogs(t *testing.T, run workflow.Run) {
	t.Helper()
	for _, step := range run.Steps {
		var logs bytes.Buffer
		if err := environment.agents.WriteLogs(environment.context, step.Session, false, &logs); err != nil {
			t.Logf("read workflow step %s logs: %v", step.Name, err)

			continue
		}

		t.Logf("workflow step %s logs:\n%s", step.Name, logs.String())
	}
}

func (environment liveTestEnvironment) logPodDiagnostics(t *testing.T) {
	t.Helper()
	pods, err := environment.kubernetes.CoreV1().Pods(environment.namespace).List(environment.context, metav1.ListOptions{})
	if !assert.NoError(t, err, "list failed Job Pods") {
		return
	}

	for _, pod := range pods.Items {
		t.Logf("Pod %s phase=%s reason=%s message=%s", pod.Name, pod.Status.Phase, pod.Status.Reason, pod.Status.Message)
		for _, status := range pod.Status.InitContainerStatuses {
			t.Logf("Pod %s init container %s: %s", pod.Name, status.Name, containerStateDescription(status))
			environment.logPodContainer(t, pod.Name, status.Name)
		}
		for _, status := range pod.Status.ContainerStatuses {
			t.Logf("Pod %s container %s: %s", pod.Name, status.Name, containerStateDescription(status))
			environment.logPodContainer(t, pod.Name, status.Name)
		}
	}
}

func (environment liveTestEnvironment) logPodContainer(t *testing.T, pod, container string) {
	t.Helper()
	logs, err := environment.kubernetes.CoreV1().Pods(environment.namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
	}).DoRaw(environment.context)
	if err != nil {
		t.Logf("read Pod %s container %s logs: %v", pod, container, err)

		return
	}

	t.Logf("Pod %s container %s logs:\n%s", pod, container, logs)
}

func containerStateDescription(status corev1.ContainerStatus) string {
	if terminated := status.State.Terminated; terminated != nil {
		return fmt.Sprintf("terminated with exit code %d, reason=%s, message=%s", terminated.ExitCode, terminated.Reason, terminated.Message)
	}
	if waiting := status.State.Waiting; waiting != nil {
		return fmt.Sprintf("waiting, reason=%s, message=%s", waiting.Reason, waiting.Message)
	}
	if status.State.Running != nil {
		return "running"
	}

	return "unknown"
}

func (environment liveTestEnvironment) requireDraftPullRequest(t *testing.T, result agent.PublishResult, artifact pullRequestArtifact) {
	t.Helper()
	require.Positive(t, result.PullRequestNumber)
	assert.Equal(t, environment.branch, result.Branch)
	assert.Regexp(t, `^[0-9a-f]{40}$`, result.CommitSHA)

	pull, _, err := environment.github.PullRequests.Get(environment.context, testRepositoryOwner, testRepositoryName, result.PullRequestNumber)
	require.NoError(t, err)
	assert.Equal(t, result.PullRequestURL, pull.GetHTMLURL())
	assert.Equal(t, "open", pull.GetState())
	assert.True(t, pull.GetDraft())
	assert.Equal(t, environment.branch, pull.GetHead().GetRef())
	assert.Equal(t, testRepositoryBase, pull.GetBase().GetRef())
	assert.Equal(t, artifact.Title, pull.GetTitle())
	assert.Equal(t, artifact.Body, pull.GetBody())

	files, _, err := environment.github.PullRequests.ListFiles(
		environment.context, testRepositoryOwner, testRepositoryName, result.PullRequestNumber, nil,
	)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "README.md", files[0].GetFilename())
	assert.Equal(t, "modified", files[0].GetStatus())

	contents := repositoryFile(t, environment.context, environment.github, environment.branch, "README.md")
	assert.Contains(t, contents, fixedDocumentationLink)
	assert.NotContains(t, contents, brokenDocumentationLink)
}

func repositoryFile(t *testing.T, ctx context.Context, client *gh.Client, ref, path string) string {
	t.Helper()
	file, directory, _, err := client.Repositories.GetContents(ctx, testRepositoryOwner, testRepositoryName, path, &gh.RepositoryContentGetOptions{Ref: ref})
	require.NoError(t, err)
	require.Nil(t, directory)
	require.NotNil(t, file)
	contents, err := file.GetContent()
	require.NoError(t, err)

	return contents
}

func registerGitHubCleanup(t *testing.T, app *dynegithub.App, branch string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		client, err := newGitHubClient(ctx, app)
		if !assert.NoError(t, err, "create GitHub cleanup client") {
			return
		}

		pulls, _, err := client.PullRequests.List(ctx, testRepositoryOwner, testRepositoryName, &gh.PullRequestListOptions{
			State: "all", Head: testRepositoryOwner + ":" + branch, Base: testRepositoryBase,
			ListOptions: gh.ListOptions{PerPage: 10},
		})
		if assert.NoError(t, err, "find E2E pull request") {
			for _, pull := range pulls {
				if pull.GetState() != "open" {
					continue
				}

				closed := "closed"
				_, _, err = client.PullRequests.Edit(ctx, testRepositoryOwner, testRepositoryName, pull.GetNumber(), &gh.PullRequest{State: &closed})
				assert.NoError(t, err, "close E2E pull request %d", pull.GetNumber())
			}
		}

		_, err = client.Git.DeleteRef(ctx, testRepositoryOwner, testRepositoryName, "heads/"+branch)
		assert.True(t, err == nil || isGitHubMissingRef(err), "delete E2E branch %s: %v", branch, err)
	})
}

func isGitHubMissingRef(err error) bool {
	var responseError *gh.ErrorResponse
	if !errors.As(err, &responseError) || responseError.Response == nil {
		return false
	}
	if responseError.Response.StatusCode == http.StatusNotFound {
		return true
	}

	return responseError.Response.StatusCode == http.StatusUnprocessableEntity && responseError.Message == "Reference does not exist"
}

func registerNamespaceCleanup(t *testing.T, client clientset.Interface, namespace string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		err := client.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
		assert.True(t, err == nil || apierrors.IsNotFound(err), "delete namespace %s: %v", namespace, err)
		err = wait.PollUntilContextTimeout(ctx, waitInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
			_, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}

			return apierrors.IsNotFound(err), nil
		})
		assert.NoError(t, err, "namespace %s was not deleted", namespace)
	})
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var suffix [4]byte
	_, err := io.ReadFull(rand.Reader, suffix[:])
	require.NoError(t, err)

	return fmt.Sprintf("%x", suffix)
}
