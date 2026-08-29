package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yarlson/airlock/internal/agentconfig"
	"github.com/yarlson/airlock/pkg/agentsandbox"
)

const maxRequestBytes = 1 << 20

// Config contains the single-cluster defaults owned by one control-plane server.
type Config struct {
	// Namespace owns every session managed by this server.
	Namespace string
	// Image is the default coding-agent image.
	Image string
	// TaskTimeout is the default Job deadline.
	TaskTimeout time.Duration
	// Agents contains the reusable agent definitions available to clients.
	Agents *agentconfig.Catalog
}

type operations interface {
	Start(context.Context, agentsandbox.StartRequest) error
	Continue(context.Context, agentsandbox.ContinueRequest) error
	Status(context.Context, agentsandbox.Target) (agentsandbox.Status, error)
	Artifacts(context.Context, agentsandbox.Target) (agentsandbox.Artifacts, error)
	WriteLogs(context.Context, agentsandbox.LogRequest, io.Writer) error
	Delete(context.Context, agentsandbox.Target) error
	Destroy(context.Context, agentsandbox.Target) error
	Publish(context.Context, agentsandbox.PublishRequest) (agentsandbox.PublishResult, error)
}

type taskIDGenerator func() (string, error)

// New returns the private-network HTTP control plane for one cluster and namespace.
func New(control operations, config Config, newTaskID taskIDGenerator) http.Handler {
	if newTaskID == nil {
		newTaskID = randomTaskID
	}

	server := &server{control: control, config: config, newTaskID: newTaskID}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agents", server.listAgents)
	mux.HandleFunc("POST /v1/agents/{agent}/sessions", server.createAgentSession)
	mux.HandleFunc("GET /v1/sessions/{name}", server.sessionStatus)
	mux.HandleFunc("POST /v1/sessions/{name}/tasks", server.continueSession)
	mux.HandleFunc("GET /v1/sessions/{name}/logs", server.sessionLogs)
	mux.HandleFunc("GET /v1/sessions/{name}/artifacts", server.sessionArtifacts)
	mux.HandleFunc("POST /v1/sessions/{name}/publish", server.publishSession)
	mux.HandleFunc("DELETE /v1/sessions/{name}", server.deleteSession)

	return mux
}

type server struct {
	control   operations
	config    Config
	newTaskID taskIDGenerator
}

type createAgentSessionRequest struct {
	Name           string `json:"name"`
	Repository     string `json:"repository"`
	InitialRef     string `json:"ref"`
	Prompt         string `json:"prompt"`
	TimeoutSeconds *int64 `json:"timeout_seconds"`
}

func (s *server) listAgents(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, struct {
		Agents []agentconfig.Summary `json:"agents"`
	}{Agents: s.config.Agents.List()})
}

func (s *server) createAgentSession(writer http.ResponseWriter, request *http.Request) {
	agentName := request.PathValue("agent")
	definition, found := s.config.Agents.Find(agentName)
	if !found {
		writeError(writer, http.StatusNotFound, fmt.Errorf("agent %s is not configured", agentName))

		return
	}

	var input createAgentSessionRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	if input.InitialRef == "" {
		input.InitialRef = "main"
	}

	timeout := definition.Timeout
	if input.TimeoutSeconds != nil {
		timeout = time.Duration(*input.TimeoutSeconds) * time.Second
	}

	err := s.control.Start(request.Context(), agentsandbox.StartRequest{
		Target:       agentsandbox.Target{Namespace: s.config.Namespace, Name: input.Name},
		Image:        s.config.Image,
		Storage:      agentsandbox.Storage(definition.Storage),
		Repository:   input.Repository,
		InitialRef:   input.InitialRef,
		SetupCommand: definition.SetupCommand,
		Prompt:       input.Prompt,
		AgentName:    definition.Name,
		Instructions: definition.Instructions,
		Skills:       sandboxSkills(definition.Skills),
		CloneDepth:   definition.CloneDepth,
		StorageSize:  definition.StorageSize,
		Timeout:      timeout,
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	writeJSON(writer, http.StatusAccepted, map[string]string{
		"agent": definition.Name, "name": input.Name, "task_id": input.Name,
	})
}

func (s *server) continueSession(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Prompt         string `json:"prompt"`
		TimeoutSeconds *int64 `json:"timeout_seconds"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	taskID, err := s.newTaskID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)

		return
	}

	timeout := s.config.TaskTimeout
	if input.TimeoutSeconds != nil {
		timeout = time.Duration(*input.TimeoutSeconds) * time.Second
	}

	name := request.PathValue("name")
	err = s.control.Continue(request.Context(), agentsandbox.ContinueRequest{
		Target:  agentsandbox.Target{Namespace: s.config.Namespace, Name: name},
		TaskID:  taskID,
		Prompt:  input.Prompt,
		Timeout: timeout,
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	writeJSON(writer, http.StatusAccepted, map[string]string{"name": name, "task_id": taskID})
}

func (s *server) sessionStatus(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	status, err := s.control.Status(request.Context(), agentsandbox.Target{Namespace: s.config.Namespace, Name: name})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	writeJSON(writer, http.StatusOK, struct {
		Name      string                        `json:"name"`
		Resources []agentsandbox.ResourceStatus `json:"resources"`
	}{Name: name, Resources: status.Resources})
}

func (s *server) sessionLogs(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/x-ndjson")
	err := s.control.WriteLogs(request.Context(), agentsandbox.LogRequest{
		Target: agentsandbox.Target{Namespace: s.config.Namespace, Name: request.PathValue("name")},
		Follow: request.URL.Query().Get("follow") == "true",
	}, writer)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
	}
}

func (s *server) sessionArtifacts(writer http.ResponseWriter, request *http.Request) {
	result, err := s.control.Artifacts(request.Context(), agentsandbox.Target{
		Namespace: s.config.Namespace,
		Name:      request.PathValue("name"),
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	writeJSON(writer, http.StatusOK, result)
}

func (s *server) deleteSession(writer http.ResponseWriter, request *http.Request) {
	target := agentsandbox.Target{Namespace: s.config.Namespace, Name: request.PathValue("name")}
	var err error
	if request.URL.Query().Get("storage") == "delete" {
		err = s.control.Destroy(request.Context(), target)
	} else {
		err = s.control.Delete(request.Context(), target)
	}

	if err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

func (s *server) publishSession(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Branch         string `json:"branch"`
		BaseBranch     string `json:"base"`
		CommitMessage  string `json:"commit_message"`
		Ready          bool   `json:"ready"`
		TimeoutSeconds *int64 `json:"timeout_seconds"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	timeout := 10 * time.Minute
	if input.TimeoutSeconds != nil {
		timeout = time.Duration(*input.TimeoutSeconds) * time.Second
	}

	result, err := s.control.Publish(request.Context(), agentsandbox.PublishRequest{
		Target:        agentsandbox.Target{Namespace: s.config.Namespace, Name: request.PathValue("name")},
		Branch:        input.Branch,
		BaseBranch:    input.BaseBranch,
		CommitMessage: input.CommitMessage,
		Draft:         !input.Ready,
		Timeout:       timeout,
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	writeJSON(writer, http.StatusOK, result)
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}

	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}

func randomTaskID() (string, error) {
	contents := make([]byte, 6)
	if _, err := rand.Read(contents); err != nil {
		return "", err
	}

	return hex.EncodeToString(contents), nil
}

func sandboxSkills(skills []agentconfig.Skill) []agentsandbox.AgentSkill {
	if len(skills) == 0 {
		return nil
	}

	result := make([]agentsandbox.AgentSkill, len(skills))
	for i, skill := range skills {
		result[i] = agentsandbox.AgentSkill{Name: skill.Name, Contents: skill.Contents}
	}

	return result
}
