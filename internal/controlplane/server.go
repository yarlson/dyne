package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yarlson/airlock/pkg/agentsandbox"
)

const maxRequestBytes = 1 << 20

// Config contains the single-cluster defaults owned by one control-plane server.
type Config struct {
	// Namespace owns every session managed by this server.
	Namespace string
	// Image is the default coding-agent image.
	Image string
	// StorageSize is the default retained-session claim size.
	StorageSize string
	// TaskTimeout is the default Job deadline.
	TaskTimeout time.Duration
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
	mux.HandleFunc("POST /v1/sessions", server.createSession)
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

type createSessionRequest struct {
	Name           string `json:"name"`
	Storage        string `json:"storage"`
	Repository     string `json:"repository"`
	InitialRef     string `json:"ref"`
	SetupCommand   string `json:"setup"`
	Prompt         string `json:"prompt"`
	CloneDepth     *int   `json:"clone_depth"`
	StorageSize    string `json:"storage_size"`
	TimeoutSeconds *int64 `json:"timeout_seconds"`
}

func (s *server) createSession(writer http.ResponseWriter, request *http.Request) {
	var input createSessionRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	cloneDepth := 1
	if input.CloneDepth != nil {
		cloneDepth = *input.CloneDepth
	}

	storageSize := input.StorageSize
	if storageSize == "" {
		storageSize = s.config.StorageSize
	}

	timeout := s.config.TaskTimeout
	if input.TimeoutSeconds != nil {
		timeout = time.Duration(*input.TimeoutSeconds) * time.Second
	}

	if input.InitialRef == "" {
		input.InitialRef = "main"
	}

	err := s.control.Start(request.Context(), agentsandbox.StartRequest{
		Target:       agentsandbox.Target{Namespace: s.config.Namespace, Name: input.Name},
		Image:        s.config.Image,
		Storage:      agentsandbox.Storage(input.Storage),
		Repository:   input.Repository,
		InitialRef:   input.InitialRef,
		SetupCommand: input.SetupCommand,
		Prompt:       input.Prompt,
		CloneDepth:   cloneDepth,
		StorageSize:  storageSize,
		Timeout:      timeout,
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	writeJSON(writer, http.StatusAccepted, map[string]string{"name": input.Name, "task_id": input.Name})
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

	return strings.ToLower(hex.EncodeToString(contents)), nil
}
