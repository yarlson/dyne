package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/yarlson/dyne/internal/agent"
)

const maxAgentConfigBytes = 900 * 1024

// AgentDefaults supplies server-owned values omitted from an agent definition.
type AgentDefaults struct {
	// StorageSize is the default PVC capacity for persistent agents.
	StorageSize string
	// TaskTimeout is the default task duration.
	TaskTimeout time.Duration
}

// Agents provides immutable agent definitions loaded at server startup.
type Agents struct {
	definitions map[string]agent.AgentDefinition
}

type agentCatalogFile struct {
	Version  string                     `json:"version"`
	Guidance string                     `json:"guidance"`
	Agents   map[string]agentDefinition `json:"agents"`
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

type catalogReader func(relativePath, kind string) ([]byte, string, error)

// LoadAgents reads and validates one complete agent catalog.
func LoadAgents(path string, defaults AgentDefaults) (*Agents, error) {
	if err := validateDefaults(defaults); err != nil {
		return nil, err
	}

	contents, err := readBoundedFile(path, maxAgentConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read agents file: %w", err)
	}

	source, err := decodeAgentCatalog(contents)
	if err != nil {
		return nil, err
	}

	directory, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve agents file directory: %w", err)
	}

	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve agents file directory: %w", err)
	}

	reader := func(relativePath, kind string) ([]byte, string, error) {
		filePath, cleanPath, err := catalogFilePath(directory, relativePath, kind)
		if err != nil {
			return nil, "", err
		}

		contents, err := readBoundedFile(filePath, maxAgentConfigBytes)
		if err != nil {
			return nil, "", fmt.Errorf("read %s %q: %w", kind, relativePath, err)
		}

		return contents, cleanPath, nil
	}

	return loadAgentCatalog(source, reader, defaults)
}

func loadAgentsFS(files fs.FS, name string, defaults AgentDefaults) (*Agents, error) {
	if err := validateDefaults(defaults); err != nil {
		return nil, err
	}

	contents, err := readBoundedFSFile(files, name, maxAgentConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read agents file: %w", err)
	}

	source, err := decodeAgentCatalog(contents)
	if err != nil {
		return nil, err
	}

	directory := path.Dir(name)
	reader := func(relativePath, kind string) ([]byte, string, error) {
		return readCatalogFSFile(files, directory, relativePath, kind)
	}

	return loadAgentCatalog(source, reader, defaults)
}

func decodeAgentCatalog(contents []byte) (agentCatalogFile, error) {
	var source agentCatalogFile
	if err := yaml.UnmarshalStrict(contents, &source); err != nil {
		return agentCatalogFile{}, fmt.Errorf("decode agents file: %w", err)
	}

	if source.Version != "v1" {
		return agentCatalogFile{}, fmt.Errorf("unsupported agents file version %q", source.Version)
	}

	if len(source.Agents) == 0 {
		return agentCatalogFile{}, errors.New("agents file must define at least one agent")
	}

	return source, nil
}

func loadAgentCatalog(source agentCatalogFile, reader catalogReader, defaults AgentDefaults) (*Agents, error) {
	guidance, err := loadGuidance(reader, source.Guidance)
	if err != nil {
		return nil, err
	}

	definitions := make(map[string]agent.AgentDefinition, len(source.Agents))
	for name, raw := range source.Agents {
		definition, err := loadAgentDefinition(reader, name, raw, guidance, defaults)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", name, err)
		}

		definitions[name] = definition
	}

	return &Agents{definitions: definitions}, nil
}

// List returns safe agent summaries sorted by name.
func (c *Agents) List() []agent.AgentSummary {
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
func (c *Agents) Find(name string) (agent.AgentDefinition, bool) {
	if c == nil {
		return agent.AgentDefinition{}, false
	}

	definition, found := c.definitions[name]
	definition.Skills = slices.Clone(definition.Skills)

	return definition, found
}

func loadAgentDefinition(reader catalogReader, name string, raw agentDefinition, guidance string, defaults AgentDefaults) (agent.AgentDefinition, error) {
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

	skills, err := loadSkills(reader, raw.Skills)
	if err != nil {
		return agent.AgentDefinition{}, err
	}

	instructions := raw.Instructions
	if guidance != "" {
		instructions = strings.TrimSpace(guidance) + "\n\n" + strings.TrimSpace(instructions)
	}

	size := len(instructions)
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
		Instructions: instructions,
		Skills:       skills,
		SetupCommand: raw.SetupCommand,
		CloneDepth:   cloneDepth,
		StorageSize:  storageSize,
		Timeout:      timeout,
	}, nil
}

func validateDefaults(defaults AgentDefaults) error {
	if _, err := resource.ParseQuantity(defaults.StorageSize); err != nil {
		return fmt.Errorf("parse default storage size: %w", err)
	}

	if defaults.TaskTimeout <= 0 {
		return errors.New("default task timeout must be greater than zero")
	}

	return nil
}

func loadSkills(reader catalogReader, paths []string) ([]agent.AgentSkill, error) {
	skills := make([]agent.AgentSkill, 0, len(paths))
	names := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		skill, err := loadSkill(reader, path)
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

func loadSkill(reader catalogReader, relativePath string) (agent.AgentSkill, error) {
	contents, cleanPath, err := reader(relativePath, "skill")
	if err != nil {
		return agent.AgentSkill{}, err
	}

	if filepath.Base(cleanPath) != "SKILL.md" {
		return agent.AgentSkill{}, fmt.Errorf("skill path %q must identify SKILL.md", relativePath)
	}

	metadata, err := parseSkillMetadata(contents)
	if err != nil {
		return agent.AgentSkill{}, fmt.Errorf("parse skill %q: %w", relativePath, err)
	}

	return agent.AgentSkill{Name: metadata.Name, Contents: string(contents)}, nil
}

func loadGuidance(reader catalogReader, relativePath string) (string, error) {
	if relativePath == "" {
		return "", nil
	}

	contents, cleanPath, err := reader(relativePath, "guidance")
	if err != nil {
		return "", err
	}

	if filepath.Base(cleanPath) != "AGENTS.md" {
		return "", fmt.Errorf("guidance path %q must identify AGENTS.md", relativePath)
	}

	if strings.TrimSpace(string(contents)) == "" {
		return "", fmt.Errorf("guidance %q is empty", relativePath)
	}

	return string(contents), nil
}

func catalogFilePath(directory, relativePath, kind string) (string, string, error) {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return "", "", fmt.Errorf("%s path %q must be relative", kind, relativePath)
	}

	cleanPath := filepath.Clean(relativePath)
	if cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%s path %q must stay within the agents file directory", kind, relativePath)
	}

	path := filepath.Join(directory, cleanPath)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s path %q: %w", kind, relativePath, err)
	}

	absoluteResolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s path %q: %w", kind, relativePath, err)
	}

	if absoluteResolved != path {
		return "", "", fmt.Errorf("%s path %q must not contain symlinks", kind, relativePath)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("stat %s path %q: %w", kind, relativePath, err)
	}

	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("%s path %q must be a regular file", kind, relativePath)
	}

	return path, cleanPath, nil
}

func readCatalogFSFile(files fs.FS, directory, relativePath, kind string) ([]byte, string, error) {
	if relativePath == "" || path.IsAbs(relativePath) {
		return nil, "", fmt.Errorf("%s path %q must be relative", kind, relativePath)
	}

	cleanPath := path.Clean(relativePath)
	if cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return nil, "", fmt.Errorf("%s path %q must stay within the agents file directory", kind, relativePath)
	}

	contents, err := readBoundedFSFile(files, path.Join(directory, cleanPath), maxAgentConfigBytes)
	if err != nil {
		return nil, "", fmt.Errorf("read %s %q: %w", kind, relativePath, err)
	}

	return contents, cleanPath, nil
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

func readBoundedFSFile(files fs.FS, name string, maximum int64) ([]byte, error) {
	if files == nil || !fs.ValidPath(name) {
		return nil, fmt.Errorf("invalid file path %q", name)
	}

	info, err := fs.Stat(files, name)
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file %q must be a regular file", name)
	}

	if info.Size() > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}

	contents, err := fs.ReadFile(files, name)
	if err != nil {
		return nil, err
	}

	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}

	return contents, nil
}
