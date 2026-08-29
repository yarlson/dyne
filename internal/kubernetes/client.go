package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/retry"

	"github.com/yarlson/airlock/internal/sessionmanifest"
)

const fieldManagerName = "airlock"

// Client applies and manages coding-agent resources through the Kubernetes API.
type Client struct {
	typed   clientset.Interface
	dynamic dynamic.Interface
	mapper  meta.RESTMapper
	stdout  io.Writer
}

// SessionDefinition contains the retained inputs needed to continue a persistent session.
type SessionDefinition struct {
	// Image runs the session task.
	Image string
	// Repository is the Git repository stored in the workspace.
	Repository string
	// InitialRef is the session's original Git ref.
	InitialRef string
	// SetupCommand prepares the retained workspace before each task.
	SetupCommand string
	// CloneDepth is the Git history depth used for the initial clone.
	CloneDepth int
	// StorageSize is the size of the retained session claim.
	StorageSize string
}

// New returns a client configured from the standard kubeconfig loading rules.
func New(contextName string, stdout io.Writer) (*Client, error) {
	config, err := loadKubeconfig("", contextName)
	if err != nil {
		return nil, err
	}

	return NewForConfig(config, stdout)
}

// NewForConfig returns a client that uses server-owned Kubernetes credentials.
func NewForConfig(config *rest.Config, stdout io.Writer) (*Client, error) {
	if config == nil {
		return nil, errors.New("kubernetes configuration is required")
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
		typed:   typed,
		dynamic: dynamicClient,
		mapper:  mapper,
		stdout:  stdout,
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

// CheckSessionAvailable returns an error when any Job or claim already owns the session name.
func (c *Client) CheckSessionAvailable(ctx context.Context, namespace, name string) error {
	selector := sessionSelector(name)
	jobs, err := c.typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("check session Jobs: %w", err)
	}

	claims, err := c.typed.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("check session PersistentVolumeClaims: %w", err)
	}

	if len(jobs.Items) > 0 || len(claims.Items) > 0 {
		return fmt.Errorf("session %s already exists", name)
	}

	return nil
}

// SetGitHubToken stores the short-lived repository credential used by clone and publisher workloads.
func (c *Client) SetGitHubToken(ctx context.Context, namespace, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("GitHub installation token is required")
	}

	secrets := c.typed.CoreV1().Secrets(namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret, err := secrets.Get(ctx, sessionmanifest.GitHubTokenSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: sessionmanifest.GitHubTokenSecretName, Namespace: namespace},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{"token": []byte(token)},
			}, metav1.CreateOptions{})

			return err
		}

		if err != nil {
			return err
		}

		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}

		secret.Data["token"] = []byte(token)
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})

		return err
	})
	if err != nil {
		return fmt.Errorf("store GitHub installation token: %w", err)
	}

	return nil
}

// PersistentSessionDefinition returns continuation inputs after confirming that no task is active.
func (c *Client) PersistentSessionDefinition(ctx context.Context, namespace, name string) (SessionDefinition, error) {
	jobs, err := c.typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: sessionTaskSelector(name)})
	if err != nil {
		return SessionDefinition{}, fmt.Errorf("list session Jobs: %w", err)
	}

	for i := range jobs.Items {
		if jobs.Items[i].Status.Active > 0 {
			return SessionDefinition{}, fmt.Errorf("session %s already has an active task", name)
		}
	}

	publisher, err := c.typed.BatchV1().Jobs(namespace).Get(ctx, publisherJobName(name), metav1.GetOptions{})
	if err == nil && !jobComplete(publisher) && !jobFailed(publisher) {
		return SessionDefinition{}, fmt.Errorf("session %s has an active publisher", name)
	}

	if err != nil && !apierrors.IsNotFound(err) {
		return SessionDefinition{}, fmt.Errorf("check publisher Job: %w", err)
	}

	claim, err := c.typed.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, sessionmanifest.SessionClaimName(name), metav1.GetOptions{})
	if err != nil {
		return SessionDefinition{}, fmt.Errorf("get session PersistentVolumeClaim: %w", err)
	}

	if claim.Status.Phase != corev1.ClaimBound {
		return SessionDefinition{}, fmt.Errorf("session PersistentVolumeClaim is %s, want Bound", claim.Status.Phase)
	}

	cloneDepth, err := strconv.Atoi(claim.Annotations["airlock.yarlson.dev/clone-depth"])
	if err != nil || cloneDepth < 0 {
		return SessionDefinition{}, errors.New("session has an invalid clone depth")
	}

	image := claim.Annotations["airlock.yarlson.dev/image"]
	initialRef := claim.Annotations["airlock.yarlson.dev/initial-ref"]
	if image == "" || initialRef == "" {
		return SessionDefinition{}, errors.New("session PersistentVolumeClaim has an incomplete definition")
	}

	return SessionDefinition{
		Image:        image,
		Repository:   claim.Annotations["airlock.yarlson.dev/repository"],
		InitialRef:   initialRef,
		SetupCommand: claim.Annotations["airlock.yarlson.dev/setup"],
		CloneDepth:   cloneDepth,
		StorageSize:  claim.Spec.Resources.Requests.Storage().String(),
	}, nil
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

	if _, err := resourceClient.Patch(ctx, resource.GetName(), types.ApplyPatchType, contents, metav1.PatchOptions{
		FieldManager: fieldManagerName,
		Force:        new(true),
	}); err != nil {
		return fmt.Errorf("apply %s %s: %w", resource.GetKind(), resource.GetName(), err)
	}

	_, _ = fmt.Fprintf(c.stdout, "%s/%s applied\n", strings.ToLower(resource.GetKind()), resource.GetName())

	return nil
}

// ResourceStatus describes the readiness and state of one Kubernetes resource.
type ResourceStatus struct {
	// Kind identifies the resource type.
	Kind string
	// Name identifies the resource.
	Name string
	// Ready reports current readiness against the desired count.
	Ready string
	// State reports the resource lifecycle state.
	State string
}

// TaskArtifacts contains the validated result files reported by the latest task Pod.
type TaskArtifacts struct {
	// Outcome is the task's completed, blocked, or failed result.
	Outcome json.RawMessage
	// PullRequest is the proposed pull request metadata for completed work.
	PullRequest json.RawMessage
}

// SessionStatus returns the workloads, Pods, and claims owned by a session.
func (c *Client) SessionStatus(ctx context.Context, namespace, session string) ([]ResourceStatus, error) {
	selector := sessionSelector(session)
	jobs, err := c.typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list Jobs: %w", err)
	}

	pods, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list Pods: %w", err)
	}

	claims, err := c.typed.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}

	resources := make([]ResourceStatus, 0, len(jobs.Items)+len(pods.Items)+len(claims.Items))
	for i := range jobs.Items {
		resources = append(resources, jobResourceStatus(&jobs.Items[i]))
	}

	for i := range pods.Items {
		resources = append(resources, podResourceStatus(&pods.Items[i]))
	}

	for i := range claims.Items {
		resources = append(resources, ResourceStatus{
			Kind:  "PersistentVolumeClaim",
			Name:  claims.Items[i].Name,
			Ready: "-",
			State: string(claims.Items[i].Status.Phase),
		})
	}

	return resources, nil
}

// StreamPodLogs writes one container's logs to the supplied output.
func (c *Client) StreamPodLogs(ctx context.Context, namespace, pod, container string, follow bool, output io.Writer) (result error) {
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
	if _, err := io.Copy(output, stream); err != nil {
		return fmt.Errorf("stream logs for Pod %s: %w", pod, err)
	}

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

	claim := sessionmanifest.SessionClaimName(name)
	if err := c.typed.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, claim, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("delete PersistentVolumeClaim %s: %w", claim, err)
	}

	_, _ = fmt.Fprintf(c.stdout, "persistentvolumeclaim/%s deleted\n", claim)

	return nil
}

func (c *Client) deleteSessionWorkloads(ctx context.Context, namespace, name string) error {
	options := metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationBackground)}
	jobs, err := c.typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: sessionSelector(name)})
	if err != nil {
		return fmt.Errorf("list session Jobs for deletion: %w", err)
	}

	var deleteErrors []error
	for i := range jobs.Items {
		jobName := jobs.Items[i].Name
		err := c.typed.BatchV1().Jobs(namespace).Delete(ctx, jobName, options)
		if apierrors.IsNotFound(err) {
			continue
		}

		if err != nil {
			deleteErrors = append(deleteErrors, fmt.Errorf("delete Job %s: %w", jobName, err))

			continue
		}

		_, _ = fmt.Fprintf(c.stdout, "job/%s deleted\n", jobName)
	}

	return errors.Join(deleteErrors...)
}

// NewestPodName returns the name of the newest Pod owned by a session.
func (c *Client) NewestPodName(ctx context.Context, namespace, session string) (string, error) {
	pod, err := c.newestSessionPod(ctx, namespace, session)
	if err != nil {
		return "", err
	}

	return pod.Name, nil
}

// SessionArtifacts returns the result files reported by the newest terminated task Pod.
func (c *Client) SessionArtifacts(ctx context.Context, namespace, session string) (TaskArtifacts, error) {
	pod, err := c.newestSessionPod(ctx, namespace, session)
	if err != nil {
		return TaskArtifacts{}, err
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "agent" || status.State.Terminated == nil {
			continue
		}

		return parseTaskArtifacts(status.State.Terminated.Message)
	}

	return TaskArtifacts{}, fmt.Errorf("session Pod %s has no terminated agent result", pod.Name)
}

func parseTaskArtifacts(message string) (TaskArtifacts, error) {
	var result TaskArtifacts
	for line := range strings.SplitSeq(message, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		contents, err := base64.StdEncoding.DecodeString(value)
		if err != nil || !json.Valid(contents) {
			return TaskArtifacts{}, fmt.Errorf("task artifact %s is invalid", key)
		}

		switch key {
		case "outcome":
			result.Outcome = contents
		case "pull-request":
			result.PullRequest = contents
		}
	}

	if len(result.Outcome) == 0 {
		return TaskArtifacts{}, errors.New("task did not report an outcome artifact")
	}

	return result, nil
}

func (c *Client) newestSessionPod(ctx context.Context, namespace, session string) (*corev1.Pod, error) {
	pods, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sessionTaskSelector(session)})
	if err != nil {
		return nil, fmt.Errorf("list session %s Pods: %w", session, err)
	}

	if len(pods.Items) == 0 {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, session)
	}

	sort.Slice(pods.Items, func(i, j int) bool {
		if pods.Items[i].CreationTimestamp.Equal(&pods.Items[j].CreationTimestamp) {
			return pods.Items[i].Name > pods.Items[j].Name
		}

		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})

	return &pods.Items[0], nil
}

func sessionSelector(session string) string {
	return labels.Set{"coding-agent/session": session}.AsSelector().String()
}

func sessionTaskSelector(session string) string {
	return sessionSelector(session) + ",coding-agent/component!=publisher"
}

func jobResourceStatus(job *batchv1.Job) ResourceStatus {
	status := "Running"
	if job.Status.Succeeded > 0 {
		status = "Complete"
	} else if job.Status.Failed > 0 {
		status = "Failed"
	} else if job.Status.Active == 0 {
		status = "Pending"
	}

	return ResourceStatus{Kind: "Job", Name: job.Name, Ready: fmt.Sprintf("%d/1", job.Status.Succeeded), State: status}
}

func podResourceStatus(pod *corev1.Pod) ResourceStatus {
	ready := "0/1"
	if podReady(pod) {
		ready = "1/1"
	}

	return ResourceStatus{Kind: "Pod", Name: pod.Name, Ready: ready, State: string(pod.Status.Phase)}
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
