package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yarlson/dyne/internal/agent"
	"github.com/yarlson/dyne/internal/workflow"
)

const maxRequestBytes = 1 << 20

// Config contains HTTP server dependencies.
type Config struct {
	// ErrorOutput receives full operation errors that are unsafe to return to clients.
	ErrorOutput io.Writer
	// Workflows enables durable workflow routes when configured.
	Workflows workflowOperations
}

type operations interface {
	Agents() []agent.AgentSummary
	Start(context.Context, agent.StartRequest) (agent.StartResult, error)
	Continue(context.Context, agent.ContinueRequest) (agent.TaskResult, error)
	Status(context.Context, string) (agent.Status, error)
	Artifacts(context.Context, string) (agent.Artifacts, error)
	WriteLogs(context.Context, string, bool, io.Writer) error
	Delete(context.Context, string) error
	Destroy(context.Context, string) error
	Publish(context.Context, agent.PublishRequest) (agent.PublishResult, error)
}

type workflowOperations interface {
	Workflows() []workflow.Summary
	Start(context.Context, workflow.StartRequest) (workflow.Run, error)
	Get(context.Context, string) (workflow.Run, error)
	Artifacts(context.Context, string) (workflow.Artifacts, error)
	Cancel(context.Context, string) error
	Delete(context.Context, string) error
}

// New returns the private-network HTTP control plane for one cluster and namespace.
func New(control operations, config Config) http.Handler {
	if config.ErrorOutput == nil {
		config.ErrorOutput = io.Discard
	}

	server := &server{control: control, config: config}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agents", server.listAgents)
	mux.HandleFunc("POST /v1/agents/{agent}/sessions", server.createAgentSession)
	mux.HandleFunc("GET /v1/sessions/{name}", server.sessionStatus)
	mux.HandleFunc("POST /v1/sessions/{name}/tasks", server.continueSession)
	mux.HandleFunc("GET /v1/sessions/{name}/logs", server.sessionLogs)
	mux.HandleFunc("GET /v1/sessions/{name}/artifacts", server.sessionArtifacts)
	mux.HandleFunc("POST /v1/sessions/{name}/publish", server.publishSession)
	mux.HandleFunc("DELETE /v1/sessions/{name}", server.deleteSession)
	if config.Workflows != nil {
		mux.HandleFunc("GET /v1/workflows", server.listWorkflows)
		mux.HandleFunc("POST /v1/workflows/{workflow}/runs", server.createWorkflowRun)
		mux.HandleFunc("GET /v1/workflow-runs/{name}", server.workflowRunStatus)
		mux.HandleFunc("GET /v1/workflow-runs/{name}/artifacts", server.workflowRunArtifacts)
		mux.HandleFunc("POST /v1/workflow-runs/{name}/cancel", server.cancelWorkflowRun)
		mux.HandleFunc("DELETE /v1/workflow-runs/{name}", server.deleteWorkflowRun)
	}

	return mux
}

func (s *server) listWorkflows(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, struct {
		Workflows []workflow.Summary `json:"workflows"`
	}{Workflows: s.config.Workflows.Workflows()})
}

func (s *server) createWorkflowRun(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		Prompt     string `json:"prompt"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	run, err := s.config.Workflows.Start(request.Context(), workflow.StartRequest{
		Workflow: request.PathValue("workflow"), Name: input.Name,
		Repository: input.Repository, Ref: input.Ref, Prompt: input.Prompt,
	})
	if err != nil {
		s.writeWorkflowError(writer, err)

		return
	}

	writeJSON(writer, http.StatusAccepted, run)
}

func (s *server) workflowRunStatus(writer http.ResponseWriter, request *http.Request) {
	run, err := s.config.Workflows.Get(request.Context(), request.PathValue("name"))
	if err != nil {
		s.writeWorkflowError(writer, err)

		return
	}

	writeJSON(writer, http.StatusOK, run)
}

func (s *server) workflowRunArtifacts(writer http.ResponseWriter, request *http.Request) {
	artifacts, err := s.config.Workflows.Artifacts(request.Context(), request.PathValue("name"))
	if err != nil {
		s.writeWorkflowError(writer, err)

		return
	}

	writeJSON(writer, http.StatusOK, artifacts)
}

func (s *server) cancelWorkflowRun(writer http.ResponseWriter, request *http.Request) {
	if err := s.config.Workflows.Cancel(request.Context(), request.PathValue("name")); err != nil {
		s.writeWorkflowError(writer, err)

		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

func (s *server) deleteWorkflowRun(writer http.ResponseWriter, request *http.Request) {
	if err := s.config.Workflows.Delete(request.Context(), request.PathValue("name")); err != nil {
		s.writeWorkflowError(writer, err)

		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

type server struct {
	control operations
	config  Config
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
		Agents []agent.AgentSummary `json:"agents"`
	}{Agents: s.control.Agents()})
}

func (s *server) createAgentSession(writer http.ResponseWriter, request *http.Request) {
	var input createAgentSessionRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)

		return
	}

	var timeout time.Duration
	if input.TimeoutSeconds != nil {
		timeout = time.Duration(*input.TimeoutSeconds) * time.Second
	}

	result, err := s.control.Start(request.Context(), agent.StartRequest{
		Agent:      request.PathValue("agent"),
		Name:       input.Name,
		Repository: input.Repository,
		InitialRef: input.InitialRef,
		Prompt:     input.Prompt,
		Timeout:    timeout,
	})
	if err != nil {
		s.writeOperationError(writer, err)

		return
	}

	writeJSON(writer, http.StatusAccepted, result)
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

	var timeout time.Duration
	if input.TimeoutSeconds != nil {
		timeout = time.Duration(*input.TimeoutSeconds) * time.Second
	}

	name := request.PathValue("name")
	result, err := s.control.Continue(request.Context(), agent.ContinueRequest{
		Name: name, Prompt: input.Prompt, Timeout: timeout,
	})
	if err != nil {
		s.writeOperationError(writer, err)

		return
	}

	writeJSON(writer, http.StatusAccepted, result)
}

func (s *server) sessionStatus(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	status, err := s.control.Status(request.Context(), name)
	if err != nil {
		s.writeOperationError(writer, err)

		return
	}

	writeJSON(writer, http.StatusOK, struct {
		Name      string                 `json:"name"`
		Resources []agent.ResourceStatus `json:"resources"`
	}{Name: name, Resources: status.Resources})
}

func (s *server) sessionLogs(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/x-ndjson")
	stream := &trackingResponseWriter{ResponseWriter: writer}
	err := s.control.WriteLogs(request.Context(), request.PathValue("name"), request.URL.Query().Get("follow") == "true", stream)
	if err != nil {
		if stream.wroteResponse {
			s.logOperationError(err)

			return
		}

		s.writeOperationError(writer, err)
	}
}

func (s *server) sessionArtifacts(writer http.ResponseWriter, request *http.Request) {
	result, err := s.control.Artifacts(request.Context(), request.PathValue("name"))
	if err != nil {
		s.writeOperationError(writer, err)

		return
	}

	writeJSON(writer, http.StatusOK, result)
}

func (s *server) deleteSession(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	var err error
	if request.URL.Query().Get("storage") == "delete" {
		err = s.control.Destroy(request.Context(), name)
	} else {
		err = s.control.Delete(request.Context(), name)
	}

	if err != nil {
		s.writeOperationError(writer, err)

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

	result, err := s.control.Publish(request.Context(), agent.PublishRequest{
		Name:          request.PathValue("name"),
		Branch:        input.Branch,
		BaseBranch:    input.BaseBranch,
		CommitMessage: input.CommitMessage,
		Draft:         !input.Ready,
		Timeout:       timeout,
	})
	if err != nil {
		s.writeOperationError(writer, err)

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

func (s *server) writeOperationError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "operation failed"
	switch agent.ErrorKindOf(err) {
	case agent.ErrorInvalid:
		status, message = http.StatusBadRequest, err.Error()
	case agent.ErrorNotFound:
		status, message = http.StatusNotFound, err.Error()
	case agent.ErrorUnavailable:
		status, message = http.StatusServiceUnavailable, err.Error()
	}

	if status >= http.StatusInternalServerError {
		s.logOperationError(err)
	}

	writeError(writer, status, errors.New(message))
}

func (s *server) writeWorkflowError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "operation failed"
	switch workflow.ErrorKindOf(err) {
	case workflow.ErrorInvalid:
		status, message = http.StatusBadRequest, err.Error()
	case workflow.ErrorNotFound:
		status, message = http.StatusNotFound, err.Error()
	case workflow.ErrorConflict:
		status, message = http.StatusConflict, err.Error()
	case workflow.ErrorUnavailable:
		status, message = http.StatusServiceUnavailable, err.Error()
	}

	if status >= http.StatusInternalServerError {
		s.logOperationError(err)
	}

	writeError(writer, status, errors.New(message))
}

func (s *server) logOperationError(err error) {
	detail := errors.Unwrap(err)
	if detail == nil {
		detail = err
	}

	_, _ = fmt.Fprintf(s.config.ErrorOutput, "control-plane operation failed: %v\n", detail)
}

type trackingResponseWriter struct {
	http.ResponseWriter
	wroteResponse bool
}

func (w *trackingResponseWriter) WriteHeader(status int) {
	w.wroteResponse = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingResponseWriter) Write(contents []byte) (int, error) {
	w.wroteResponse = true

	return w.ResponseWriter.Write(contents)
}

func (w *trackingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
