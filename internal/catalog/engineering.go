package catalog

import (
	"embed"
	"fmt"

	"github.com/yarlson/dyne/internal/agent"
)

//go:embed engineering/AGENTS.md engineering/agents.yaml engineering/workflows.yaml engineering/skills
var engineeringFiles embed.FS

// LoadEngineeringAgents loads Dyne's built-in engineering agents and skills.
func LoadEngineeringAgents(defaults AgentDefaults) (*Agents, error) {
	agents, err := loadAgentsFS(engineeringFiles, "engineering/agents.yaml", defaults)
	if err != nil {
		return nil, fmt.Errorf("load built-in agents: %w", err)
	}

	return agents, nil
}

// LoadEngineeringWorkflows loads Dyne's built-in engineering workflows against the supplied agents.
func LoadEngineeringWorkflows(agents agent.AgentCatalog) (*Workflows, error) {
	workflows, err := loadWorkflowsFS(engineeringFiles, "engineering/workflows.yaml", agents)
	if err != nil {
		return nil, fmt.Errorf("load built-in workflows: %w", err)
	}

	return workflows, nil
}
