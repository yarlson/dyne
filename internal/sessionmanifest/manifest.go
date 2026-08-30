package sessionmanifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	// DefaultNamespace is the namespace used when a caller does not specify one.
	DefaultNamespace = "coding-agents"
	// DefaultImage is the coding-session image used when a caller does not specify one.
	DefaultImage = "coding-agent:local"
	// GitHubTokenSecretName is the name of the Secret that stores the GitHub token.
	GitHubTokenSecretName = "coding-agent-git-auth"
	codexAuthSecretName   = "coding-agent-auth"
	maxAgentConfigBytes   = 900 * 1024
	// SessionImageAnnotation stores the image in a retained session definition.
	SessionImageAnnotation = "dyne.yarlson.dev/image"
	// SessionRepositoryAnnotation stores the repository in a retained session definition.
	SessionRepositoryAnnotation = "dyne.yarlson.dev/repository"
	// SessionInitialRefAnnotation stores the initial ref in a retained session definition.
	SessionInitialRefAnnotation = "dyne.yarlson.dev/initial-ref"
	// SessionSetupAnnotation stores the setup command in a retained session definition.
	SessionSetupAnnotation = "dyne.yarlson.dev/setup"
	// SessionCloneDepthAnnotation stores the clone depth in a retained session definition.
	SessionCloneDepthAnnotation = "dyne.yarlson.dev/clone-depth"
	// SessionAgentAnnotation stores the configured agent name in retained resources.
	SessionAgentAnnotation = "dyne.yarlson.dev/agent"
)

// Storage controls whether a session retains state after its task Pod is removed.
type Storage string

const (
	// StorageEphemeral uses Pod-owned temporary storage for one disposable task.
	StorageEphemeral Storage = "ephemeral"
	// StoragePersistent retains workspace, tool, and agent state on one claim.
	StoragePersistent Storage = "persistent"
)

// AgentSkill contains one instruction-only Codex skill.
type AgentSkill struct {
	// Name identifies the skill inside the Codex skill directory.
	Name string
	// Contents is the complete SKILL.md file.
	Contents string
}

// Spec defines one coding-session workload.
type Spec struct {
	// Name identifies the session and prefixes its Kubernetes resources.
	Name string
	// Namespace is the Kubernetes namespace that owns the session.
	Namespace string
	// Image is the container image that runs setup and agent commands.
	Image string
	// Storage selects whether session state is temporary or persistent.
	Storage Storage
	// TaskName identifies the Job; an empty value uses the session name.
	TaskName string
	// Resume continues the retained agent thread for a persistent session.
	Resume bool
	// Repository is the Git repository cloned into the workspace; an empty value initializes a new repository.
	Repository string
	// InitialRef is the Git branch or tag cloned for the session.
	InitialRef string
	// SetupCommand is the shell command run before the agent starts.
	SetupCommand string
	// Prompt is the task given to a bounded session.
	Prompt string
	// AgentName identifies the reusable agent template used to start the session.
	AgentName string
	// Instructions are additional Codex developer instructions.
	Instructions string
	// Skills are the instruction-only Codex skills available to the agent.
	Skills []AgentSkill
	// CloneDepth limits fetched Git history; zero fetches the full history.
	CloneDepth int
	// StorageSize is the requested size of the workspace and tool-home claims.
	StorageSize string
	// TimeoutSeconds is the bounded session deadline in seconds.
	TimeoutSeconds int64
}

type resource map[string]any

type resourceList struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Items      []resource `json:"items"`
}

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

func (s Spec) validate(initial bool) error {
	if !dnsLabelPattern.MatchString(s.Name) || len(s.Name) > 40 {
		return errors.New("name must be a lowercase DNS label no longer than 40 characters")
	}

	if !dnsLabelPattern.MatchString(s.Namespace) || len(s.Namespace) > 63 {
		return errors.New("namespace must be a lowercase DNS label no longer than 63 characters")
	}

	if s.TaskName != "" && (!dnsLabelPattern.MatchString(s.TaskName) || len(s.TaskName) > 63) {
		return errors.New("task name must be a lowercase DNS label no longer than 63 characters")
	}

	if strings.TrimSpace(s.Image) == "" {
		return errors.New("image is required")
	}

	if s.InitialRef == "" {
		return errors.New("ref is required")
	}

	if initial && s.StorageSize == "" {
		return errors.New("storage size is required")
	}

	if s.CloneDepth < 0 {
		return errors.New("clone depth cannot be negative")
	}

	if s.TimeoutSeconds <= 0 {
		return errors.New("timeout must be greater than zero")
	}

	if strings.TrimSpace(s.Prompt) == "" {
		return errors.New("prompt is required")
	}

	if s.AgentName == "" {
		if s.Instructions != "" || len(s.Skills) > 0 {
			return errors.New("agent name is required for instructions and skills")
		}
	} else {
		if !dnsLabelPattern.MatchString(s.AgentName) || len(s.AgentName) > 63 {
			return errors.New("agent name must be a lowercase DNS label no longer than 63 characters")
		}

		if initial && strings.TrimSpace(s.Instructions) == "" {
			return errors.New("agent instructions are required")
		}

		if err := validateAgentSkills(s.Skills); err != nil {
			return err
		}

		size := len(s.Instructions)
		for _, skill := range s.Skills {
			size += len(skill.Name) + len(skill.Contents)
		}

		if size > maxAgentConfigBytes {
			return fmt.Errorf("instructions and skills exceed %d bytes", maxAgentConfigBytes)
		}
	}

	switch s.Storage {
	case StorageEphemeral, StoragePersistent:
	default:
		return fmt.Errorf("unsupported storage %q", s.Storage)
	}

	return nil
}

// Render validates a session and returns the Kubernetes resources that run it.
func Render(s Spec) ([]byte, error) {
	return render(s, true)
}

// RenderContinuation returns a Job that reuses an existing persistent session claim.
func RenderContinuation(s Spec) ([]byte, error) {
	if s.Storage != StoragePersistent {
		return nil, errors.New("continuation requires persistent storage")
	}

	if s.TaskName == "" {
		return nil, errors.New("continuation task name is required")
	}

	return render(s, false)
}

func render(s Spec, initial bool) ([]byte, error) {
	if err := s.validate(initial); err != nil {
		return nil, err
	}

	items := []resource{denyIngressPolicy(s.Namespace)}
	if initial && s.Storage == StoragePersistent {
		items = append(items, persistentVolumeClaim(s))
	}

	if initial && s.AgentName != "" {
		items = append(items, agentConfigMap(s))
	}

	items = append(items, sessionJob(s))

	return encodeResourceList(items)
}

func encodeResourceList(items []resource) ([]byte, error) {
	return json.MarshalIndent(resourceList{APIVersion: "v1", Kind: "List", Items: items}, "", "  ")
}

// SessionClaimName returns the persistent claim that owns all retained session state.
func SessionClaimName(name string) string {
	return "session-" + name
}

func denyIngressPolicy(namespace string) resource {
	return resource{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      "deny-all-ingress",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []any{"Ingress"},
		},
	}
}

func persistentVolumeClaim(s Spec) resource {
	annotations := map[string]any{
		SessionImageAnnotation:      s.Image,
		SessionRepositoryAnnotation: s.Repository,
		SessionInitialRefAnnotation: s.InitialRef,
		SessionSetupAnnotation:      s.SetupCommand,
		SessionCloneDepthAnnotation: fmt.Sprintf("%d", s.CloneDepth),
	}
	if s.AgentName != "" {
		annotations[SessionAgentAnnotation] = s.AgentName
	}

	return resource{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":        SessionClaimName(s.Name),
			"namespace":   s.Namespace,
			"labels":      sessionLabels(s.Name),
			"annotations": annotations,
		},
		"spec": map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{"storage": s.StorageSize},
			},
		},
	}
}

func sessionJob(s Spec) resource {
	name := taskName(s)
	labels := sessionLabels(s.Name)
	labels["coding-agent/task"] = name

	return resource{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": s.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"backoffLimit":          1,
			"activeDeadlineSeconds": s.TimeoutSeconds,
			"template":              sessionPodTemplate(s),
		},
	}
}

func sessionPodTemplate(s Spec) map[string]any {
	workspaceReadOnly := s.Storage == StorageEphemeral

	return map[string]any{
		"metadata": map[string]any{"labels": taskLabels(s)},
		"spec": map[string]any{
			"automountServiceAccountToken":  false,
			"restartPolicy":                 "Never",
			"terminationGracePeriodSeconds": 30,
			"securityContext": map[string]any{
				"runAsNonRoot": true,
				"runAsUser":    1000,
				"runAsGroup":   1000,
				"fsGroup":      1000,
				"seccompProfile": map[string]any{
					"type": "RuntimeDefault",
				},
			},
			"initContainers": []any{
				sessionDirectoryContainer(s),
				sessionContainer(s, "repo-init", []any{"clone"}, false, mountAccess{workspace: true, gitAuth: true}),
				sessionContainer(s, "workspace-init", []any{"init"}, false, mountAccess{workspace: true, home: true, tmp: true}),
				sessionContainer(s, "auth-init", []any{"auth"}, false, mountAccess{codex: true}),
			},
			"containers": []any{sessionContainer(s, "agent", []any{"run"}, workspaceReadOnly, mountAccess{workspace: true, home: true, tmp: true, codex: true, artifacts: true, logs: true, agentInstructions: s.AgentName != "", agentSkills: len(s.Skills) > 0})},
			"volumes":    sessionVolumes(s),
		},
	}
}

type mountAccess struct {
	workspace         bool
	home              bool
	tmp               bool
	codex             bool
	artifacts         bool
	logs              bool
	gitAuth           bool
	agentInstructions bool
	agentSkills       bool
}

func sessionContainer(s Spec, name string, args []any, workspaceReadOnly bool, access mountAccess) map[string]any {
	volumeMounts := make([]any, 0, 5)
	if access.workspace {
		volumeMounts = append(volumeMounts, map[string]any{"name": "session", "mountPath": "/workspace", "subPath": "workspace", "readOnly": workspaceReadOnly})
	}

	if access.home {
		volumeMounts = append(volumeMounts, map[string]any{"name": "session", "mountPath": "/home/agent", "subPath": "home"})
	}

	if access.tmp {
		volumeMounts = append(volumeMounts, map[string]any{"name": "tmp", "mountPath": "/tmp"})
	}

	if access.codex {
		volumeMounts = append(volumeMounts,
			map[string]any{"name": "session", "mountPath": "/codex", "subPath": "agent"},
			map[string]any{"name": "auth", "mountPath": "/var/run/agent-auth", "readOnly": true},
		)
	}

	if access.artifacts {
		volumeMounts = append(volumeMounts, map[string]any{"name": "session", "mountPath": "/artifacts", "subPath": "artifacts"})
	}

	if access.logs {
		volumeMounts = append(volumeMounts, map[string]any{"name": "session", "mountPath": "/logs", "subPath": "logs"})
	}

	if access.gitAuth {
		volumeMounts = append(volumeMounts, map[string]any{"name": "git-auth", "mountPath": "/var/run/git-auth", "readOnly": true})
	}

	if access.agentSkills {
		volumeMounts = append(volumeMounts, map[string]any{"name": "agent-config", "mountPath": "/home/agent/.agents/skills", "readOnly": true})
	}

	environment := []any{
		map[string]any{"name": "AGENT_STORAGE", "value": string(s.Storage)},
		map[string]any{"name": "AGENT_REPOSITORY", "value": s.Repository},
		map[string]any{"name": "AGENT_REF", "value": s.InitialRef},
		map[string]any{"name": "AGENT_SETUP", "value": s.SetupCommand},
		map[string]any{"name": "AGENT_TASK", "value": s.Prompt},
		map[string]any{"name": "AGENT_TASK_ID", "value": taskName(s)},
		map[string]any{"name": "AGENT_RESUME", "value": fmt.Sprintf("%t", s.Resume)},
		map[string]any{"name": "AGENT_CLONE_DEPTH", "value": fmt.Sprintf("%d", s.CloneDepth)},
		map[string]any{"name": "HOME", "value": "/home/agent"},
		map[string]any{"name": "CODEX_HOME", "value": "/codex"},
		map[string]any{"name": "MISE_DATA_DIR", "value": "/home/agent/.local/share/mise"},
		map[string]any{"name": "MISE_CACHE_DIR", "value": "/home/agent/.cache/mise"},
		map[string]any{"name": "npm_config_cache", "value": "/home/agent/.cache/npm"},
	}
	if access.agentInstructions {
		environment = append(environment, map[string]any{
			"name": "AGENT_INSTRUCTIONS",
			"valueFrom": map[string]any{
				"configMapKeyRef": map[string]any{"name": SessionAgentConfigName(s.Name), "key": "instructions"},
			},
		})
	}

	return map[string]any{
		"name":            name,
		"image":           s.Image,
		"imagePullPolicy": "IfNotPresent",
		"args":            args,
		"workingDir":      "/workspace",
		"env":             environment,
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   true,
			"capabilities": map[string]any{
				"drop": []any{"ALL"},
			},
		},
		"resources": map[string]any{
			"requests": map[string]any{
				"cpu":               "250m",
				"memory":            "512Mi",
				"ephemeral-storage": "256Mi",
			},
			"limits": map[string]any{
				"cpu":               "2",
				"memory":            "4Gi",
				"ephemeral-storage": "2Gi",
			},
		},
		"volumeMounts": volumeMounts,
	}
}

func taskLabels(s Spec) map[string]any {
	labels := sessionLabels(s.Name)
	labels["coding-agent/task"] = taskName(s)

	return labels
}

func taskName(s Spec) string {
	if s.TaskName != "" {
		return s.TaskName
	}

	return s.Name
}

func sessionDirectoryContainer(s Spec) map[string]any {
	container := sessionContainer(s, "session-init", nil, false, mountAccess{})
	container["command"] = []any{"mkdir"}
	container["args"] = []any{"-p", "/session/workspace", "/session/home", "/session/agent", "/session/logs", "/session/artifacts", "/session/state"}
	container["volumeMounts"] = []any{map[string]any{"name": "session", "mountPath": "/session"}}

	return container
}

func sessionVolumes(s Spec) []any {
	session := map[string]any{"name": "session"}
	if s.Storage == StorageEphemeral {
		session["emptyDir"] = map[string]any{"sizeLimit": "7Gi"}
	} else {
		session["persistentVolumeClaim"] = map[string]any{"claimName": SessionClaimName(s.Name)}
	}

	volumes := []any{
		session,
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "1Gi"}},
		map[string]any{
			"name": "auth",
			"secret": map[string]any{
				"secretName":  codexAuthSecretName,
				"optional":    true,
				"defaultMode": 288,
			},
		},
		map[string]any{
			"name": "git-auth",
			"secret": map[string]any{
				"secretName":  GitHubTokenSecretName,
				"optional":    true,
				"defaultMode": 288,
			},
		},
	}
	if len(s.Skills) > 0 {
		items := make([]any, len(s.Skills))
		skills := slices.Clone(s.Skills)
		slices.SortFunc(skills, func(left, right AgentSkill) int {
			return strings.Compare(left.Name, right.Name)
		})
		for i, skill := range skills {
			items[i] = map[string]any{"key": agentSkillKey(skill.Name), "path": skill.Name + "/SKILL.md"}
		}

		volumes = append(volumes, map[string]any{
			"name": "agent-config",
			"configMap": map[string]any{
				"name":  SessionAgentConfigName(s.Name),
				"items": items,
			},
		})
	}

	return volumes
}

// SessionAgentConfigName returns the immutable agent configuration owned by a session.
func SessionAgentConfigName(name string) string {
	return "session-" + name + "-agent"
}

func agentConfigMap(s Spec) resource {
	data := map[string]any{"instructions": s.Instructions}
	for _, skill := range s.Skills {
		data[agentSkillKey(skill.Name)] = skill.Contents
	}

	return resource{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":        SessionAgentConfigName(s.Name),
			"namespace":   s.Namespace,
			"labels":      sessionLabels(s.Name),
			"annotations": map[string]any{SessionAgentAnnotation: s.AgentName},
		},
		"immutable": true,
		"data":      data,
	}
}

func agentSkillKey(name string) string {
	return "skill-" + name
}

func validateAgentSkills(skills []AgentSkill) error {
	names := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		if !dnsLabelPattern.MatchString(skill.Name) || len(skill.Name) > 63 {
			return errors.New("skill name must be a lowercase DNS label no longer than 63 characters")
		}

		if strings.TrimSpace(skill.Contents) == "" {
			return fmt.Errorf("skill %s contents are required", skill.Name)
		}

		if _, exists := names[skill.Name]; exists {
			return fmt.Errorf("skill %s is duplicated", skill.Name)
		}

		names[skill.Name] = struct{}{}
	}

	return nil
}

func sessionLabels(session string) map[string]any {
	return map[string]any{
		"app.kubernetes.io/name":       "coding-agent",
		"app.kubernetes.io/managed-by": "dyne",
		"coding-agent/session":         session,
	}
}
