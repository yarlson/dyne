package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"coding-agent-k8s/internal/agent"

	"golang.org/x/term"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
)

const fieldManagerName = "agentctl"

// Client applies and manages coding-agent resources through the Kubernetes API.
type Client struct {
	config  *rest.Config
	typed   clientset.Interface
	dynamic dynamic.Interface
	mapper  meta.RESTMapper
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
}

// New returns a client configured from the standard kubeconfig loading rules.
func New(contextName string) (*Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	typed, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(typed.Discovery()))
	return &Client{
		config:  config,
		typed:   typed,
		dynamic: dynamicClient,
		mapper:  mapper,
		stdin:   os.Stdin,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
	}, nil
}

// Apply applies every resource in a Kubernetes List manifest using server-side apply.
func (c *Client) Apply(ctx context.Context, manifest []byte) error {
	var list unstructured.UnstructuredList
	if err := json.Unmarshal(manifest, &list); err != nil {
		return fmt.Errorf("decode resource list: %w", err)
	}
	for i := range list.Items {
		if err := c.applyResource(ctx, &list.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

// CheckSessionModeAvailable returns an error when another workload kind already owns the session name.
func (c *Client) CheckSessionModeAvailable(ctx context.Context, namespace, name string, mode agent.Mode) error {
	switch mode {
	case agent.ModeLong:
		_, err := c.typed.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return fmt.Errorf("job %s already exists; delete it before starting a long session", name)
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("check session Job %s: %w", name, err)
		}
	case agent.ModeExplore, agent.ModeUpdate:
		_, err := c.typed.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return fmt.Errorf("StatefulSet %s already exists; delete it before starting a bounded session", name)
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("check session StatefulSet %s: %w", name, err)
		}
	default:
		return fmt.Errorf("unsupported session mode %q", mode)
	}
	return nil
}

func (c *Client) applyResource(ctx context.Context, resource *unstructured.Unstructured) error {
	if resource.GetName() == "" {
		return fmt.Errorf("apply %s: resource name is required", resource.GroupVersionKind().String())
	}
	mapping, err := c.mapper.RESTMapping(resource.GroupVersionKind().GroupKind(), resource.GroupVersionKind().Version)
	if err != nil {
		return fmt.Errorf("map %s: %w", resource.GroupVersionKind().String(), err)
	}
	client := c.dynamic.Resource(mapping.Resource)
	var resourceClient dynamic.ResourceInterface = client
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if resource.GetNamespace() == "" {
			return fmt.Errorf("apply %s %s: namespace is required", resource.GetKind(), resource.GetName())
		}
		resourceClient = client.Namespace(resource.GetNamespace())
	}
	contents, err := json.Marshal(resource.Object)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", resource.GetKind(), resource.GetName(), err)
	}
	force := true
	if _, err := resourceClient.Patch(ctx, resource.GetName(), types.ApplyPatchType, contents, metav1.PatchOptions{
		FieldManager: fieldManagerName,
		Force:        &force,
	}); err != nil {
		return fmt.Errorf("apply %s %s: %w", resource.GetKind(), resource.GetName(), err)
	}
	_, _ = fmt.Fprintf(c.stdout, "%s/%s applied\n", strings.ToLower(resource.GetKind()), resource.GetName())
	return nil
}

// WriteSessionStatus writes the workloads, Pods, and claims owned by a session to the client's output.
func (c *Client) WriteSessionStatus(ctx context.Context, namespace, session string) error {
	selector := sessionSelector(session)
	jobs, err := c.typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list Jobs: %w", err)
	}
	sets, err := c.typed.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list StatefulSets: %w", err)
	}
	pods, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list Pods: %w", err)
	}
	claims, err := c.typed.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}
	output := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(output, "KIND\tNAME\tREADY\tSTATUS")
	for i := range jobs.Items {
		writeJobStatus(output, &jobs.Items[i])
	}
	for i := range sets.Items {
		writeStatefulSetStatus(output, &sets.Items[i])
	}
	for i := range pods.Items {
		writePodStatus(output, &pods.Items[i])
	}
	for i := range claims.Items {
		_, _ = fmt.Fprintf(output, "PersistentVolumeClaim\t%s\t-\t%s\n", claims.Items[i].Name, claims.Items[i].Status.Phase)
	}
	if err := output.Flush(); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	return nil
}

// StreamPodLogs streams one container's logs to the client's output.
func (c *Client) StreamPodLogs(ctx context.Context, namespace, pod, container string, follow bool) (result error) {
	stream, err := c.typed.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		Follow:    follow,
	}).Stream(ctx)
	if err != nil {
		return fmt.Errorf("open logs for Pod %s: %w", pod, err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close logs for Pod %s: %w", pod, err))
		}
	}()
	if _, err := io.Copy(c.stdout, stream); err != nil {
		return fmt.Errorf("stream logs for Pod %s: %w", pod, err)
	}
	return nil
}

// ExecPod runs a command in a Pod container and optionally connects the local terminal.
func (c *Client) ExecPod(ctx context.Context, namespace, pod, container string, command []string, interactive bool) (result error) {
	request := c.typed.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     interactive,
			Stdout:    true,
			Stderr:    true,
			TTY:       interactive,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(c.config, "POST", request.URL())
	if err != nil {
		return fmt.Errorf("create executor for Pod %s: %w", pod, err)
	}
	options := remotecommand.StreamOptions{Stdout: c.stdout, Stderr: c.stderr, Tty: interactive}
	if interactive {
		terminal, ok := c.stdin.(*os.File)
		if !ok || !term.IsTerminal(int(terminal.Fd())) {
			return errors.New("interactive shell requires a terminal")
		}
		state, err := term.MakeRaw(int(terminal.Fd()))
		if err != nil {
			return fmt.Errorf("enable raw terminal mode: %w", err)
		}
		defer func() {
			if err := term.Restore(int(terminal.Fd()), state); err != nil {
				result = errors.Join(result, fmt.Errorf("restore terminal mode: %w", err))
			}
		}()
		sizes := newTerminalSizeQueue(terminal)
		defer sizes.Stop()
		options.Stdin = c.stdin
		options.TerminalSizeQueue = sizes
	}
	if err := executor.StreamWithContext(ctx, options); err != nil {
		return fmt.Errorf("execute command in Pod %s: %w", pod, err)
	}
	return nil
}

// StopSession scales a long session to zero while retaining its storage.
func (c *Client) StopSession(ctx context.Context, namespace, name string) error {
	return c.scaleSession(ctx, namespace, name, 0)
}

// ResumeSession starts a stopped long session unless its publisher is active.
func (c *Client) ResumeSession(ctx context.Context, namespace, name string) error {
	publisher, err := c.typed.BatchV1().Jobs(namespace).Get(ctx, publisherJobName(name), metav1.GetOptions{})
	if err == nil && !jobComplete(publisher) && !jobFailed(publisher) {
		return fmt.Errorf("cannot resume while publisher Job %s is active", publisher.Name)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("check publisher Job before resume: %w", err)
	}
	return c.scaleSession(ctx, namespace, name, 1)
}

func (c *Client) scaleSession(ctx context.Context, namespace, name string, replicas int32) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		scale, err := c.typed.AppsV1().StatefulSets(namespace).GetScale(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		scale.Spec.Replicas = replicas
		_, err = c.typed.AppsV1().StatefulSets(namespace).UpdateScale(ctx, name, scale, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("scale StatefulSet %s: %w", name, err)
	}
	_, _ = fmt.Fprintf(c.stdout, "statefulset/%s scaled to %d\n", name, replicas)
	return nil
}

// DeleteSession removes a session's compute resources and retains its persistent claims.
func (c *Client) DeleteSession(ctx context.Context, namespace, name string) error {
	return c.deleteSession(ctx, namespace, name, false)
}

// DestroySession removes a session's compute resources and persistent claims.
func (c *Client) DestroySession(ctx context.Context, namespace, name string) error {
	return c.deleteSession(ctx, namespace, name, true)
}

func (c *Client) deleteSession(ctx context.Context, namespace, name string, deleteStorage bool) error {
	if err := c.deletePublisherJob(ctx, namespace, name); err != nil {
		return err
	}
	if err := c.deleteSessionWorkloads(ctx, namespace, name); err != nil {
		return err
	}
	if !deleteStorage {
		return nil
	}
	var deleteErrors []error
	for _, claim := range []string{agent.WorkspaceClaimName(name), agent.HomeClaimName(name), agent.CodexClaimName(name)} {
		err := c.typed.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, claim, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			deleteErrors = append(deleteErrors, fmt.Errorf("delete PersistentVolumeClaim %s: %w", claim, err))
			continue
		}
		_, _ = fmt.Fprintf(c.stdout, "persistentvolumeclaim/%s deleted\n", claim)
	}
	return errors.Join(deleteErrors...)
}

func (c *Client) deleteSessionWorkloads(ctx context.Context, namespace, name string) error {
	propagation := metav1.DeletePropagationBackground
	options := metav1.DeleteOptions{PropagationPolicy: &propagation}
	deletions := []struct {
		kind   string
		remove func() error
	}{
		{kind: "job", remove: func() error { return c.typed.BatchV1().Jobs(namespace).Delete(ctx, name, options) }},
		{kind: "statefulset", remove: func() error {
			return c.typed.AppsV1().StatefulSets(namespace).Delete(ctx, name, options)
		}},
		{kind: "service", remove: func() error { return c.typed.CoreV1().Services(namespace).Delete(ctx, name, options) }},
	}
	var deleteErrors []error
	for _, deletion := range deletions {
		err := deletion.remove()
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			deleteErrors = append(deleteErrors, fmt.Errorf("delete %s %s: %w", deletion.kind, name, err))
			continue
		}
		_, _ = fmt.Fprintf(c.stdout, "%s/%s deleted\n", deletion.kind, name)
	}
	return errors.Join(deleteErrors...)
}

// WaitForReadyPod waits until the newest Pod for a session is ready and returns its name.
func (c *Client) WaitForReadyPod(ctx context.Context, namespace, session string, timeout time.Duration) (string, error) {
	var podName string
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pod, err := c.newestSessionPod(ctx, namespace, session)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if pod.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("pod %s failed", pod.Name)
		}
		if podReady(pod) {
			podName = pod.Name
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return "", fmt.Errorf("wait for session %s Pod: %w", session, err)
	}
	return podName, nil
}

// NewestPodName returns the name of the newest Pod owned by a session.
func (c *Client) NewestPodName(ctx context.Context, namespace, session string) (string, error) {
	pod, err := c.newestSessionPod(ctx, namespace, session)
	if err != nil {
		return "", err
	}
	return pod.Name, nil
}

func (c *Client) newestSessionPod(ctx context.Context, namespace, session string) (*corev1.Pod, error) {
	pods, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sessionSelector(session)})
	if err != nil {
		return nil, fmt.Errorf("list session %s Pods: %w", session, err)
	}
	if len(pods.Items) == 0 {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, session)
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})
	return &pods.Items[0], nil
}

func sessionSelector(session string) string {
	return labels.Set{"coding-agent/session": session}.AsSelector().String()
}

func writeJobStatus(output io.Writer, job *batchv1.Job) {
	status := "Running"
	if job.Status.Succeeded > 0 {
		status = "Complete"
	} else if job.Status.Failed > 0 {
		status = "Failed"
	} else if job.Status.Active == 0 {
		status = "Pending"
	}
	_, _ = fmt.Fprintf(output, "Job\t%s\t%d/1\t%s\n", job.Name, job.Status.Succeeded, status)
}

func writeStatefulSetStatus(output io.Writer, set *appsv1.StatefulSet) {
	desired := int32(0)
	if set.Spec.Replicas != nil {
		desired = *set.Spec.Replicas
	}
	_, _ = fmt.Fprintf(output, "StatefulSet\t%s\t%d/%d\t%s\n", set.Name, set.Status.ReadyReplicas, desired, statefulSetStatus(set, desired))
}

func statefulSetStatus(set *appsv1.StatefulSet, desired int32) string {
	if desired == 0 {
		return "Stopped"
	}
	if set.Status.ReadyReplicas == desired {
		return "Ready"
	}
	return "Pending"
}

func writePodStatus(output io.Writer, pod *corev1.Pod) {
	ready := "0/1"
	if podReady(pod) {
		ready = "1/1"
	}
	_, _ = fmt.Fprintf(output, "Pod\t%s\t%s\t%s\n", pod.Name, ready, pod.Status.Phase)
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

type terminalSizeQueue struct {
	terminal *os.File
	resize   chan os.Signal
	stop     chan struct{}
	once     sync.Once
}

func newTerminalSizeQueue(terminal *os.File) *terminalSizeQueue {
	queue := &terminalSizeQueue{
		terminal: terminal,
		resize:   make(chan os.Signal, 1),
		stop:     make(chan struct{}),
	}
	signal.Notify(queue.resize, syscall.SIGWINCH)
	queue.resize <- syscall.SIGWINCH
	return queue
}

func (q *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case <-q.stop:
		return nil
	case <-q.resize:
		width, height, err := term.GetSize(int(q.terminal.Fd()))
		if err != nil || width <= 0 || height <= 0 || width > 65535 || height > 65535 {
			return nil
		}
		return &remotecommand.TerminalSize{Width: uint16(width), Height: uint16(height)}
	}
}

func (q *terminalSizeQueue) Stop() {
	q.once.Do(func() {
		signal.Stop(q.resize)
		close(q.stop)
	})
}
