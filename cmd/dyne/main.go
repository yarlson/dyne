package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yarlson/dyne/internal/agent"
	"github.com/yarlson/dyne/internal/agentconfig"
	"github.com/yarlson/dyne/internal/controlplane"
	dynegithub "github.com/yarlson/dyne/internal/github"
	"github.com/yarlson/dyne/internal/kubernetes"
	"github.com/yarlson/dyne/internal/publish"
	"github.com/yarlson/dyne/internal/session"
	"github.com/yarlson/dyne/internal/storage"
	"github.com/yarlson/dyne/internal/workflow"
	"github.com/yarlson/dyne/internal/workflowconfig"
	"github.com/yarlson/dyne/internal/workload"
)

const (
	defaultServerURL = "http://127.0.0.1:8080"
	defaultNamespace = "coding-agents"
	defaultImage     = "coding-agent:local"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage())
	}

	switch args[0] {
	case "server":
		return serve(ctx, args[1:], stderr)
	case "agents":
		return listAgents(ctx, args[1:], stdout)
	case "workflows":
		return listWorkflows(ctx, args[1:], stdout)
	case "workflow-start":
		return startWorkflow(ctx, args[1:], stdout)
	case "workflow-status":
		return getWorkflowRun(ctx, "workflow-status", args[1:], "", stdout)
	case "workflow-artifacts":
		return getWorkflowRun(ctx, "workflow-artifacts", args[1:], "/artifacts", stdout)
	case "workflow-cancel":
		return changeWorkflowRun(ctx, "workflow-cancel", http.MethodPost, args[1:], "/cancel", stdout)
	case "workflow-delete":
		return changeWorkflowRun(ctx, "workflow-delete", http.MethodDelete, args[1:], "", stdout)
	case "start":
		return start(ctx, args[1:], stdout)
	case "status":
		return getSession(ctx, "status", args[1:], "", stdout)
	case "logs":
		return logs(ctx, args[1:], stdout)
	case "artifacts":
		return getSession(ctx, "artifacts", args[1:], "/artifacts", stdout)
	case "task":
		return task(ctx, args[1:], stdout)
	case "publish":
		return publishSession(ctx, args[1:], stdout)
	case "delete":
		return deleteSession(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage())
	}
}

func serve(ctx context.Context, args []string, stderr io.Writer) (result error) {
	set := flag.NewFlagSet("server", flag.ContinueOnError)
	set.SetOutput(stderr)
	listenAddress := set.String("listen", "127.0.0.1:8080", "HTTP listen address")
	namespace := set.String("namespace", defaultNamespace, "Kubernetes namespace owned by this server")
	image := set.String("image", defaultImage, "coding-agent image")
	storageSize := set.String("storage-size", "10Gi", "persistent session claim size")
	taskTimeout := set.Duration("task-timeout", 2*time.Hour, "default task deadline")
	kubeconfig := set.String("kubeconfig", "", "kubeconfig file")
	contextName := set.String("context", "", "kubeconfig context")
	eksCluster := set.String("eks-cluster", "", "Amazon EKS cluster name")
	awsRegion := set.String("aws-region", "", "AWS region override")
	awsRoleARN := set.String("aws-role-arn", "", "AWS role to assume")
	githubAppID := set.Int64("github-app-id", 0, "GitHub App ID")
	githubInstallationID := set.Int64("github-installation-id", 0, "GitHub App installation ID")
	githubPrivateKeyFile := set.String("github-private-key-file", "", "GitHub App private key file")
	agentsFile := set.String("agents-file", "", "agent definitions YAML file")
	workflowsFile := set.String("workflows-file", "", "workflow definitions YAML file")
	databaseURL := set.String("database-url", defaultDatabaseURL(), "application database URL")
	if err := set.Parse(args); err != nil {
		return err
	}

	var agents *agentconfig.Catalog
	if *agentsFile != "" {
		var err error
		agents, err = agentconfig.Load(*agentsFile, agentconfig.Defaults{
			StorageSize: *storageSize,
			TaskTimeout: *taskTimeout,
		})
		if err != nil {
			return fmt.Errorf("load agents file: %w", err)
		}
	}

	var workflows *workflowconfig.Catalog
	if *workflowsFile != "" {
		if agents == nil {
			return errors.New("--agents-file is required with --workflows-file")
		}

		var err error
		workflows, err = workflowconfig.Load(*workflowsFile, agents)
		if err != nil {
			return fmt.Errorf("load workflows file: %w", err)
		}
	}

	if *githubPrivateKeyFile == "" {
		return errors.New("--github-private-key-file is required")
	}

	privateKey, err := os.ReadFile(*githubPrivateKeyFile)
	if err != nil {
		return fmt.Errorf("read GitHub App private key: %w", err)
	}

	githubApp, err := dynegithub.NewApp(*githubAppID, *githubInstallationID, privateKey)
	if err != nil {
		return err
	}

	restConfig, err := kubernetes.LoadConnectionConfig(ctx, kubernetes.ConnectionConfig{
		KubeconfigPath: *kubeconfig,
		ContextName:    *contextName,
		EKSCluster:     *eksCluster,
		AWSRegion:      *awsRegion,
		AWSRoleARN:     *awsRoleARN,
	})
	if err != nil {
		return err
	}

	runtime, err := workload.NewForConfig(restConfig, workload.Config{
		Namespace: *namespace, Output: io.Discard,
	})
	if err != nil {
		return err
	}

	database, err := storage.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}

	defer func() {
		if err := database.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close storage: %w", err))
		}
	}()

	sessions, err := session.New(session.Config{
		Repository: database.Sessions(), Runtime: runtime, RepositoryAuth: githubApp,
		Image: *image, TaskTimeout: *taskTimeout,
	})
	if err != nil {
		return err
	}

	agentsControl, err := agent.New(sessions, agents)
	if err != nil {
		return err
	}

	publisher, err := publish.New(publish.Config{
		Sessions: sessions, Repository: database.Publications(), Runtime: runtime, RepositoryAuth: githubApp,
	})
	if err != nil {
		return err
	}

	serverConfig := controlplane.Config{ErrorOutput: stderr, Sessions: sessions, Publisher: publisher}
	if err := sessions.ReconcileDeletions(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "reconcile session deletions: %v\n", err)
	}

	if workflows != nil {
		workflowControl, err := workflow.New(workflow.Config{
			Repository: database.Workflows(), ErrorOutput: stderr,
		}, sessions, workflows)
		if err != nil {
			return err
		}

		workflowContext, stopWorkflows := context.WithCancel(ctx)
		workflowDone := make(chan struct{})
		defer func() {
			stopWorkflows()
			<-workflowDone
		}()
		serverConfig.Workflows = workflowControl
		go func() {
			defer close(workflowDone)
			_ = workflowControl.Run(workflowContext, 2*time.Second)
		}()
	}

	handler := controlplane.New(agentsControl, serverConfig)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listenAddress, err)
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}

		return nil
	}
}

func start(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	server := serverFlag(set)
	agent := set.String("agent", "", "configured agent name")
	name := set.String("name", "", "session name")
	repository := set.String("repo", "", "Git repository URL")
	ref := set.String("ref", "main", "initial Git ref")
	prompt := set.String("prompt", "", "coding task")
	timeout := set.Duration("timeout", 0, "task deadline override")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *agent == "" {
		return errors.New("--agent is required")
	}

	body := map[string]any{
		"name": *name, "repository": *repository, "ref": *ref, "prompt": *prompt,
	}
	if *timeout != 0 {
		body["timeout_seconds"] = int64(timeout.Seconds())
	}

	return requestJSON(ctx, http.MethodPost, *server, "/v1/agents/"+url.PathEscape(*agent)+"/sessions", body, stdout)
}

func listAgents(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("agents", flag.ContinueOnError)
	server := serverFlag(set)
	if err := set.Parse(args); err != nil {
		return err
	}

	return requestJSON(ctx, http.MethodGet, *server, "/v1/agents", nil, stdout)
}

func listWorkflows(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("workflows", flag.ContinueOnError)
	server := serverFlag(set)
	if err := set.Parse(args); err != nil {
		return err
	}

	return requestJSON(ctx, http.MethodGet, *server, "/v1/workflows", nil, stdout)
}

func startWorkflow(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("workflow-start", flag.ContinueOnError)
	server := serverFlag(set)
	workflowName := set.String("workflow", "", "configured workflow name")
	name := set.String("name", "", "workflow run name")
	repository := set.String("repo", "", "Git repository URL")
	ref := set.String("ref", "main", "initial Git ref")
	prompt := set.String("prompt", "", "workflow goal")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *workflowName == "" || *name == "" || *repository == "" || *prompt == "" {
		return errors.New("workflow-start requires --workflow, --name, --repo, and --prompt")
	}

	body := map[string]any{
		"name": *name, "repository": *repository, "ref": *ref, "prompt": *prompt,
	}

	return requestJSON(
		ctx, http.MethodPost, *server, "/v1/workflows/"+url.PathEscape(*workflowName)+"/runs", body, stdout,
	)
}

func getWorkflowRun(ctx context.Context, command string, args []string, suffix string, stdout io.Writer) error {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	server := serverFlag(set)
	name := set.String("name", "", "workflow run name")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return errors.New("--name is required")
	}

	return requestJSON(ctx, http.MethodGet, *server, "/v1/workflow-runs/"+url.PathEscape(*name)+suffix, nil, stdout)
}

func changeWorkflowRun(
	ctx context.Context,
	command, method string,
	args []string,
	suffix string,
	stdout io.Writer,
) error {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	server := serverFlag(set)
	name := set.String("name", "", "workflow run name")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return errors.New("--name is required")
	}

	return requestJSON(ctx, method, *server, "/v1/workflow-runs/"+url.PathEscape(*name)+suffix, nil, stdout)
}

func task(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("task", flag.ContinueOnError)
	server := serverFlag(set)
	name := set.String("name", "", "persistent session name")
	timeout := set.Duration("timeout", 0, "task deadline override")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" || set.NArg() != 1 {
		return errors.New("task requires --name and one prompt argument")
	}

	body := map[string]any{"prompt": set.Arg(0)}
	if *timeout != 0 {
		body["timeout_seconds"] = int64(timeout.Seconds())
	}

	return requestJSON(ctx, http.MethodPost, *server, "/v1/sessions/"+url.PathEscape(*name)+"/tasks", body, stdout)
}

func publishSession(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("publish", flag.ContinueOnError)
	server := serverFlag(set)
	name := set.String("name", "", "persistent session name")
	branch := set.String("branch", "", "new remote branch")
	base := set.String("base", "", "pull request base branch")
	commitMessage := set.String("commit-message", "", "workspace commit message")
	ready := set.Bool("ready", false, "create a ready-for-review pull request")
	timeout := set.Duration("timeout", 0, "publish deadline override")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return errors.New("--name is required")
	}

	body := map[string]any{
		"branch": *branch, "base": *base, "commit_message": *commitMessage, "ready": *ready,
	}
	if *timeout != 0 {
		body["timeout_seconds"] = int64(timeout.Seconds())
	}

	return requestJSON(ctx, http.MethodPost, *server, "/v1/sessions/"+url.PathEscape(*name)+"/publish", body, stdout)
}

func getSession(ctx context.Context, command string, args []string, suffix string, stdout io.Writer) error {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	server := serverFlag(set)
	name := set.String("name", "", "session name")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return errors.New("--name is required")
	}

	return requestJSON(ctx, http.MethodGet, *server, "/v1/sessions/"+url.PathEscape(*name)+suffix, nil, stdout)
}

func logs(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("logs", flag.ContinueOnError)
	server := serverFlag(set)
	name := set.String("name", "", "session name")
	follow := set.Bool("follow", false, "follow log output")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return errors.New("--name is required")
	}

	path := "/v1/sessions/" + url.PathEscape(*name) + "/logs"
	if *follow {
		path += "?follow=true"
	}

	return requestJSON(ctx, http.MethodGet, *server, path, nil, stdout)
}

func deleteSession(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("delete", flag.ContinueOnError)
	server := serverFlag(set)
	name := set.String("name", "", "session name")
	destroy := set.Bool("storage", false, "also delete persistent storage")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return errors.New("--name is required")
	}

	path := "/v1/sessions/" + url.PathEscape(*name)
	if *destroy {
		path += "?storage=delete"
	}

	return requestJSON(ctx, http.MethodDelete, *server, path, nil, stdout)
}

func serverFlag(set *flag.FlagSet) *string {
	defaultURL := os.Getenv("DYNE_SERVER")
	if defaultURL == "" {
		defaultURL = defaultServerURL
	}

	return set.String("server", defaultURL, "dyne server URL")
}

func defaultDatabaseURL() string {
	if value := os.Getenv("DYNE_DATABASE_URL"); value != "" {
		return value
	}

	return "sqlite:dyne.db"
}

func requestJSON(ctx context.Context, method, serverURL, path string, body any, output io.Writer) (result error) {
	var contents io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}

		contents = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(serverURL, "/")+path, contents)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if strings.Contains(path, "/logs") {
		client.Timeout = 0
	}

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("call dyne server: %w", err)
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close dyne response: %w", err))
		}
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))

		return fmt.Errorf("dyne server returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}

	if _, err := io.Copy(output, response.Body); err != nil {
		return fmt.Errorf("read dyne response: %w", err)
	}

	return nil
}

func usage() string {
	return "usage: dyne <server|agents|workflows|start|workflow-start|status|workflow-status|logs|artifacts|workflow-artifacts|task|publish|delete|workflow-cancel|workflow-delete> [options]"
}
