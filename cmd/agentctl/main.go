package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"coding-agent-k8s/internal/agent"
	"coding-agent-k8s/internal/kubectl"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "bootstrap":
		return bootstrap(args[1:])
	case "start":
		return start(args[1:])
	case "status":
		return status(args[1:])
	case "logs":
		return logs(args[1:])
	case "task":
		return task(args[1:])
	case "shell":
		return shell(args[1:])
	case "stop":
		return scale(args[1:], 0)
	case "resume":
		return scale(args[1:], 1)
	case "delete":
		return deleteSession(args[1:], false)
	case "destroy":
		return deleteSession(args[1:], true)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage())
	}
}

func bootstrap(args []string) error {
	set := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	contextName := set.String("context", "", "kubectl context")
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
	resources, err := agent.Bootstrap(*namespace, *authFile, *apiKeyEnv, *githubTokenEnv)
	if err != nil {
		return err
	}
	manifest, err := agent.JSON(resources)
	if err != nil {
		return fmt.Errorf("encode bootstrap resources: %w", err)
	}
	return kubectl.New(*contextName).Apply(manifest)
}

func start(args []string) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	contextName := set.String("context", "", "kubectl context")
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
	resources, err := agent.Build(agent.Session{
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
	manifest, err := agent.JSON(resources)
	if err != nil {
		return fmt.Errorf("encode session resources: %w", err)
	}
	return kubectl.New(*contextName).Apply(manifest)
}

func status(args []string) error {
	common, err := parseCommon("status", args)
	if err != nil {
		return err
	}
	return common.client.Run("-n", common.namespace, "get", "job,statefulset,pod,pvc", "-l", "coding-agent/session="+common.name, "-o", "wide")
}

func logs(args []string) error {
	set := flag.NewFlagSet("logs", flag.ContinueOnError)
	contextName := set.String("context", "", "kubectl context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "session name")
	follow := set.Bool("follow", false, "follow log output")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}
	pod, err := sessionPod(kubectl.New(*contextName), *namespace, *name)
	if err != nil {
		return err
	}
	command := []string{"-n", *namespace, "logs", pod, "-c", "agent"}
	if *follow {
		command = append(command, "-f")
	}
	return kubectl.New(*contextName).Run(command...)
}

func task(args []string) error {
	set := flag.NewFlagSet("task", flag.ContinueOnError)
	contextName := set.String("context", "", "kubectl context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "long session name")
	resume := set.Bool("resume-last", false, "resume the latest Codex thread")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *name == "" || set.NArg() != 1 {
		return errors.New("task requires --name and one prompt argument")
	}
	client := kubectl.New(*contextName)
	pod, err := readySessionPod(client, *namespace, *name)
	if err != nil {
		return err
	}
	command := []string{"-n", *namespace, "exec", pod, "-c", "agent", "--", "/usr/local/bin/agent-entrypoint", "task"}
	if *resume {
		command = append(command, "--resume-last")
	}
	command = append(command, set.Arg(0))
	return client.Run(command...)
}

func shell(args []string) error {
	common, err := parseCommon("shell", args)
	if err != nil {
		return err
	}
	pod, err := readySessionPod(common.client, common.namespace, common.name)
	if err != nil {
		return err
	}
	return common.client.Run("-n", common.namespace, "exec", "-it", pod, "-c", "agent", "--", "bash")
}

func scale(args []string, replicas int) error {
	command := "stop"
	if replicas == 1 {
		command = "resume"
	}
	common, err := parseCommon(command, args)
	if err != nil {
		return err
	}
	return common.client.Run("-n", common.namespace, "scale", "statefulset/"+common.name, "--replicas", strconv.Itoa(replicas))
}

func deleteSession(args []string, storage bool) error {
	command := "delete"
	if storage {
		command = "destroy"
	}
	common, err := parseCommon(command, args)
	if err != nil {
		return err
	}
	if err := common.client.Run(
		"-n", common.namespace,
		"delete",
		"job/"+common.name,
		"statefulset/"+common.name,
		"service/"+common.name,
		"--ignore-not-found",
	); err != nil {
		return err
	}
	if !storage {
		return nil
	}
	return common.client.Run(
		"-n", common.namespace,
		"delete",
		"pvc/workspace-"+common.name,
		"pvc/home-"+common.name,
		"pvc/codex-"+common.name,
		"--ignore-not-found",
	)
}

type commonOptions struct {
	client    kubectl.Client
	namespace string
	name      string
}

func parseCommon(command string, args []string) (commonOptions, error) {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	contextName := set.String("context", "", "kubectl context")
	namespace := set.String("namespace", agent.DefaultNamespace, "Kubernetes namespace")
	name := set.String("name", "", "session name")
	if err := set.Parse(args); err != nil {
		return commonOptions{}, err
	}
	if *name == "" {
		return commonOptions{}, errors.New("--name is required")
	}
	return commonOptions{client: kubectl.New(*contextName), namespace: *namespace, name: *name}, nil
}

func readySessionPod(client kubectl.Client, namespace string, name string) (string, error) {
	if err := client.Run("-n", namespace, "wait", "pod", "-l", "coding-agent/session="+name, "--for=condition=Ready", "--timeout=120s"); err != nil {
		return "", err
	}
	return sessionPod(client, namespace, name)
}

func sessionPod(client kubectl.Client, namespace string, name string) (string, error) {
	pod, err := client.Output("-n", namespace, "get", "pod", "-l", "coding-agent/session="+name, "-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	pod = strings.TrimSpace(pod)
	if pod == "" {
		return "", fmt.Errorf("session %s has no Pod", name)
	}
	return pod, nil
}

func usageError() error {
	return errors.New(usage())
}

func usage() string {
	return "usage: agentctl <bootstrap|start|status|logs|task|shell|stop|resume|delete|destroy> [options]"
}
