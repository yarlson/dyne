package kubernetes

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ConnectionConfig selects the Kubernetes cluster managed by the server.
type ConnectionConfig struct {
	// KubeconfigPath loads one explicit kubeconfig file outside a cluster.
	KubeconfigPath string
	// ContextName selects a context from the loaded kubeconfig.
	ContextName string
	// EKSCluster selects an Amazon EKS cluster instead of in-cluster or kubeconfig credentials.
	EKSCluster string
	// AWSRegion selects the AWS region containing the EKS cluster.
	AWSRegion string
	// AWSRoleARN is assumed before the server connects to EKS.
	AWSRoleARN string
}

type connectionLoaders struct {
	inCluster  func() (*rest.Config, error)
	kubeconfig func(string, string) (*rest.Config, error)
	eks        func(context.Context, ConnectionConfig) (*rest.Config, error)
}

// LoadConnectionConfig returns the server-owned Kubernetes configuration.
func LoadConnectionConfig(ctx context.Context, config ConnectionConfig) (*rest.Config, error) {
	return loadConnectionConfig(ctx, config, connectionLoaders{
		inCluster:  rest.InClusterConfig,
		kubeconfig: loadKubeconfig,
		eks:        loadEKSConnectionConfig,
	})
}

func loadConnectionConfig(ctx context.Context, config ConnectionConfig, loaders connectionLoaders) (*rest.Config, error) {
	if config.EKSCluster != "" {
		if config.KubeconfigPath != "" || config.ContextName != "" {
			return nil, errors.New("--eks-cluster cannot be combined with --kubeconfig or --context")
		}

		return loaders.eks(ctx, config)
	}

	if config.KubeconfigPath != "" || config.ContextName != "" {
		return loaders.kubeconfig(config.KubeconfigPath, config.ContextName)
	}

	inClusterConfig, err := loaders.inCluster()
	if err == nil {
		return inClusterConfig, nil
	}

	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("load in-cluster Kubernetes configuration: %w", err)
	}

	return loaders.kubeconfig("", "")
}

func loadKubeconfig(path, contextName string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		loadingRules.ExplicitPath = path
	}

	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
	}

	return config, nil
}
