package catalog

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedCatalogLoadsAgentsSkillsAndWorkflows(t *testing.T) {
	agents, err := LoadEngineeringAgents(AgentDefaults{StorageSize: "10Gi", TaskTimeout: time.Hour})
	require.NoError(t, err)
	summaries := agents.List()
	names := make([]string, len(summaries))
	for index, summary := range summaries {
		names[index] = summary.Name
	}

	assert.Equal(t, []string{
		"finisher",
		"implementer",
		"investigator",
		"planner",
		"security-reviewer",
		"test-reviewer",
	}, names)

	implementer, found := agents.Find("implementer")
	require.True(t, found)
	require.Len(t, implementer.Skills, 1)
	assert.Equal(t, "behavior-implement", implementer.Skills[0].Name)
	assert.Contains(t, implementer.Instructions, "## Engineering Quality Gate")

	workflows, err := LoadEngineeringWorkflows(agents)
	require.NoError(t, err)
	require.Len(t, workflows.List(), 2)
	assert.Equal(t, "engineering-change", workflows.List()[0].Name)
	assert.Equal(t, "focused-change", workflows.List()[1].Name)

	focused, found := workflows.Find("focused-change")
	require.True(t, found)
	assert.Equal(t, "implement", focused.Steps["test-review"].ChangeFrom)
	assert.True(t, focused.Steps["finalize"].Publishable)
}
