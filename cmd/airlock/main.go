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

	"github.com/yarlson/airlock/internal/agent"
	"github.com/yarlson/airlock/internal/agentconfig"
	"github.com/yarlson/airlock/internal/controlplane"
	airlockgithub "github.com/yarlson/airlock/internal/github"
)

const defaultServerURL = "http://127.0.0.1:8080"

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

func serve(ctx context.Context, args []string, stderr io.Writer) error {
	set := flag.NewFlagSet("server", flag.ContinueOnError)
	set.SetOutput(stderr)
	listenAddress := set.String("listen", "127.0.0.1:8080", "HTTP listen address")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace owned by this server")
	image := set.String("image", agent.DefaultImage, "coding-agent image")
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

	if *githubPrivateKeyFile == "" {
		return errors.New("--github-private-key-file is required")
	}

	privateKey, err := os.ReadFile(*githubPrivateKeyFile)
	if err != nil {
		return fmt.Errorf("read GitHub App private key: %w", err)
	}

	githubApp, err := airlockgithub.NewApp(*githubAppID, *githubInstallationID, privateKey)
	if err != nil {
		return err
	}

	control, err := agent.Connect(ctx, agent.Config{
		Connection: agent.Connection{
			KubeconfigPath: *kubeconfig,
			ContextName:    *contextName,
			EKSCluster:     *eksCluster,
			AWSRegion:      *awsRegion,
			AWSRoleARN:     *awsRoleARN,
		},
		Namespace: *namespace, Image: *image, TaskTimeout: *taskTimeout,
	}, io.Discard, githubApp, agents)
	if err != nil {
		return err
	}

	handler := controlplane.New(control, controlplane.Config{ErrorOutput: stderr})
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
	defaultURL := os.Getenv("AIRLOCK_SERVER")
	if defaultURL == "" {
		defaultURL = defaultServerURL
	}

	return set.String("server", defaultURL, "Airlock server URL")
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
		return fmt.Errorf("call Airlock server: %w", err)
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close Airlock response: %w", err))
		}
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))

		return fmt.Errorf("airlock server returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}

	if _, err := io.Copy(output, response.Body); err != nil {
		return fmt.Errorf("read Airlock response: %w", err)
	}

	return nil
}

func usage() string {
	return "usage: airlock <server|agents|start|status|logs|artifacts|task|publish|delete> [options]"
}
