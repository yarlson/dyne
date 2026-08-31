package workload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

const fieldManagerName = "dyne"

// Runtime projects session and publisher executions into Kubernetes.
type Runtime struct {
	typed     clientset.Interface
	dynamic   dynamic.Interface
	mapper    meta.RESTMapper
	stdout    io.Writer
	namespace string
}

// Config contains Kubernetes deployment settings.
type Config struct {
	Namespace string
	Output    io.Writer
}

// NewForConfig creates a runtime using server-owned Kubernetes credentials.
func NewForConfig(restConfig *rest.Config, config Config) (*Runtime, error) {
	if restConfig == nil {
		return nil, errors.New("kubernetes configuration is required")
	}

	if !dnsLabelPattern.MatchString(config.Namespace) || len(config.Namespace) > 63 {
		return nil, errors.New("namespace must be a lowercase DNS label no longer than 63 characters")
	}

	if config.Output == nil {
		return nil, errors.New("output stream is required")
	}

	typed, err := clientset.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(typed.Discovery()))

	return &Runtime{
		typed: typed, dynamic: dynamicClient, mapper: mapper,
		stdout: config.Output, namespace: config.Namespace,
	}, nil
}

// Scope identifies the namespace owned by this runtime.
func (c *Runtime) Scope() string { return c.namespace }

func (c *Runtime) apply(ctx context.Context, manifest []byte) error {
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

func (c *Runtime) applyResource(ctx context.Context, resource *unstructured.Unstructured) error {
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
		FieldManager: fieldManagerName, Force: new(true),
	}); err != nil {
		return fmt.Errorf("apply %s %s: %w", resource.GetKind(), resource.GetName(), err)
	}

	_, _ = fmt.Fprintf(c.stdout, "%s/%s applied\n", strings.ToLower(resource.GetKind()), resource.GetName())

	return nil
}
