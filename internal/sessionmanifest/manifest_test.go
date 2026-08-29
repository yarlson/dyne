package sessionmanifest

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
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
			if err != nil {
				t.Fatal(err)
			}

			resources := decodeRenderedResources(t, manifest)
			identities := make([]string, 0, len(resources))
			for identity := range resources {
				identities = append(identities, identity)
			}

			slices.Sort(identities)
			if !slices.Equal(identities, test.wantResources) {
				t.Fatalf("got resources %v, want %v", identities, test.wantResources)
			}

			storage := renderedStorage(resources[test.workload])
			if !maps.Equal(storage, test.wantStorage) {
				t.Fatalf("got storage %v, want %v", storage, test.wantStorage)
			}
		})
	}
}

func TestRenderBootstrapIncludesRequestedCredentials(t *testing.T) {
	manifest, err := RenderBootstrap("coding-agents", []byte(`{"token":"codex"}`), "", "github-secret")
	if err != nil {
		t.Fatal(err)
	}

	resources := decodeRenderedResources(t, manifest)
	auth := resources["Secret/coding-agent-auth"].Data
	if len(auth) != 1 || auth["auth.json"] != "eyJ0b2tlbiI6ImNvZGV4In0=" {
		t.Fatalf("got Codex credentials %v", auth)
	}

	git := resources["Secret/coding-agent-git-auth"].Data
	if len(git) != 1 || git["token"] != "Z2l0aHViLXNlY3JldA==" {
		t.Fatalf("got GitHub credentials %v", git)
	}
}

func TestRenderBootstrapRejectsMultipleCodexCredentials(t *testing.T) {
	_, err := RenderBootstrap("coding-agents", []byte(`{"token":"codex"}`), "api-key", "")
	if err == nil || !strings.Contains(err.Error(), "either Codex auth JSON or an API key") {
		t.Fatalf("got error %v", err)
	}
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
			if _, err := Render(spec); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("got error %v, want message containing %q", err, test.message)
			}
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
	if err := json.Unmarshal(manifest, &list); err != nil {
		t.Fatal(err)
	}

	resources := make(map[string]renderedResource, len(list.Items))
	for _, item := range list.Items {
		identity := item.Kind + "/" + item.Metadata.Name
		if _, exists := resources[identity]; exists {
			t.Fatalf("manifest contains duplicate resource %s", identity)
		}

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
