package workload

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
		Labels      map[string]string `json:"labels"`
	} `json:"metadata"`
	Data       map[string]string `json:"data"`
	StringData map[string]string `json:"stringData"`
	Spec       struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name string `json:"name"`
					Env  []struct {
						Name      string `json:"name"`
						Value     string `json:"value"`
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
					Secret *struct {
						SecretName string `json:"secretName"`
					} `json:"secret"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func TestRenderSelectsWorkflowOutputResultContract(t *testing.T) {
	spec := validSpec()
	spec.ResultKind = ResultKindWorkflowOutput

	manifest, err := renderSessionManifest(spec)
	require.NoError(t, err)
	resources := decodeRenderedResources(t, manifest)

	var resultKind string
	for _, environment := range resources["Job/example"].Spec.Template.Spec.Containers[0].Env {
		if environment.Name == "AGENT_RESULT_KIND" {
			resultKind = environment.Value
		}
	}

	assert.Equal(t, "workflow-output", resultKind)
}

func TestRenderLabelsWorkflowOwnedSessionResources(t *testing.T) {
	spec := validSpec()
	spec.AgentName = "reviewer"
	spec.Instructions = "Review the change."
	spec.WorkflowRun = "change-123"
	spec.WorkflowStep = "security"

	manifest, err := renderSessionManifest(spec)
	require.NoError(t, err)
	resources := decodeRenderedResources(t, manifest)

	for _, identity := range []string{"Job/example", "PersistentVolumeClaim/session-example", "ConfigMap/session-example-agent"} {
		assert.Equal(t, "change-123", resources[identity].Metadata.Labels["coding-agent/workflow-run"], identity)
		assert.Equal(t, "security", resources[identity].Metadata.Labels["coding-agent/workflow-step"], identity)
	}
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
				"git-auth": "ephemeral",
				"session":  "ephemeral",
				"tmp":      "memory",
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
				"git-auth": "ephemeral",
				"session":  "session-example",
				"tmp":      "memory",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validSpec()
			spec.Storage = test.storage
			manifest, err := renderSessionManifest(spec)
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
	spec.StorageSize = ""
	spec.Resume = true
	manifest, err := renderContinuationManifest(spec)
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
	spec.Skills = []Skill{{
		Name:     "code-review",
		Contents: "---\nname: code-review\ndescription: Review code.\n---\n\nReview changed code.\n",
	}}

	manifest, err := renderSessionManifest(spec)
	require.NoError(t, err)
	resources := decodeRenderedResources(t, manifest)

	configuration := resources["ConfigMap/session-example-agent"]
	assert.True(t, configuration.Immutable)
	assert.Empty(t, configuration.Metadata.Annotations)
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

func TestRenderContinuationProjectsDurableAgentConfiguration(t *testing.T) {
	spec := validSpec()
	spec.TaskName = "example-continue-abc123"
	spec.Resume = true
	spec.AgentName = "reviewer"
	spec.Instructions = "Review correctness and tests."
	spec.Skills = []Skill{{Name: "code-review", Contents: "retained skill"}}

	manifest, err := renderContinuationManifest(spec)
	require.NoError(t, err)
	resources := decodeRenderedResources(t, manifest)

	assert.Contains(t, resources, "ConfigMap/session-example-agent")
	job := resources["Job/example-continue-abc123"]
	assert.Equal(t, "session-example-agent", renderedStorage(job)["agent-config"])
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == "agent-config" {
			assert.Equal(t, "code-review/SKILL.md", volume.ConfigMap.Items[0].Path)
		}
	}
}

func TestRenderKeepsDefinitionOutOfPVCAndScopesRepositoryCredentialToTask(t *testing.T) {
	spec := validSpec()
	spec.Repository = "https://github.com/lokalise/ratchet-test-service"
	spec.SetupCommand = "make tools"
	spec.GitCredential = "short-lived-token"

	manifest, err := renderSessionManifest(spec)
	require.NoError(t, err)
	resources := decodeRenderedResources(t, manifest)

	claim := resources["PersistentVolumeClaim/session-example"]
	assert.Empty(t, claim.Metadata.Annotations)
	credential := resources["Secret/example-git"]
	assert.Equal(t, "short-lived-token", credential.StringData["token"])
	assert.Equal(t, "example", credential.Metadata.Labels["coding-agent/task"])

	job := resources["Job/example"]
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == "git-auth" {
			require.NotNil(t, volume.Secret)
			assert.Equal(t, "example-git", volume.Secret.SecretName)
		}
	}
}

func TestRenderAgentWithoutSkillsDoesNotProjectInstructionsAsSkill(t *testing.T) {
	spec := validSpec()
	spec.AgentName = "reviewer"
	spec.Instructions = "Review correctness and tests."

	manifest, err := renderSessionManifest(spec)
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
		change  func(*sessionManifestSpec)
		message string
	}{
		{name: "unsafe name", change: func(s *sessionManifestSpec) { s.Name = "Not Safe" }, message: "name"},
		{name: "missing prompt", change: func(s *sessionManifestSpec) { s.Prompt = "" }, message: "prompt"},
		{name: "unknown storage", change: func(s *sessionManifestSpec) { s.Storage = "remote" }, message: "unsupported"},
		{name: "negative clone depth", change: func(s *sessionManifestSpec) { s.CloneDepth = -1 }, message: "clone depth"},
		{
			name: "oversized agent configuration",
			change: func(s *sessionManifestSpec) {
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
			_, err := renderSessionManifest(spec)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func validSpec() sessionManifestSpec {
	return sessionManifestSpec{
		Name:           "example",
		Namespace:      "coding-agents",
		Image:          "coding-agent:local",
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
