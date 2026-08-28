package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	// DefaultNamespace is the namespace used when a command does not specify one.
	DefaultNamespace = "coding-agents"
	// DefaultImage is the agent image used when a session does not specify one.
	DefaultImage = "coding-agent:local"
	// GitSecretName is the name of the Secret that stores the GitHub token.
	GitSecretName  = "coding-agent-git-auth"
	authSecretName = "coding-agent-auth"
)

// Mode controls the workload lifecycle and storage used by a session.
type Mode string

const (
	// ModeExplore runs a bounded task with an ephemeral, read-only workspace.
	ModeExplore Mode = "explore"
	// ModeUpdate runs a bounded task and retains its workspace on persistent storage.
	ModeUpdate Mode = "update"
	// ModeLong runs a resumable interactive session on persistent storage.
	ModeLong Mode = "long"
)

// Session defines one coding-agent workload.
type Session struct {
	// Name identifies the session and prefixes its Kubernetes resources.
	Name string
	// Namespace is the Kubernetes namespace that owns the session.
	Namespace string
	// Image is the container image that runs setup and agent commands.
	Image string
	// Mode selects the session lifecycle and storage behavior.
	Mode Mode
	// Repository is the Git repository cloned into the workspace; an empty value initializes a new repository.
	Repository string
	// Ref is the initial Git branch or tag cloned for the session.
	Ref string
	// Setup is the shell command run before the agent starts.
	Setup string
	// Prompt is the task given to a bounded session.
	Prompt string
	// CloneDepth limits fetched Git history; zero fetches the full history.
	CloneDepth int
	// StorageSize is the requested size of each persistent workspace claim.
	StorageSize string
	// Timeout is the bounded session deadline in seconds.
	Timeout int64
}

type resource map[string]any

type resourceList struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Items      []resource `json:"items"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

func (s Session) validate() error {
	if !dnsLabel.MatchString(s.Name) || len(s.Name) > 40 {
		return errors.New("name must be a lowercase DNS label no longer than 40 characters")
	}
	if !dnsLabel.MatchString(s.Namespace) || len(s.Namespace) > 63 {
		return errors.New("namespace must be a lowercase DNS label no longer than 63 characters")
	}
	if strings.TrimSpace(s.Image) == "" {
		return errors.New("image is required")
	}
	if s.Ref == "" {
		return errors.New("ref is required")
	}
	if s.StorageSize == "" {
		return errors.New("storage size is required")
	}
	if s.CloneDepth < 0 {
		return errors.New("clone depth cannot be negative")
	}
	if s.Timeout <= 0 {
		return errors.New("timeout must be greater than zero")
	}
	switch s.Mode {
	case ModeExplore, ModeUpdate:
		if strings.TrimSpace(s.Prompt) == "" {
			return fmt.Errorf("prompt is required for %s sessions", s.Mode)
		}
	case ModeLong:
		if s.Prompt != "" {
			return errors.New("long sessions accept tasks through the task command")
		}
	default:
		return fmt.Errorf("unsupported mode %q", s.Mode)
	}
	return nil
}

// Bootstrap returns a Kubernetes manifest for the shared namespace, network policy, and requested credentials.
func Bootstrap(namespace string, authFile string, apiKeyEnv string, githubTokenEnv string) ([]byte, error) {
	if !dnsLabel.MatchString(namespace) || len(namespace) > 63 {
		return nil, errors.New("namespace must be a lowercase DNS label no longer than 63 characters")
	}
	items := []resource{namespaceResource(namespace), ingressPolicy(namespace)}
	secret, err := authSecret(namespace, authFile, apiKeyEnv)
	if err != nil {
		return nil, err
	}
	if secret != nil {
		items = append(items, secret)
	}
	gitSecret, err := githubSecret(namespace, githubTokenEnv)
	if err != nil {
		return nil, err
	}
	if gitSecret != nil {
		items = append(items, gitSecret)
	}
	return encodeResources(items)
}

// Manifest validates a session and returns the Kubernetes resources that run it.
func Manifest(s Session) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	items := []resource{namespaceResource(s.Namespace), ingressPolicy(s.Namespace)}
	if s.Mode != ModeExplore {
		items = append(items,
			persistentVolumeClaim(s.Namespace, WorkspaceClaimName(s.Name), s.Name, s.StorageSize),
			persistentVolumeClaim(s.Namespace, HomeClaimName(s.Name), s.Name, s.StorageSize),
			persistentVolumeClaim(s.Namespace, CodexClaimName(s.Name), s.Name, "1Gi"),
		)
	}
	if s.Mode == ModeLong {
		items = append(items, headlessService(s), statefulSet(s))
	} else {
		items = append(items, job(s))
	}
	return encodeResources(items)
}

func encodeResources(items []resource) ([]byte, error) {
	return json.MarshalIndent(resourceList{APIVersion: "v1", Kind: "List", Items: items}, "", "  ")
}

// WorkspaceClaimName returns the persistent workspace claim owned by a session.
func WorkspaceClaimName(name string) string {
	return "workspace-" + name
}

// HomeClaimName returns the persistent tool-home claim owned by a session.
func HomeClaimName(name string) string {
	return "home-" + name
}

// CodexClaimName returns the persistent Codex-state claim owned by a session.
func CodexClaimName(name string) string {
	return "codex-" + name
}

func namespaceResource(namespace string) resource {
	return resource{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": namespace,
			"labels": map[string]any{
				"pod-security.kubernetes.io/enforce": "restricted",
				"pod-security.kubernetes.io/audit":   "restricted",
				"pod-security.kubernetes.io/warn":    "restricted",
			},
		},
	}
}

func ingressPolicy(namespace string) resource {
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

func authSecret(namespace string, authFile string, apiKeyEnv string) (resource, error) {
	data := map[string]any{}
	if authFile != "" {
		contents, err := os.ReadFile(authFile)
		if err != nil {
			return nil, fmt.Errorf("read auth file: %w", err)
		}
		data["auth.json"] = base64.StdEncoding.EncodeToString(contents)
	}
	if apiKeyEnv != "" {
		value, ok := os.LookupEnv(apiKeyEnv)
		if !ok || value == "" {
			return nil, fmt.Errorf("environment variable %s is empty", apiKeyEnv)
		}
		data["CODEX_API_KEY"] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	if len(data) == 0 {
		return nil, nil
	}
	return resource{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      authSecretName,
			"namespace": namespace,
		},
		"type": "Opaque",
		"data": data,
	}, nil
}

func githubSecret(namespace string, tokenEnv string) (resource, error) {
	if tokenEnv == "" {
		return nil, nil
	}
	value, ok := os.LookupEnv(tokenEnv)
	if !ok || value == "" {
		return nil, fmt.Errorf("environment variable %s is empty", tokenEnv)
	}
	return resource{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      GitSecretName,
			"namespace": namespace,
		},
		"type": "Opaque",
		"data": map[string]any{
			"token": base64.StdEncoding.EncodeToString([]byte(value)),
		},
	}, nil
}

func persistentVolumeClaim(namespace string, name string, session string, size string) resource {
	return resource{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    labels(session),
		},
		"spec": map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{"storage": size},
			},
		},
	}
}

func headlessService(s Session) resource {
	return resource{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      s.Name,
			"namespace": s.Namespace,
			"labels":    labels(s.Name),
		},
		"spec": map[string]any{
			"clusterIP": "None",
			"selector":  labels(s.Name),
		},
	}
}

func job(s Session) resource {
	return resource{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      s.Name,
			"namespace": s.Namespace,
			"labels":    labels(s.Name),
		},
		"spec": map[string]any{
			"backoffLimit":          0,
			"activeDeadlineSeconds": s.Timeout,
			"template":              podTemplate(s, false),
		},
	}
}

func statefulSet(s Session) resource {
	return resource{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": map[string]any{
			"name":      s.Name,
			"namespace": s.Namespace,
			"labels":    labels(s.Name),
		},
		"spec": map[string]any{
			"serviceName": s.Name,
			"replicas":    1,
			"selector": map[string]any{
				"matchLabels": labels(s.Name),
			},
			"template": podTemplate(s, true),
		},
	}
}

func podTemplate(s Session, long bool) map[string]any {
	workspaceReadOnly := s.Mode == ModeExplore
	mainArgs := []any{"run"}
	if long {
		mainArgs = []any{"idle"}
	}
	return map[string]any{
		"metadata": map[string]any{"labels": labels(s.Name)},
		"spec": map[string]any{
			"automountServiceAccountToken":  false,
			"restartPolicy":                 restartPolicy(long),
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
				container(s, "repo-init", []any{"clone"}, false, mountAccess{workspace: true, gitAuth: true}),
				container(s, "workspace-init", []any{"init"}, false, mountAccess{workspace: true, home: true, tmp: true}),
				container(s, "auth-init", []any{"auth"}, false, mountAccess{codex: true}),
			},
			"containers": []any{container(s, "agent", mainArgs, workspaceReadOnly, mountAccess{workspace: true, home: true, tmp: true, codex: true})},
			"volumes":    volumes(s),
		},
	}
}

func restartPolicy(long bool) string {
	if long {
		return "Always"
	}
	return "Never"
}

type mountAccess struct {
	workspace bool
	home      bool
	tmp       bool
	codex     bool
	gitAuth   bool
}

func container(s Session, name string, args []any, workspaceReadOnly bool, access mountAccess) map[string]any {
	volumeMounts := make([]any, 0, 5)
	if access.workspace {
		volumeMounts = append(volumeMounts, map[string]any{"name": "workspace", "mountPath": "/workspace", "readOnly": workspaceReadOnly})
	}
	if access.home {
		volumeMounts = append(volumeMounts, map[string]any{"name": "home", "mountPath": "/home/agent"})
	}
	if access.tmp {
		volumeMounts = append(volumeMounts, map[string]any{"name": "tmp", "mountPath": "/tmp"})
	}
	if access.codex {
		volumeMounts = append(volumeMounts,
			map[string]any{"name": "codex", "mountPath": "/codex"},
			map[string]any{"name": "auth", "mountPath": "/var/run/agent-auth", "readOnly": true},
		)
	}
	if access.gitAuth {
		volumeMounts = append(volumeMounts, map[string]any{"name": "git-auth", "mountPath": "/var/run/git-auth", "readOnly": true})
	}
	return map[string]any{
		"name":            name,
		"image":           s.Image,
		"imagePullPolicy": "IfNotPresent",
		"args":            args,
		"workingDir":      "/workspace",
		"env": []any{
			map[string]any{"name": "AGENT_MODE", "value": string(s.Mode)},
			map[string]any{"name": "AGENT_REPOSITORY", "value": s.Repository},
			map[string]any{"name": "AGENT_REF", "value": s.Ref},
			map[string]any{"name": "AGENT_SETUP", "value": s.Setup},
			map[string]any{"name": "AGENT_TASK", "value": s.Prompt},
			map[string]any{"name": "AGENT_CLONE_DEPTH", "value": fmt.Sprintf("%d", s.CloneDepth)},
			map[string]any{"name": "HOME", "value": "/home/agent"},
			map[string]any{"name": "CODEX_HOME", "value": "/codex"},
			map[string]any{"name": "MISE_DATA_DIR", "value": "/home/agent/.local/share/mise"},
			map[string]any{"name": "MISE_CACHE_DIR", "value": "/home/agent/.cache/mise"},
			map[string]any{"name": "npm_config_cache", "value": "/home/agent/.cache/npm"},
		},
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

func volumes(s Session) []any {
	workspace := map[string]any{"name": "workspace"}
	home := map[string]any{"name": "home"}
	codex := map[string]any{"name": "codex"}
	if s.Mode == ModeExplore {
		workspace["emptyDir"] = map[string]any{"sizeLimit": "4Gi"}
		home["emptyDir"] = map[string]any{"sizeLimit": "2Gi"}
		codex["emptyDir"] = map[string]any{"sizeLimit": "1Gi"}
	} else {
		workspace["persistentVolumeClaim"] = map[string]any{"claimName": WorkspaceClaimName(s.Name)}
		home["persistentVolumeClaim"] = map[string]any{"claimName": HomeClaimName(s.Name)}
		codex["persistentVolumeClaim"] = map[string]any{"claimName": CodexClaimName(s.Name)}
	}
	return []any{
		workspace,
		home,
		codex,
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "1Gi"}},
		map[string]any{
			"name": "auth",
			"secret": map[string]any{
				"secretName":  authSecretName,
				"optional":    true,
				"defaultMode": 288,
			},
		},
		map[string]any{
			"name": "git-auth",
			"secret": map[string]any{
				"secretName":  GitSecretName,
				"optional":    true,
				"defaultMode": 288,
			},
		},
	}
}

func labels(session string) map[string]any {
	return map[string]any{
		"app.kubernetes.io/name":       "coding-agent",
		"app.kubernetes.io/managed-by": "agentctl",
		"coding-agent/session":         session,
	}
}
