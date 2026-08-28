package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"coding-agent-k8s/internal/agent"
	"coding-agent-k8s/internal/kubernetes"
	"coding-agent-k8s/internal/publish"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "bootstrap":
		return bootstrap(ctx, args[1:])
	case "start":
		return start(ctx, args[1:])
	case "status":
		return status(ctx, args[1:])
	case "logs":
		return logs(ctx, args[1:])
	case "task":
		return task(ctx, args[1:])
	case "shell":
		return shell(ctx, args[1:])
	case "publish":
		return publishPullRequest(ctx, args[1:])
	case "stop":
		return scale(ctx, args[1:], 0)
	case "resume":
		return scale(ctx, args[1:], 1)
	case "delete":
		return deleteSession(ctx, args[1:], false)
	case "destroy":
		return deleteSession(ctx, args[1:], true)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage())
	}
}

func bootstrap(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	authFile := set.String("auth-file", "", "Codex auth.json file")
	apiKeyEnv := set.String("api-key-env", "", "environment variable containing a Codex API key")
	githubTokenEnv := set.String("github-token-env", "", "environment variable containing a GitHub token")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *authFile != "" && *apiKeyEnv != "" {
		return errors.New("use either --auth-file or --api-key-env")
	}
	manifest, err := agent.Bootstrap(*namespace, *authFile, *apiKeyEnv, *githubTokenEnv)
	if err != nil {
		return err
	}
	client, err := kubernetes.New(*contextName)
	if err != nil {
		return err
	}
	return client.Apply(ctx, manifest)
}

func start(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "session name")
	mode := set.String("mode", "", "explore, update, or long")
	image := set.String("image", agent.DefaultImage, "agent image")
	repository := set.String("repo", "", "Git repository URL")
	ref := set.String("ref", "main", "initial Git ref")
	cloneDepth := set.Int("clone-depth", 1, "Git history depth; zero clones full history")
	setupCommand := set.String("setup", "", "setup command run before the agent")
	prompt := set.String("prompt", "", "task for a bounded session")
	storageSize := set.String("storage-size", "10Gi", "size of each persistent claim")
	timeout := set.Duration("timeout", 2*time.Hour, "bounded session deadline")
	if err := set.Parse(args); err != nil {
		return err
	}
	seconds := int64(timeout.Seconds())
	manifest, err := agent.Manifest(agent.Session{
		Name:        *name,
		Namespace:   *namespace,
		Image:       *image,
		Mode:        agent.Mode(*mode),
		Repository:  *repository,
		Ref:         *ref,
		Setup:       *setupCommand,
		Prompt:      *prompt,
		CloneDepth:  *cloneDepth,
		StorageSize: *storageSize,
		Timeout:     seconds,
	})
	if err != nil {
		return err
	}
	client, err := kubernetes.New(*contextName)
	if err != nil {
		return err
	}
	return client.Apply(ctx, manifest)
}

func status(ctx context.Context, args []string) error {
	target, err := parseSessionTarget("status", args)
	if err != nil {
		return err
	}
	return target.client.Status(ctx, target.namespace, target.name)
}

func logs(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("logs", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "session name")
	follow := set.Bool("follow", false, "follow log output")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}
	client, err := kubernetes.New(*contextName)
	if err != nil {
		return err
	}
	pod, err := client.PodName(ctx, *namespace, *name)
	if err != nil {
		return err
	}
	return client.Logs(ctx, *namespace, pod, "agent", *follow)
}

func task(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("task", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "long session name")
	resume := set.Bool("resume-last", false, "resume the latest Codex thread")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *name == "" || set.NArg() != 1 {
		return errors.New("task requires --name and one prompt argument")
	}
	client, err := kubernetes.New(*contextName)
	if err != nil {
		return err
	}
	pod, err := client.WaitPodReady(ctx, *namespace, *name, 120*time.Second)
	if err != nil {
		return err
	}
	command := []string{"/usr/local/bin/agent-entrypoint", "task"}
	if *resume {
		command = append(command, "--resume-last")
	}
	command = append(command, set.Arg(0))
	return client.Exec(ctx, *namespace, pod, "agent", command, false)
}

func shell(ctx context.Context, args []string) error {
	target, err := parseSessionTarget("shell", args)
	if err != nil {
		return err
	}
	pod, err := target.client.WaitPodReady(ctx, target.namespace, target.name, 120*time.Second)
	if err != nil {
		return err
	}
	return target.client.Exec(ctx, target.namespace, pod, "agent", []string{"bash"}, true)
}

func publishPullRequest(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("publish", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "completed or stopped session name")
	branch := set.String("branch", "", "new remote branch name")
	base := set.String("base", "", "pull request base branch; defaults to the session ref")
	commitMessage := set.String("commit-message", "", "commit message")
	title := set.String("title", "", "pull request title; defaults to the commit message")
	bodyFile := set.String("body-file", "", "pull request body file")
	ready := set.Bool("ready", false, "create a ready-for-review pull request instead of a draft")
	timeout := set.Duration("timeout", 10*time.Minute, "publisher Job deadline")
	if err := set.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*title) == "" {
		*title = *commitMessage
	}
	body, err := readPullRequestBody(*bodyFile)
	if err != nil {
		return err
	}
	request := publish.Request{
		Namespace:     *namespace,
		Session:       *name,
		Branch:        *branch,
		Base:          *base,
		CommitMessage: *commitMessage,
		Title:         *title,
		Body:          body,
		Draft:         !*ready,
		Timeout:       *timeout,
	}
	if err := request.Validate(); err != nil {
		return err
	}
	cluster, err := kubernetes.New(*contextName)
	if err != nil {
		return err
	}
	result, err := publish.Run(ctx, cluster, request)
	if err != nil {
		return err
	}
	printPublishResult(result)
	return nil
}

func scale(ctx context.Context, args []string, replicas int32) error {
	command := "stop"
	if replicas == 1 {
		command = "resume"
	}
	target, err := parseSessionTarget(command, args)
	if err != nil {
		return err
	}
	if replicas == 0 {
		return target.client.StopSession(ctx, target.namespace, target.name)
	}
	return target.client.ResumeSession(ctx, target.namespace, target.name)
}

func deleteSession(ctx context.Context, args []string, deleteStorage bool) error {
	command := "delete"
	if deleteStorage {
		command = "destroy"
	}
	target, err := parseSessionTarget(command, args)
	if err != nil {
		return err
	}
	return target.client.DeleteSession(ctx, target.namespace, target.name, deleteStorage)
}

type sessionTarget struct {
	client    *kubernetes.Client
	namespace string
	name      string
}

func parseSessionTarget(command string, args []string) (sessionTarget, error) {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "session name")
	if err := set.Parse(args); err != nil {
		return sessionTarget{}, err
	}
	if *name == "" {
		return sessionTarget{}, errors.New("--name is required")
	}
	client, err := kubernetes.New(*contextName)
	if err != nil {
		return sessionTarget{}, err
	}
	return sessionTarget{client: client, namespace: *namespace, name: *name}, nil
}

func usageError() error {
	return errors.New(usage())
}

func usage() string {
	return "usage: agentctl <bootstrap|start|status|logs|task|shell|publish|stop|resume|delete|destroy> [options]"
}

func readPullRequestBody(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read pull request body: %w", err)
	}
	if len(contents) > 64*1024 {
		return "", errors.New("pull request body cannot exceed 64 KiB")
	}
	return string(contents), nil
}

func printPublishResult(result publish.Result) {
	fmt.Printf("pull request #%d: %s\n", result.PullRequestNumber, result.PullRequestURL)
	fmt.Printf("branch: %s\n", result.Branch)
	if result.Commit != "" {
		fmt.Printf("commit: %s\n", result.Commit)
	}
}
