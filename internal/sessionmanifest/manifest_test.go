package sessionmanifest

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type renderedResource struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
	Spec struct {
		Template struct {
			Spec struct {
				Volumes []struct {
					Name                  string         `json:"name"`
					EmptyDir              map[string]any `json:"emptyDir"`
					PersistentVolumeClaim *struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func TestRenderSelectsWorkloadAndStorageForSessionLifecycle(t *testing.T) {
	tests := []struct {
		name          string
		mode          Mode
		prompt        string
		workload      string
		wantResources []string
		wantStorage   map[string]string
	}{
		{
			name:     "bounded exploration uses ephemeral storage",
			mode:     ModeExplore,
			prompt:   "inspect the repository",
			workload: "Job/example",
			wantResources: []string{
				"Job/example",
				"Namespace/coding-agents",
				"NetworkPolicy/deny-all-ingress",
			},
			wantStorage: map[string]string{
				"codex":     "ephemeral",
				"home":      "ephemeral",
				"tmp":       "memory",
				"workspace": "ephemeral",
			},
		},
		{
			name:     "bounded update retains repository and tool state",
			mode:     ModeUpdate,
			prompt:   "update the repository",
			workload: "Job/example",
			wantResources: []string{
				"Job/example",
				"Namespace/coding-agents",
				"NetworkPolicy/deny-all-ingress",
				"PersistentVolumeClaim/codex-example",
				"PersistentVolumeClaim/home-example",
				"PersistentVolumeClaim/workspace-example",
			},
			wantStorage: map[string]string{
				"codex":     "codex-example",
				"home":      "home-example",
				"tmp":       "memory",
				"workspace": "workspace-example",
			},
		},
		{
			name:     "long session is resumable and retains state",
			mode:     ModeLong,
			workload: "StatefulSet/example",
			wantResources: []string{
				"Namespace/coding-agents",
				"NetworkPolicy/deny-all-ingress",
				"PersistentVolumeClaim/codex-example",
				"PersistentVolumeClaim/home-example",
				"PersistentVolumeClaim/workspace-example",
				"Service/example",
				"StatefulSet/example",
			},
			wantStorage: map[string]string{
				"codex":     "codex-example",
				"home":      "home-example",
				"tmp":       "memory",
				"workspace": "workspace-example",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validSpec()
			spec.Mode = test.mode
			spec.Prompt = test.prompt
			manifest, err := Render(spec)
			require.NoError(t, err)

			resources := decodeRenderedResources(t, manifest)
			identities := make([]string, 0, len(resources))
			for identity := range resources {
				identities = append(identities, identity)
			}

			slices.Sort(identities)
			assert.Equal(t, test.wantResources, identities)

			storage := renderedStorage(resources[test.workload])
			assert.Equal(t, test.wantStorage, storage)
		})
	}
}

func TestRenderBootstrapIncludesRequestedCredentials(t *testing.T) {
	manifest, err := RenderBootstrap("coding-agents", []byte(`{"token":"codex"}`), "", "github-secret")
	require.NoError(t, err)

	resources := decodeRenderedResources(t, manifest)
	auth := resources["Secret/coding-agent-auth"].Data
	assert.Equal(t, map[string]string{"auth.json": "eyJ0b2tlbiI6ImNvZGV4In0="}, auth)

	git := resources["Secret/coding-agent-git-auth"].Data
	assert.Equal(t, map[string]string{"token": "Z2l0aHViLXNlY3JldA=="}, git)
}

func TestRenderBootstrapRejectsMultipleCodexCredentials(t *testing.T) {
	_, err := RenderBootstrap("coding-agents", []byte(`{"token":"codex"}`), "api-key", "")
	require.ErrorContains(t, err, "either Codex auth JSON or an API key")
}

func TestRenderRejectsInvalidContracts(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*Spec)
		message string
	}{
		{name: "unsafe name", change: func(s *Spec) { s.Name = "Not Safe" }, message: "name"},
		{name: "missing prompt", change: func(s *Spec) { s.Prompt = "" }, message: "prompt"},
		{name: "unknown mode", change: func(s *Spec) { s.Mode = "daemon" }, message: "unsupported"},
		{name: "negative clone depth", change: func(s *Spec) { s.CloneDepth = -1 }, message: "clone depth"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec := validSpec()
			test.change(&spec)
			_, err := Render(spec)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func validSpec() Spec {
	return Spec{
		Name:           "example",
		Namespace:      DefaultNamespace,
		Image:          DefaultImage,
		Mode:           ModeUpdate,
		InitialRef:     "main",
		Prompt:         "inspect the repository",
		CloneDepth:     1,
		StorageSize:    "1Gi",
		TimeoutSeconds: 600,
	}
}

func decodeRenderedResources(t *testing.T, manifest []byte) map[string]renderedResource {
	t.Helper()
	var list struct {
		Items []renderedResource `json:"items"`
	}
	require.NoError(t, json.Unmarshal(manifest, &list))

	resources := make(map[string]renderedResource, len(list.Items))
	for _, item := range list.Items {
		identity := item.Kind + "/" + item.Metadata.Name
		require.NotContains(t, resources, identity, "manifest contains duplicate resource")

		resources[identity] = item
	}

	return resources
}

func renderedStorage(workload renderedResource) map[string]string {
	storage := make(map[string]string)
	for _, volume := range workload.Spec.Template.Spec.Volumes {
		switch {
		case volume.PersistentVolumeClaim != nil:
			storage[volume.Name] = volume.PersistentVolumeClaim.ClaimName
		case volume.EmptyDir["medium"] == "Memory":
			storage[volume.Name] = "memory"
		case volume.EmptyDir != nil:
			storage[volume.Name] = "ephemeral"
		}
	}

	return storage
}
