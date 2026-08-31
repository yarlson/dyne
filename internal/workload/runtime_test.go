package workload

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewForConfigRequiresKubernetesConfiguration(t *testing.T) {
	_, err := NewForConfig(nil, Config{})
	require.EqualError(t, err, "kubernetes configuration is required")
}
