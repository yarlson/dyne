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

func TestLoadAgentsResolvesAgentDefinitionsAndSkillFiles(t *testing.T) {
	directory := t.TempDir()
	skillDirectory := filepath.Join(directory, "skills", "code-review")
	require.NoError(t, os.MkdirAll(skillDirectory, 0o755))
	skillContents := "---\nname: code-review\ndescription: Review changed code.\n---\n\nReview correctness and tests.\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDirectory, "SKILL.md"), []byte(skillContents), 0o600))
	catalogContents := `version: v1
agents:
  reviewer:
    description: Reviews repository changes.
    storage: ephemeral
    instructions: Review correctness, security, and tests.
    skills:
      - skills/code-review/SKILL.md
  implementer:
    description: Implements focused changes.
    storage: persistent
    instructions: Implement the smallest safe change.
    setup: mise install
    clone_depth: 0
    storage_size: 20Gi
    timeout: 4h
`
	path := filepath.Join(directory, "agents.yaml")
	require.NoError(t, os.WriteFile(path, []byte(catalogContents), 0o600))

	catalog, err := LoadAgents(path, AgentDefaults{StorageSize: "10Gi", TaskTimeout: 2 * time.Hour})
	require.NoError(t, err)

	assert.Equal(t, []agent.AgentSummary{
		{Name: "implementer", Description: "Implements focused changes.", Storage: "persistent", Skills: []string{}},
		{Name: "reviewer", Description: "Reviews repository changes.", Storage: "ephemeral", Skills: []string{"code-review"}},
	}, catalog.List())

	reviewer, found := catalog.Find("reviewer")
	require.True(t, found)
	assert.Equal(t, agent.AgentDefinition{
		Name:         "reviewer",
		Description:  "Reviews repository changes.",
		Storage:      "ephemeral",
		Instructions: "Review correctness, security, and tests.",
		Skills:       []agent.AgentSkill{{Name: "code-review", Contents: skillContents}},
		CloneDepth:   1,
		StorageSize:  "10Gi",
		Timeout:      2 * time.Hour,
	}, reviewer)

	implementer, found := catalog.Find("implementer")
	require.True(t, found)
	assert.Equal(t, 0, implementer.CloneDepth)
	assert.Equal(t, "20Gi", implementer.StorageSize)
	assert.Equal(t, 4*time.Hour, implementer.Timeout)
	assert.Equal(t, "mise install", implementer.SetupCommand)
}

func TestLoadAgentsPrependsSharedGuidanceToEveryAgent(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "AGENTS.md"), []byte("Keep changes small.\n"), 0o600))
	path := filepath.Join(directory, "agents.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: v1
guidance: AGENTS.md
agents:
  reviewer:
    description: Reviews changes.
    storage: ephemeral
    instructions: Stay read-only.
`), 0o600))

	catalog, err := LoadAgents(path, AgentDefaults{StorageSize: "10Gi", TaskTimeout: time.Hour})
	require.NoError(t, err)
	reviewer, found := catalog.Find("reviewer")
	require.True(t, found)
	assert.Equal(t, "Keep changes small.\n\nStay read-only.", reviewer.Instructions)
}

func TestNilAgentsBehavesAsEmpty(t *testing.T) {
	var catalog *Agents
	assert.Equal(t, []agent.AgentSummary{}, catalog.List())
	_, found := catalog.Find("missing")
	assert.False(t, found)
}

func TestLoadAgentsRejectsUnknownFields(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "agents.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: v1
agents:
  reviewer:
    description: Reviews repository changes.
    storage: ephemeral
    instructions: Review the repository.
    unknown: value
`), 0o600))

	_, err := LoadAgents(path, AgentDefaults{StorageSize: "10Gi", TaskTimeout: time.Hour})
	require.ErrorContains(t, err, "unknown")
}

func TestLoadAgentsRejectsSkillOutsideCatalogDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "catalog")
	require.NoError(t, os.MkdirAll(directory, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: escaped\ndescription: Escaped skill.\n---\n"), 0o600))
	path := filepath.Join(directory, "agents.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: v1
agents:
  reviewer:
    description: Reviews repository changes.
    storage: ephemeral
    instructions: Review the repository.
    skills:
      - ../SKILL.md
`), 0o600))

	_, err := LoadAgents(path, AgentDefaults{StorageSize: "10Gi", TaskTimeout: time.Hour})
	require.ErrorContains(t, err, "must stay within")
}

func TestLoadAgentsRejectsGuidanceOutsideCatalogDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "catalog")
	require.NoError(t, os.MkdirAll(directory, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Escaped guidance.\n"), 0o600))
	path := filepath.Join(directory, "agents.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: v1
guidance: ../AGENTS.md
agents:
  reviewer:
    description: Reviews repository changes.
    storage: ephemeral
    instructions: Review the repository.
`), 0o600))

	_, err := LoadAgents(path, AgentDefaults{StorageSize: "10Gi", TaskTimeout: time.Hour})
	require.ErrorContains(t, err, "must stay within")
}

func TestLoadAgentsRejectsSymlinkedSkill(t *testing.T) {
	directory := t.TempDir()
	skillDirectory := filepath.Join(directory, "skills", "linked")
	require.NoError(t, os.MkdirAll(skillDirectory, 0o755))
	target := filepath.Join(directory, "skill-source.md")
	require.NoError(t, os.WriteFile(target, []byte("---\nname: linked\ndescription: Linked skill.\n---\n"), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(skillDirectory, "SKILL.md")))
	path := filepath.Join(directory, "agents.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: v1
agents:
  reviewer:
    description: Reviews repository changes.
    storage: ephemeral
    instructions: Review the repository.
    skills:
      - skills/linked/SKILL.md
`), 0o600))

	_, err := LoadAgents(path, AgentDefaults{StorageSize: "10Gi", TaskTimeout: time.Hour})
	require.ErrorContains(t, err, "must not contain symlinks")
}

func TestLoadAgentsRejectsInvalidAgentDefinitions(t *testing.T) {
	tests := []struct {
		name       string
		definition string
		message    string
	}{
		{
			name: "missing instructions",
			definition: `    description: Reviews repository changes.
    storage: ephemeral
`,
			message: "instructions are required",
		},
		{
			name: "unsupported storage",
			definition: `    description: Reviews repository changes.
    storage: shared
    instructions: Review the repository.
`,
			message: "unsupported storage",
		},
		{
			name: "ephemeral storage size",
			definition: `    description: Reviews repository changes.
    storage: ephemeral
    storage_size: 20Gi
    instructions: Review the repository.
`,
			message: "storage_size is only valid",
		},
		{
			name: "negative clone depth",
			definition: `    description: Reviews repository changes.
    storage: persistent
    instructions: Review the repository.
    clone_depth: -1
`,
			message: "clone_depth cannot be negative",
		},
		{
			name: "invalid timeout",
			definition: `    description: Reviews repository changes.
    storage: persistent
    instructions: Review the repository.
    timeout: later
`,
			message: "parse timeout",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.yaml")
			contents := "version: v1\nagents:\n  reviewer:\n" + test.definition
			require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

			_, err := LoadAgents(path, AgentDefaults{StorageSize: "10Gi", TaskTimeout: time.Hour})
			require.ErrorContains(t, err, test.message)
		})
	}
}
