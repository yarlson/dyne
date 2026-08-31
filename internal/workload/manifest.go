package workload

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	codexAuthSecretName = "coding-agent-auth"
	maxAgentConfigBytes = 900 * 1024
)

type sessionManifestSpec struct {
	Name           string
	Namespace      string
	Image          string
	Storage        Storage
	TaskName       string
	Resume         bool
	Repository     string
	InitialRef     string
	SetupCommand   string
	Prompt         string
	AgentName      string
	Instructions   string
	Skills         []Skill
	CloneDepth     int
	StorageSize    string
	TimeoutSeconds int64
	ResultKind     ResultKind
	WorkflowRun    string
	WorkflowStep   string
	GitCredential  string
}

type manifestResource map[string]any

type manifestResourceList struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Items      []manifestResource `json:"items"`
}

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

func (s sessionManifestSpec) validate(initial bool) error {
	if s.ResultKind == "" {
		s.ResultKind = ResultKindPullRequest
	}

	if !dnsLabelPattern.MatchString(s.Name) || len(s.Name) > 40 {
		return errors.New("name must be a lowercase DNS label no longer than 40 characters")
	}

	if !dnsLabelPattern.MatchString(s.Namespace) || len(s.Namespace) > 63 {
		return errors.New("namespace must be a lowercase DNS label no longer than 63 characters")
	}

	if s.TaskName != "" && (!dnsLabelPattern.MatchString(s.TaskName) || len(s.TaskName) > 63) {
		return errors.New("task name must be a lowercase DNS label no longer than 63 characters")
	}

	if (s.WorkflowRun == "") != (s.WorkflowStep == "") {
		return errors.New("workflow run and step must be provided together")
	}

	if s.WorkflowRun != "" {
		if !dnsLabelPattern.MatchString(s.WorkflowRun) || len(s.WorkflowRun) > 40 {
			return errors.New("workflow run must be a lowercase DNS label no longer than 40 characters")
		}

		if !dnsLabelPattern.MatchString(s.WorkflowStep) || len(s.WorkflowStep) > 63 {
			return errors.New("workflow step must be a lowercase DNS label no longer than 63 characters")
		}
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

		if strings.TrimSpace(s.Instructions) == "" {
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

	switch s.ResultKind {
	case "", ResultKindPullRequest, ResultKindWorkflowOutput:
	default:
		return fmt.Errorf("unsupported result kind %q", s.ResultKind)
	}

	return nil
}

func renderSessionManifest(s sessionManifestSpec) ([]byte, error) {
	return render(s, true)
}

func renderContinuationManifest(s sessionManifestSpec) ([]byte, error) {
	if s.Storage != StoragePersistent {
		return nil, errors.New("continuation requires persistent storage")
	}

	if s.TaskName == "" {
		return nil, errors.New("continuation task name is required")
	}

	return render(s, false)
}

func render(s sessionManifestSpec, initial bool) ([]byte, error) {
	if err := s.validate(initial); err != nil {
		return nil, err
	}

	items := []manifestResource{denyIngressPolicy(s.Namespace)}
	if initial && s.Storage == StoragePersistent {
		items = append(items, persistentVolumeClaim(s))
	}

	if s.AgentName != "" {
		items = append(items, agentConfigMap(s))
	}

	if s.GitCredential != "" {
		items = append(items, repositoryCredentialSecret(s))
	}

	items = append(items, sessionJob(s))

	return encodeResourceList(items)
}

func encodeResourceList(items []manifestResource) ([]byte, error) {
	return json.MarshalIndent(manifestResourceList{APIVersion: "v1", Kind: "List", Items: items}, "", "  ")
}

func sessionClaimName(name string) string {
	return "session-" + name
}

func denyIngressPolicy(namespace string) manifestResource {
	return manifestResource{
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

func persistentVolumeClaim(s sessionManifestSpec) manifestResource {
	return manifestResource{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      sessionClaimName(s.Name),
			"namespace": s.Namespace,
			"labels":    sessionLabelsFor(s),
		},
		"spec": map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{"storage": s.StorageSize},
			},
		},
	}
}

func sessionJob(s sessionManifestSpec) manifestResource {
	name := taskName(s)
	labels := sessionLabelsFor(s)
	labels["coding-agent/task"] = name

	return manifestResource{
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

func sessionPodTemplate(s sessionManifestSpec) map[string]any {
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

func sessionContainer(s sessionManifestSpec, name string, args []any, workspaceReadOnly bool, access mountAccess) map[string]any {
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
		map[string]any{"name": "AGENT_RESULT_KIND", "value": string(resultKind(s.ResultKind))},
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
				"configMapKeyRef": map[string]any{"name": sessionAgentConfigName(s.Name), "key": "instructions"},
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

func resultKind(kind ResultKind) ResultKind {
	if kind == "" {
		return ResultKindPullRequest
	}

	return kind
}

func taskLabels(s sessionManifestSpec) map[string]any {
	labels := sessionLabelsFor(s)
	labels["coding-agent/task"] = taskName(s)

	return labels
}

func taskName(s sessionManifestSpec) string {
	if s.TaskName != "" {
		return s.TaskName
	}

	return s.Name
}

func sessionDirectoryContainer(s sessionManifestSpec) map[string]any {
	container := sessionContainer(s, "session-init", nil, false, mountAccess{})
	container["command"] = []any{"mkdir"}
	container["args"] = []any{"-p", "/session/workspace", "/session/home", "/session/agent", "/session/logs", "/session/artifacts"}
	container["volumeMounts"] = []any{map[string]any{"name": "session", "mountPath": "/session"}}

	return container
}

func sessionVolumes(s sessionManifestSpec) []any {
	sessionVolume := map[string]any{"name": "session"}
	if s.Storage == StorageEphemeral {
		sessionVolume["emptyDir"] = map[string]any{"sizeLimit": "7Gi"}
	} else {
		sessionVolume["persistentVolumeClaim"] = map[string]any{"claimName": sessionClaimName(s.Name)}
	}

	volumes := []any{
		sessionVolume,
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
			"name":     "git-auth",
			"emptyDir": map[string]any{},
		},
	}
	if s.GitCredential != "" {
		volumes[3] = map[string]any{
			"name": "git-auth",
			"secret": map[string]any{
				"secretName":  repositoryCredentialSecretName(s),
				"defaultMode": 288,
			},
		}
	}

	if len(s.Skills) > 0 {
		items := make([]any, len(s.Skills))
		skills := slices.Clone(s.Skills)
		slices.SortFunc(skills, func(left, right Skill) int {
			return strings.Compare(left.Name, right.Name)
		})
		for i, skill := range skills {
			items[i] = map[string]any{"key": agentSkillKey(skill.Name), "path": skill.Name + "/SKILL.md"}
		}

		volumes = append(volumes, map[string]any{
			"name": "agent-config",
			"configMap": map[string]any{
				"name":  sessionAgentConfigName(s.Name),
				"items": items,
			},
		})
	}

	return volumes
}

func sessionAgentConfigName(name string) string {
	return "session-" + name + "-agent"
}

func agentConfigMap(s sessionManifestSpec) manifestResource {
	data := map[string]any{"instructions": s.Instructions}
	for _, skill := range s.Skills {
		data[agentSkillKey(skill.Name)] = skill.Contents
	}

	return manifestResource{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      sessionAgentConfigName(s.Name),
			"namespace": s.Namespace,
			"labels":    sessionLabelsFor(s),
		},
		"immutable": true,
		"data":      data,
	}
}

func repositoryCredentialSecret(s sessionManifestSpec) manifestResource {
	return manifestResource{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      repositoryCredentialSecretName(s),
			"namespace": s.Namespace,
			"labels":    taskLabels(s),
		},
		"type":       "Opaque",
		"stringData": map[string]any{"token": s.GitCredential},
	}
}

func repositoryCredentialSecretName(s sessionManifestSpec) string {
	return taskName(s) + "-git"
}

func agentSkillKey(name string) string {
	return "skill-" + name
}

func validateAgentSkills(skills []Skill) error {
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

func sessionLabelsFor(s sessionManifestSpec) map[string]any {
	labels := sessionLabels(s.Name)
	if s.WorkflowRun != "" {
		labels["coding-agent/workflow-run"] = s.WorkflowRun
		labels["coding-agent/workflow-step"] = s.WorkflowStep
	}

	return labels
}
