package agentconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/yarlson/airlock/internal/agent"
)

const maxAgentConfigBytes = 900 * 1024

// Defaults supplies server-owned values omitted from an agent definition.
type Defaults struct {
	// StorageSize is the default PVC capacity for persistent agents.
	StorageSize string
	// TaskTimeout is the default task duration.
	TaskTimeout time.Duration
}

// Catalog provides immutable agent definitions loaded at server startup.
type Catalog struct {
	definitions map[string]agent.AgentDefinition
}

type catalogFile struct {
	Version string                     `json:"version"`
	Agents  map[string]agentDefinition `json:"agents"`
}

type agentDefinition struct {
	Description  string   `json:"description"`
	Storage      string   `json:"storage"`
	Instructions string   `json:"instructions"`
	Skills       []string `json:"skills"`
	SetupCommand string   `json:"setup"`
	CloneDepth   *int     `json:"clone_depth"`
	StorageSize  string   `json:"storage_size"`
	Timeout      string   `json:"timeout"`
}

type skillMetadata struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Load reads and validates one complete agent catalog.
func Load(path string, defaults Defaults) (*Catalog, error) {
	if err := validateDefaults(defaults); err != nil {
		return nil, err
	}

	contents, err := readBoundedFile(path, maxAgentConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read agents file: %w", err)
	}

	var source catalogFile
	if err := yaml.UnmarshalStrict(contents, &source); err != nil {
		return nil, fmt.Errorf("decode agents file: %w", err)
	}

	if source.Version != "v1" {
		return nil, fmt.Errorf("unsupported agents file version %q", source.Version)
	}

	if len(source.Agents) == 0 {
		return nil, errors.New("agents file must define at least one agent")
	}

	directory, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve agents file directory: %w", err)
	}

	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve agents file directory: %w", err)
	}

	definitions := make(map[string]agent.AgentDefinition, len(source.Agents))
	for name, raw := range source.Agents {
		definition, err := loadDefinition(directory, name, raw, defaults)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", name, err)
		}

		definitions[name] = definition
	}

	return &Catalog{definitions: definitions}, nil
}

// List returns safe agent summaries sorted by name.
func (c *Catalog) List() []agent.AgentSummary {
	if c == nil {
		return []agent.AgentSummary{}
	}

	summaries := make([]agent.AgentSummary, 0, len(c.definitions))
	for _, definition := range c.definitions {
		skillNames := make([]string, len(definition.Skills))
		for i, skill := range definition.Skills {
			skillNames[i] = skill.Name
		}

		slices.Sort(skillNames)
		summaries = append(summaries, agent.AgentSummary{
			Name: definition.Name, Description: definition.Description, Storage: definition.Storage, Skills: skillNames,
		})
	}

	slices.SortFunc(summaries, func(left, right agent.AgentSummary) int {
		return strings.Compare(left.Name, right.Name)
	})

	return summaries
}

// Find returns one agent definition by name.
func (c *Catalog) Find(name string) (agent.AgentDefinition, bool) {
	if c == nil {
		return agent.AgentDefinition{}, false
	}

	definition, found := c.definitions[name]
	definition.Skills = slices.Clone(definition.Skills)

	return definition, found
}

func loadDefinition(directory, name string, raw agentDefinition, defaults Defaults) (agent.AgentDefinition, error) {
	if messages := validation.IsDNS1123Label(name); len(messages) > 0 {
		return agent.AgentDefinition{}, fmt.Errorf("name must be a lowercase DNS label: %s", strings.Join(messages, ", "))
	}

	if strings.TrimSpace(raw.Description) == "" {
		return agent.AgentDefinition{}, errors.New("description is required")
	}

	if strings.TrimSpace(raw.Instructions) == "" {
		return agent.AgentDefinition{}, errors.New("instructions are required")
	}

	if raw.Storage != "ephemeral" && raw.Storage != "persistent" {
		return agent.AgentDefinition{}, fmt.Errorf("unsupported storage %q", raw.Storage)
	}

	if raw.Storage == "ephemeral" && raw.StorageSize != "" {
		return agent.AgentDefinition{}, errors.New("storage_size is only valid for persistent agents")
	}

	cloneDepth := 1
	if raw.CloneDepth != nil {
		cloneDepth = *raw.CloneDepth
	}

	if cloneDepth < 0 {
		return agent.AgentDefinition{}, errors.New("clone_depth cannot be negative")
	}

	storageSize := raw.StorageSize
	if storageSize == "" {
		storageSize = defaults.StorageSize
	}

	if _, err := resource.ParseQuantity(storageSize); err != nil {
		return agent.AgentDefinition{}, fmt.Errorf("parse storage_size: %w", err)
	}

	timeout := defaults.TaskTimeout
	if raw.Timeout != "" {
		parsed, err := time.ParseDuration(raw.Timeout)
		if err != nil {
			return agent.AgentDefinition{}, fmt.Errorf("parse timeout: %w", err)
		}

		timeout = parsed
	}

	if timeout <= 0 {
		return agent.AgentDefinition{}, errors.New("timeout must be greater than zero")
	}

	skills, err := loadSkills(directory, raw.Skills)
	if err != nil {
		return agent.AgentDefinition{}, err
	}

	size := len(raw.Instructions)
	for _, skill := range skills {
		size += len(skill.Name) + len(skill.Contents)
	}

	if size > maxAgentConfigBytes {
		return agent.AgentDefinition{}, fmt.Errorf("instructions and skills exceed %d bytes", maxAgentConfigBytes)
	}

	return agent.AgentDefinition{
		Name:         name,
		Description:  raw.Description,
		Storage:      agent.Storage(raw.Storage),
		Instructions: raw.Instructions,
		Skills:       skills,
		SetupCommand: raw.SetupCommand,
		CloneDepth:   cloneDepth,
		StorageSize:  storageSize,
		Timeout:      timeout,
	}, nil
}

func validateDefaults(defaults Defaults) error {
	if _, err := resource.ParseQuantity(defaults.StorageSize); err != nil {
		return fmt.Errorf("parse default storage size: %w", err)
	}

	if defaults.TaskTimeout <= 0 {
		return errors.New("default task timeout must be greater than zero")
	}

	return nil
}

func loadSkills(directory string, paths []string) ([]agent.AgentSkill, error) {
	skills := make([]agent.AgentSkill, 0, len(paths))
	names := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		skill, err := loadSkill(directory, path)
		if err != nil {
			return nil, err
		}

		if _, exists := names[skill.Name]; exists {
			return nil, fmt.Errorf("skill name %q is duplicated", skill.Name)
		}

		names[skill.Name] = struct{}{}
		skills = append(skills, skill)
	}

	return skills, nil
}

func loadSkill(directory, relativePath string) (agent.AgentSkill, error) {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return agent.AgentSkill{}, fmt.Errorf("skill path %q must be relative", relativePath)
	}

	cleanPath := filepath.Clean(relativePath)
	if cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return agent.AgentSkill{}, fmt.Errorf("skill path %q must stay within the agents file directory", relativePath)
	}

	if filepath.Base(cleanPath) != "SKILL.md" {
		return agent.AgentSkill{}, fmt.Errorf("skill path %q must identify SKILL.md", relativePath)
	}

	path := filepath.Join(directory, cleanPath)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return agent.AgentSkill{}, fmt.Errorf("resolve skill path %q: %w", relativePath, err)
	}

	absoluteResolved, err := filepath.Abs(resolved)
	if err != nil {
		return agent.AgentSkill{}, fmt.Errorf("resolve skill path %q: %w", relativePath, err)
	}

	if absoluteResolved != path {
		return agent.AgentSkill{}, fmt.Errorf("skill path %q must not contain symlinks", relativePath)
	}

	info, err := os.Stat(path)
	if err != nil {
		return agent.AgentSkill{}, fmt.Errorf("stat skill path %q: %w", relativePath, err)
	}

	if !info.Mode().IsRegular() {
		return agent.AgentSkill{}, fmt.Errorf("skill path %q must be a regular file", relativePath)
	}

	contents, err := readBoundedFile(path, maxAgentConfigBytes)
	if err != nil {
		return agent.AgentSkill{}, fmt.Errorf("read skill %q: %w", relativePath, err)
	}

	metadata, err := parseSkillMetadata(contents)
	if err != nil {
		return agent.AgentSkill{}, fmt.Errorf("parse skill %q: %w", relativePath, err)
	}

	return agent.AgentSkill{Name: metadata.Name, Contents: string(contents)}, nil
}

func parseSkillMetadata(contents []byte) (skillMetadata, error) {
	lines := strings.Split(string(contents), "\n")
	if len(lines) < 3 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return skillMetadata{}, errors.New("SKILL.md must start with YAML frontmatter")
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			end = i

			break
		}
	}

	if end < 0 {
		return skillMetadata{}, errors.New("SKILL.md frontmatter is not closed")
	}

	var metadata skillMetadata
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &metadata); err != nil {
		return skillMetadata{}, err
	}

	if messages := validation.IsDNS1123Label(metadata.Name); len(messages) > 0 {
		return skillMetadata{}, fmt.Errorf("skill name must be a lowercase DNS label: %s", strings.Join(messages, ", "))
	}

	if strings.TrimSpace(metadata.Description) == "" {
		return skillMetadata{}, errors.New("skill description is required")
	}

	return metadata, nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if info.Size() > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}

	return os.ReadFile(path)
}
