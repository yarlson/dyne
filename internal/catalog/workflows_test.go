package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yarlson/dyne/internal/agent"
)

type agentCatalog map[string]agent.AgentDefinition

func (c agentCatalog) List() []agent.AgentSummary { return nil }

func (c agentCatalog) Find(name string) (agent.AgentDefinition, bool) {
	definition, found := c[name]

	return definition, found
}

func TestLoadWorkflowsResolvesFanOutAndPublishableLeaf(t *testing.T) {
	path := writeWorkflows(t, `version: v1
workflows:
  delivery:
    description: Review and implement one change.
    max_parallelism: 2
    steps:
      security:
        agent: reviewer
        prompt: Review the trust boundary.
      tests:
        agent: reviewer
        prompt: Review the test gaps.
      implement:
        agent: implementer
        prompt: Implement the approved change.
        after: [security, tests]
        publishable: true
`)
	agents := agentCatalog{
		"reviewer":    {Name: "reviewer", Storage: agent.StorageEphemeral, Timeout: time.Hour},
		"implementer": {Name: "implementer", Storage: agent.StoragePersistent, Timeout: 2 * time.Hour},
	}

	catalog, err := LoadWorkflows(path, agents)
	require.NoError(t, err)

	assert.Equal(t, 2, catalog.List()[0].MaxParallelism)
	definition, found := catalog.Find("delivery")
	require.True(t, found)
	assert.Equal(t, []string{"security", "tests"}, definition.Steps["implement"].After)
	assert.Equal(t, 2*time.Hour, definition.Steps["implement"].SessionDefinition.Timeout)
}

func TestLoadWorkflowsAllowsPersistentChangeProducerForDirectDependents(t *testing.T) {
	path := writeWorkflows(t, `version: v1
workflows:
  delivery:
    description: Implement, review, and finalize one change.
    steps:
      implement:
        agent: implementer
        prompt: Implement the change.
      review:
        agent: reviewer
        prompt: Review the implemented change.
        after: [implement]
        change_from: implement
      finalize:
        agent: implementer
        prompt: Address review findings and validate the change.
        after: [implement, review]
        change_from: implement
        publishable: true
`)
	agents := agentCatalog{
		"reviewer":    {Name: "reviewer", Storage: agent.StorageEphemeral},
		"implementer": {Name: "implementer", Storage: agent.StoragePersistent},
	}

	_, err := LoadWorkflows(path, agents)
	require.NoError(t, err)
}

func TestLoadWorkflowsDefaultsMaxParallelismToOne(t *testing.T) {
	path := writeWorkflows(t, `version: v1
workflows:
  review:
    description: Review one change.
    steps:
      inspect:
        agent: reviewer
        prompt: Review the change.
`)

	catalog, err := LoadWorkflows(path, agentCatalog{"reviewer": {Name: "reviewer", Storage: agent.StorageEphemeral}})
	require.NoError(t, err)

	definition, found := catalog.Find("review")
	require.True(t, found)
	assert.Equal(t, 1, definition.MaxParallelism)
}

func TestLoadWorkflowsRejectsInvalidWorkflowGraphs(t *testing.T) {
	tests := []struct {
		name    string
		steps   string
		message string
	}{
		{name: "unknown dependency", steps: `      inspect:
        agent: reviewer
        prompt: Review.
        after: [missing]
`, message: "unknown step missing"},
		{name: "cycle", steps: `      first:
        agent: reviewer
        prompt: First.
        after: [second]
      second:
        agent: reviewer
        prompt: Second.
        after: [first]
`, message: "contains a cycle"},
		{name: "publishable non-leaf", steps: `      implement:
        agent: implementer
        prompt: Implement.
        publishable: true
      inspect:
        agent: reviewer
        prompt: Inspect.
        after: [implement]
`, message: "must be a leaf"},
		{name: "persistent finding", steps: `      inspect:
        agent: implementer
        prompt: Inspect.
`, message: "non-publishable step requires an ephemeral agent"},
		{name: "change source is not a direct dependency", steps: `      implement:
        agent: implementer
        prompt: Implement.
      inspect:
        agent: reviewer
        prompt: Inspect.
        change_from: implement
`, message: "must be a direct dependency"},
		{name: "change producer is ephemeral", steps: `      implement:
        agent: reviewer
        prompt: Implement.
      inspect:
        agent: reviewer
        prompt: Inspect.
        after: [implement]
        change_from: implement
`, message: "step implement requires a persistent agent"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeWorkflows(t, "version: v1\nworkflows:\n  delivery:\n    description: Delivery.\n    steps:\n"+test.steps)
			agents := agentCatalog{
				"reviewer":    {Name: "reviewer", Storage: agent.StorageEphemeral},
				"implementer": {Name: "implementer", Storage: agent.StoragePersistent},
			}

			_, err := LoadWorkflows(path, agents)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func TestLoadWorkflowsRejectsUnknownFields(t *testing.T) {
	path := writeWorkflows(t, `version: v1
workflows:
  review:
    description: Review one change.
    unknown: value
    steps:
      inspect:
        agent: reviewer
        prompt: Review.
`)

	_, err := LoadWorkflows(path, agentCatalog{"reviewer": {Name: "reviewer", Storage: agent.StorageEphemeral}})
	require.ErrorContains(t, err, "unknown")
}

func writeWorkflows(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workflows.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	return path
}
