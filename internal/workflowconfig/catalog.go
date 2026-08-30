package workflowconfig

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/yarlson/dyne/internal/agent"
	"github.com/yarlson/dyne/internal/workflow"
)

const (
	maxWorkflowConfigBytes = 900 * 1024
	maxWorkflowSteps       = 16
	maxParallelism         = 8
)

// Catalog provides immutable workflow definitions loaded at server startup.
type Catalog struct {
	definitions map[string]workflow.Definition
}

type catalogFile struct {
	Version   string                    `json:"version"`
	Workflows map[string]definitionFile `json:"workflows"`
}

type definitionFile struct {
	Description    string              `json:"description"`
	MaxParallelism int                 `json:"max_parallelism"`
	Steps          map[string]stepFile `json:"steps"`
}

type stepFile struct {
	Agent       string   `json:"agent"`
	Prompt      string   `json:"prompt"`
	After       []string `json:"after"`
	Publishable bool     `json:"publishable"`
}

// Load reads and validates one complete workflow catalog.
func Load(path string, agents agent.AgentCatalog) (*Catalog, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflows file: %w", err)
	}

	if len(contents) > maxWorkflowConfigBytes {
		return nil, fmt.Errorf("workflows file exceeds %d bytes", maxWorkflowConfigBytes)
	}

	var source catalogFile
	if err := yaml.UnmarshalStrict(contents, &source); err != nil {
		return nil, fmt.Errorf("decode workflows file: %w", err)
	}

	if source.Version != "v1" {
		return nil, fmt.Errorf("unsupported workflows file version %q", source.Version)
	}

	if len(source.Workflows) == 0 {
		return nil, errors.New("workflows file must define at least one workflow")
	}

	definitions := make(map[string]workflow.Definition, len(source.Workflows))
	for name, raw := range source.Workflows {
		definition, err := loadDefinition(name, raw, agents)
		if err != nil {
			return nil, fmt.Errorf("workflow %s: %w", name, err)
		}

		definitions[name] = definition
	}

	return &Catalog{definitions: definitions}, nil
}

// List returns safe workflow summaries sorted by name.
func (c *Catalog) List() []workflow.Summary {
	if c == nil {
		return []workflow.Summary{}
	}

	summaries := make([]workflow.Summary, 0, len(c.definitions))
	for _, definition := range c.definitions {
		summaries = append(summaries, workflow.Summary{
			Name: definition.Name, Description: definition.Description,
			MaxParallelism: definition.MaxParallelism, Steps: len(definition.Steps),
		})
	}

	slices.SortFunc(summaries, func(left, right workflow.Summary) int {
		return strings.Compare(left.Name, right.Name)
	})

	return summaries
}

// Find returns one workflow definition by name.
func (c *Catalog) Find(name string) (workflow.Definition, bool) {
	if c == nil {
		return workflow.Definition{}, false
	}

	definition, found := c.definitions[name]

	return workflow.CloneDefinition(definition), found
}

func loadDefinition(name string, raw definitionFile, agents agent.AgentCatalog) (workflow.Definition, error) {
	if messages := validation.IsDNS1123Label(name); len(messages) > 0 {
		return workflow.Definition{}, fmt.Errorf("name must be a lowercase DNS label: %s", strings.Join(messages, ", "))
	}

	if strings.TrimSpace(raw.Description) == "" {
		return workflow.Definition{}, errors.New("description is required")
	}

	parallelism := raw.MaxParallelism
	if parallelism == 0 {
		parallelism = 1
	}

	if parallelism < 1 || parallelism > maxParallelism {
		return workflow.Definition{}, fmt.Errorf("max_parallelism must be between 1 and %d", maxParallelism)
	}

	if len(raw.Steps) == 0 || len(raw.Steps) > maxWorkflowSteps {
		return workflow.Definition{}, fmt.Errorf("steps must contain between 1 and %d entries", maxWorkflowSteps)
	}

	steps := make(map[string]workflow.StepDefinition, len(raw.Steps))
	publishable := ""
	for stepName, rawStep := range raw.Steps {
		step, err := loadStep(stepName, rawStep, agents)
		if err != nil {
			return workflow.Definition{}, fmt.Errorf("step %s: %w", stepName, err)
		}

		if step.Publishable {
			if publishable != "" {
				return workflow.Definition{}, errors.New("only one step may be publishable")
			}

			publishable = stepName
		}

		steps[stepName] = step
	}

	if err := validateGraph(steps); err != nil {
		return workflow.Definition{}, err
	}

	if publishable != "" {
		for _, step := range steps {
			if slices.Contains(step.After, publishable) {
				return workflow.Definition{}, fmt.Errorf("publishable step %s must be a leaf", publishable)
			}
		}
	}

	return workflow.Definition{
		Name: name, Description: raw.Description, MaxParallelism: parallelism, Steps: steps,
	}, nil
}

func loadStep(name string, raw stepFile, agents agent.AgentCatalog) (workflow.StepDefinition, error) {
	if messages := validation.IsDNS1123Label(name); len(messages) > 0 {
		return workflow.StepDefinition{}, fmt.Errorf("name must be a lowercase DNS label: %s", strings.Join(messages, ", "))
	}

	if strings.TrimSpace(raw.Prompt) == "" {
		return workflow.StepDefinition{}, errors.New("prompt is required")
	}

	if agents == nil {
		return workflow.StepDefinition{}, fmt.Errorf("agent %s is not configured", raw.Agent)
	}

	definition, found := agents.Find(raw.Agent)
	if !found {
		return workflow.StepDefinition{}, fmt.Errorf("agent %s is not configured", raw.Agent)
	}

	if raw.Publishable && definition.Storage != agent.StoragePersistent {
		return workflow.StepDefinition{}, errors.New("publishable step requires a persistent agent")
	}

	if !raw.Publishable && definition.Storage != agent.StorageEphemeral {
		return workflow.StepDefinition{}, errors.New("non-publishable step requires an ephemeral agent")
	}

	dependencies := make(map[string]struct{}, len(raw.After))
	for _, dependency := range raw.After {
		if _, exists := dependencies[dependency]; exists {
			return workflow.StepDefinition{}, fmt.Errorf("dependency %s is duplicated", dependency)
		}

		dependencies[dependency] = struct{}{}
	}

	return workflow.StepDefinition{
		Name: name, Agent: raw.Agent, Prompt: raw.Prompt, After: slices.Clone(raw.After),
		Publishable: raw.Publishable, SessionDefinition: definition.SessionDefinition(),
	}, nil
}

func validateGraph(steps map[string]workflow.StepDefinition) error {
	for name, step := range steps {
		for _, dependency := range step.After {
			if dependency == name {
				return fmt.Errorf("step %s cannot depend on itself", name)
			}

			if _, found := steps[dependency]; !found {
				return fmt.Errorf("step %s depends on unknown step %s", name, dependency)
			}
		}
	}

	states := make(map[string]uint8, len(steps))
	var visit func(string) error
	visit = func(name string) error {
		switch states[name] {
		case 1:
			return fmt.Errorf("workflow contains a cycle through step %s", name)
		case 2:
			return nil
		}

		states[name] = 1
		for _, dependency := range steps[name].After {
			if err := visit(dependency); err != nil {
				return err
			}
		}

		states[name] = 2

		return nil
	}

	for name := range steps {
		if err := visit(name); err != nil {
			return err
		}
	}

	return nil
}
