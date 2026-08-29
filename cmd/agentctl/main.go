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
	"text/tabwriter"
	"time"

	"coding-agent-k8s/internal/codingsession"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
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
		return stopSession(ctx, args[1:])
	case "resume":
		return resumeSession(ctx, args[1:])
	case "delete":
		return deleteSession(ctx, args[1:])
	case "destroy":
		return destroySession(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage())
	}
}

func bootstrap(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", codingsession.DefaultNamespace, "Kubernetes namespace")
	authFile := set.String("auth-file", "", "Codex auth.json file")
	apiKeyEnv := set.String("api-key-env", "", "environment variable containing a Codex API key")
	githubTokenEnv := set.String("github-token-env", "", "environment variable containing a GitHub token")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *authFile != "" && *apiKeyEnv != "" {
		return errors.New("use either --auth-file or --api-key-env")
	}

	request := codingsession.BootstrapRequest{Namespace: *namespace}
	if err := request.Validate(); err != nil {
		return err
	}

	var err error
	if *authFile != "" {
		request.CodexAuthJSON, err = os.ReadFile(*authFile)
		if err != nil {
			return fmt.Errorf("read auth file: %w", err)
		}
	}

	request.CodexAPIKey, err = environmentValue(*apiKeyEnv)
	if err != nil {
		return err
	}

	request.GitHubToken, err = environmentValue(*githubTokenEnv)
	if err != nil {
		return err
	}

	if err := request.Validate(); err != nil {
		return err
	}

	control, err := newControl(*contextName)
	if err != nil {
		return err
	}

	return control.Bootstrap(ctx, request)
}

func start(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", codingsession.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "session name")
	mode := set.String("mode", "", "explore, update, or long")
	image := set.String("image", codingsession.DefaultImage, "agent image")
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

	request := codingsession.StartRequest{
		Target:       codingsession.Target{Namespace: *namespace, Name: *name},
		Image:        *image,
		Mode:         codingsession.Mode(*mode),
		Repository:   *repository,
		InitialRef:   *ref,
		SetupCommand: *setupCommand,
		Prompt:       *prompt,
		CloneDepth:   *cloneDepth,
		StorageSize:  *storageSize,
		Timeout:      *timeout,
	}
	if err := request.Validate(); err != nil {
		return err
	}

	control, err := newControl(*contextName)
	if err != nil {
		return err
	}

	return control.Start(ctx, request)
}

func status(ctx context.Context, args []string) error {
	target, err := sessionTargetFromFlags("status", args)
	if err != nil {
		return err
	}

	result, err := target.control.Status(ctx, target.target)
	if err != nil {
		return err
	}

	return printStatus(result)
}

func logs(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("logs", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", codingsession.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "session name")
	follow := set.Bool("follow", false, "follow log output")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return errors.New("--name is required")
	}

	control, err := newControl(*contextName)
	if err != nil {
		return err
	}

	return control.StreamLogs(ctx, codingsession.LogRequest{
		Target: codingsession.Target{Namespace: *namespace, Name: *name},
		Follow: *follow,
	})
}

func task(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("task", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", codingsession.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "long session name")
	resume := set.Bool("resume-last", false, "resume the latest Codex thread")
	if err := set.Parse(args); err != nil {
		return err
	}

	if *name == "" || set.NArg() != 1 {
		return errors.New("task requires --name and one prompt argument")
	}

	control, err := newControl(*contextName)
	if err != nil {
		return err
	}

	return control.RunTask(ctx, codingsession.TaskRequest{
		Target:     codingsession.Target{Namespace: *namespace, Name: *name},
		Prompt:     set.Arg(0),
		ResumeLast: *resume,
	})
}

func shell(ctx context.Context, args []string) error {
	target, err := sessionTargetFromFlags("shell", args)
	if err != nil {
		return err
	}

	return target.control.OpenShell(ctx, target.target)
}

func publishPullRequest(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("publish", flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", codingsession.DefaultNamespace, "Kubernetes namespace")
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

	request := codingsession.PublishRequest{
		Target:        codingsession.Target{Namespace: *namespace, Name: *name},
		Branch:        *branch,
		BaseBranch:    *base,
		CommitMessage: *commitMessage,
		Title:         *title,
		Body:          body,
		Draft:         !*ready,
		Timeout:       *timeout,
	}
	if err := request.Validate(); err != nil {
		return err
	}

	control, err := newControl(*contextName)
	if err != nil {
		return err
	}

	result, err := control.Publish(ctx, request)
	if err != nil {
		return err
	}

	printPublishResult(result)

	return nil
}

func stopSession(ctx context.Context, args []string) error {
	target, err := sessionTargetFromFlags("stop", args)
	if err != nil {
		return err
	}

	return target.control.Stop(ctx, target.target)
}

func resumeSession(ctx context.Context, args []string) error {
	target, err := sessionTargetFromFlags("resume", args)
	if err != nil {
		return err
	}

	return target.control.Resume(ctx, target.target)
}

func deleteSession(ctx context.Context, args []string) error {
	target, err := sessionTargetFromFlags("delete", args)
	if err != nil {
		return err
	}

	return target.control.Delete(ctx, target.target)
}

func destroySession(ctx context.Context, args []string) error {
	target, err := sessionTargetFromFlags("destroy", args)
	if err != nil {
		return err
	}

	return target.control.Destroy(ctx, target.target)
}

type sessionTarget struct {
	control *codingsession.Control
	target  codingsession.Target
}

func sessionTargetFromFlags(command string, args []string) (sessionTarget, error) {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	contextName := set.String("context", "", "Kubernetes context")
	namespace := set.String("namespace", codingsession.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "session name")
	if err := set.Parse(args); err != nil {
		return sessionTarget{}, err
	}

	if *name == "" {
		return sessionTarget{}, errors.New("--name is required")
	}

	control, err := newControl(*contextName)
	if err != nil {
		return sessionTarget{}, err
	}

	return sessionTarget{
		control: control,
		target:  codingsession.Target{Namespace: *namespace, Name: *name},
	}, nil
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

func printStatus(status codingsession.Status) error {
	output := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(output, "KIND\tNAME\tREADY\tSTATUS")
	for _, resource := range status.Resources {
		_, _ = fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", resource.Kind, resource.Name, resource.Ready, resource.State)
	}

	if err := output.Flush(); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	return nil
}

func printPublishResult(result codingsession.PublishResult) {
	fmt.Printf("pull request #%d: %s\n", result.PullRequestNumber, result.PullRequestURL)
	fmt.Printf("branch: %s\n", result.Branch)
	if result.CommitSHA != "" {
		fmt.Printf("commit: %s\n", result.CommitSHA)
	}
}

func newControl(contextName string) (*codingsession.Control, error) {
	return codingsession.New(contextName, codingsession.Streams{
		Input:       os.Stdin,
		Output:      os.Stdout,
		ErrorOutput: os.Stderr,
	})
}

func environmentValue(name string) (string, error) {
	if name == "" {
		return "", nil
	}

	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return "", fmt.Errorf("environment variable %s is empty", name)
	}

	return value, nil
}
