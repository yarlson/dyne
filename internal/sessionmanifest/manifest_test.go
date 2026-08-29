package sessionmanifest

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type renderedResource struct {
	Kind      string `json:"kind"`
	Immutable bool   `json:"immutable"`
	Metadata  struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name string `json:"name"`
					Env  []struct {
						Name      string `json:"name"`
						ValueFrom *struct {
							ConfigMapKeyRef struct {
								Name string `json:"name"`
								Key  string `json:"key"`
							} `json:"configMapKeyRef"`
						} `json:"valueFrom"`
					} `json:"env"`
					VolumeMounts []struct {
						Name      string `json:"name"`
						MountPath string `json:"mountPath"`
						ReadOnly  bool   `json:"readOnly"`
					} `json:"volumeMounts"`
				} `json:"containers"`
				Volumes []struct {
					Name                  string         `json:"name"`
					EmptyDir              map[string]any `json:"emptyDir"`
					PersistentVolumeClaim *struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
					ConfigMap *struct {
						Name  string `json:"name"`
						Items []struct {
							Key  string `json:"key"`
							Path string `json:"path"`
						} `json:"items"`
					} `json:"configMap"`
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

func TestRenderPackagesAgentInstructionsAndSkills(t *testing.T) {
	spec := validSpec()
	spec.AgentName = "reviewer"
	spec.Instructions = "Review correctness and tests."
	spec.Skills = []AgentSkill{{
		Name:     "code-review",
		Contents: "---\nname: code-review\ndescription: Review code.\n---\n\nReview changed code.\n",
	}}

	manifest, err := Render(spec)
	require.NoError(t, err)
	resources := decodeRenderedResources(t, manifest)

	configuration := resources["ConfigMap/session-example-agent"]
	assert.True(t, configuration.Immutable)
	assert.Equal(t, "reviewer", configuration.Metadata.Annotations["airlock.yarlson.dev/agent"])
	assert.Equal(t, "Review correctness and tests.", configuration.Data["instructions"])
	assert.Equal(t, spec.Skills[0].Contents, configuration.Data["skill-code-review"])

	job := resources["Job/example"]
	agent := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "agent", agent.Name)
	assert.Contains(t, agent.VolumeMounts, struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		ReadOnly  bool   `json:"readOnly"`
	}{Name: "agent-config", MountPath: "/home/agent/.agents/skills", ReadOnly: true})

	var instructionsReference string
	for _, environment := range agent.Env {
		if environment.Name == "AGENT_INSTRUCTIONS" && environment.ValueFrom != nil {
			instructionsReference = environment.ValueFrom.ConfigMapKeyRef.Name + "/" + environment.ValueFrom.ConfigMapKeyRef.Key
		}
	}

	assert.Equal(t, "session-example-agent/instructions", instructionsReference)

	var skills []struct {
		Key  string `json:"key"`
		Path string `json:"path"`
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == "agent-config" {
			require.NotNil(t, volume.ConfigMap)
			assert.Equal(t, "session-example-agent", volume.ConfigMap.Name)
			skills = volume.ConfigMap.Items
		}
	}

	assert.Equal(t, []struct {
		Key  string `json:"key"`
		Path string `json:"path"`
	}{{Key: "skill-code-review", Path: "code-review/SKILL.md"}}, skills)
}

func TestRenderContinuationReusesAgentConfiguration(t *testing.T) {
	spec := validSpec()
	spec.TaskName = "example-continue-abc123"
	spec.Resume = true
	spec.AgentName = "reviewer"
	spec.Skills = []AgentSkill{{Name: "code-review", Contents: "retained skill"}}

	manifest, err := RenderContinuation(spec)
	require.NoError(t, err)
	resources := decodeRenderedResources(t, manifest)

	assert.NotContains(t, resources, "ConfigMap/session-example-agent")
	job := resources["Job/example-continue-abc123"]
	assert.Equal(t, "session-example-agent", renderedStorage(job)["agent-config"])
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == "agent-config" {
			assert.Equal(t, "code-review/SKILL.md", volume.ConfigMap.Items[0].Path)
		}
	}
}

func TestRenderAgentWithoutSkillsDoesNotProjectInstructionsAsSkill(t *testing.T) {
	spec := validSpec()
	spec.AgentName = "reviewer"
	spec.Instructions = "Review correctness and tests."

	manifest, err := Render(spec)
	require.NoError(t, err)
	resources := decodeRenderedResources(t, manifest)

	job := resources["Job/example"]
	assert.NotContains(t, renderedStorage(job), "agent-config")
	agent := job.Spec.Template.Spec.Containers[0]
	assert.NotContains(t, agent.VolumeMounts, struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		ReadOnly  bool   `json:"readOnly"`
	}{Name: "agent-config", MountPath: "/home/agent/.agents/skills", ReadOnly: true})
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
		{
			name: "oversized agent configuration",
			change: func(s *Spec) {
				s.AgentName = "reviewer"
				s.Instructions = strings.Repeat("x", 900*1024+1)
			},
			message: "instructions and skills",
		},
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
		case volume.ConfigMap != nil:
			storage[volume.Name] = volume.ConfigMap.Name
		case volume.EmptyDir["medium"] == "Memory":
			storage[volume.Name] = "memory"
		case volume.EmptyDir != nil:
			storage[volume.Name] = "ephemeral"
		}
	}

	return storage
}
