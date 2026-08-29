package sessionmanifest

import (
	"encoding/json"
	"maps"
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

func TestRenderSelectsExplicitSessionStorage(t *testing.T) {
	tests := []struct {
		name          string
		storage       Storage
		workload      string
		wantResources []string
		wantStorage   map[string]string
	}{
		{
			name:     "ephemeral session uses emptyDir",
			storage:  StorageEphemeral,
			workload: "Job/example",
			wantResources: []string{
				"Job/example",
				"NetworkPolicy/deny-all-ingress",
			},
			wantStorage: map[string]string{
				"session": "ephemeral",
				"tmp":     "memory",
			},
		},
		{
			name:     "persistent session uses one claim",
			storage:  StoragePersistent,
			workload: "Job/example",
			wantResources: []string{
				"Job/example",
				"NetworkPolicy/deny-all-ingress",
				"PersistentVolumeClaim/session-example",
			},
			wantStorage: map[string]string{
				"session": "session-example",
				"tmp":     "memory",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validSpec()
			spec.Storage = test.storage
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

func TestRenderContinuationReusesPersistentStorage(t *testing.T) {
	spec := validSpec()
	spec.TaskName = "example-continue-abc123"
	spec.Resume = true
	manifest, err := RenderContinuation(spec)
	require.NoError(t, err)

	resources := decodeRenderedResources(t, manifest)
	assert.ElementsMatch(t, []string{
		"Job/example-continue-abc123",
		"NetworkPolicy/deny-all-ingress",
	}, slices.Collect(maps.Keys(resources)))
	job := resources["Job/example-continue-abc123"]
	assert.Equal(t, "session-example", renderedStorage(job)["session"])
}

func TestRenderRejectsInvalidContracts(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*Spec)
		message string
	}{
		{name: "unsafe name", change: func(s *Spec) { s.Name = "Not Safe" }, message: "name"},
		{name: "missing prompt", change: func(s *Spec) { s.Prompt = "" }, message: "prompt"},
		{name: "unknown storage", change: func(s *Spec) { s.Storage = "remote" }, message: "unsupported"},
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
		Storage:        StoragePersistent,
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
