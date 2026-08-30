package kubernetes

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestLoadConnectionConfigSelectsExplicitEKS(t *testing.T) {
	want := &rest.Config{Host: "https://eks.example"}
	loaders := connectionLoaders{
		inCluster: func() (*rest.Config, error) {
			require.FailNow(t, "loaded in-cluster configuration for explicit EKS")

			return nil, nil
		},
		kubeconfig: func(string, string) (*rest.Config, error) {
			require.FailNow(t, "loaded kubeconfig for explicit EKS")

			return nil, nil
		},
		eks: func(_ context.Context, config ConnectionConfig) (*rest.Config, error) {
			assert.Equal(t, "production", config.EKSCluster)
			assert.Equal(t, "eu-west-1", config.AWSRegion)
			assert.Equal(t, "arn:aws:iam::123456789012:role/dyne", config.AWSRoleARN)

			return want, nil
		},
	}

	got, err := loadConnectionConfig(context.Background(), ConnectionConfig{
		EKSCluster: "production",
		AWSRegion:  "eu-west-1",
		AWSRoleARN: "arn:aws:iam::123456789012:role/dyne",
	}, loaders)
	require.NoError(t, err)
	assert.Same(t, want, got)
}

func TestLoadConnectionConfigRejectsConflictingEKSAndKubeconfig(t *testing.T) {
	loaders := connectionLoaders{
		inCluster: func() (*rest.Config, error) {
			require.FailNow(t, "loaded configuration after validation failed")

			return nil, nil
		},
		kubeconfig: func(string, string) (*rest.Config, error) {
			require.FailNow(t, "loaded configuration after validation failed")

			return nil, nil
		},
		eks: func(context.Context, ConnectionConfig) (*rest.Config, error) {
			require.FailNow(t, "loaded configuration after validation failed")

			return nil, nil
		},
	}

	_, err := loadConnectionConfig(context.Background(), ConnectionConfig{
		EKSCluster:     "production",
		KubeconfigPath: "/tmp/config",
	}, loaders)
	require.EqualError(t, err, "--eks-cluster cannot be combined with --kubeconfig or --context")
}

func TestLoadConnectionConfigPrefersInClusterCredentials(t *testing.T) {
	want := &rest.Config{Host: "https://kubernetes.default.svc"}
	loaders := connectionLoaders{
		inCluster: func() (*rest.Config, error) {
			return want, nil
		},
		kubeconfig: func(string, string) (*rest.Config, error) {
			require.FailNow(t, "loaded kubeconfig after finding in-cluster credentials")

			return nil, nil
		},
		eks: func(context.Context, ConnectionConfig) (*rest.Config, error) {
			require.FailNow(t, "loaded EKS without explicit configuration")

			return nil, nil
		},
	}

	got, err := loadConnectionConfig(context.Background(), ConnectionConfig{}, loaders)
	require.NoError(t, err)
	assert.Same(t, want, got)
}

func TestLoadConnectionConfigFallsBackToStandardKubeconfig(t *testing.T) {
	want := &rest.Config{Host: "https://local.example"}
	loaders := connectionLoaders{
		inCluster: func() (*rest.Config, error) {
			return nil, rest.ErrNotInCluster
		},
		kubeconfig: func(path, contextName string) (*rest.Config, error) {
			assert.Empty(t, path)
			assert.Empty(t, contextName)

			return want, nil
		},
		eks: func(context.Context, ConnectionConfig) (*rest.Config, error) {
			require.FailNow(t, "loaded EKS without explicit configuration")

			return nil, nil
		},
	}

	got, err := loadConnectionConfig(context.Background(), ConnectionConfig{}, loaders)
	require.NoError(t, err)
	assert.Same(t, want, got)
}

func TestLoadConnectionConfigReturnsUnexpectedInClusterFailure(t *testing.T) {
	inClusterFailure := errors.New("read service account token")
	loaders := connectionLoaders{
		inCluster: func() (*rest.Config, error) {
			return nil, inClusterFailure
		},
		kubeconfig: func(string, string) (*rest.Config, error) {
			require.FailNow(t, "loaded kubeconfig after an unexpected in-cluster failure")

			return nil, nil
		},
		eks: func(context.Context, ConnectionConfig) (*rest.Config, error) {
			require.FailNow(t, "loaded EKS without explicit configuration")

			return nil, nil
		},
	}

	_, err := loadConnectionConfig(context.Background(), ConnectionConfig{}, loaders)
	require.ErrorIs(t, err, inClusterFailure)
	assert.ErrorContains(t, err, "load in-cluster Kubernetes configuration")
}
