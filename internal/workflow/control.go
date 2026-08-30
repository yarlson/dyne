package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/yarlson/dyne/internal/agent"
)

type sessionOperations interface {
	StartDefinition(context.Context, agent.AgentDefinition, agent.StartRequest) (agent.StartResult, error)
	Status(context.Context, string) (agent.Status, error)
	Artifacts(context.Context, string) (agent.Artifacts, error)
	Delete(context.Context, string) error
	Destroy(context.Context, string) error
}

// Control creates and reconciles durable workflow runs.
type Control struct {
	repository  Repository
	sessions    sessionOperations
	catalog     Catalog
	now         func() time.Time
	errorOutput io.Writer
	mutex       sync.Mutex
}

func newControl(repository Repository, sessions sessionOperations, catalog Catalog, now func() time.Time) *Control {
	if now == nil {
		now = time.Now
	}

	return &Control{repository: repository, sessions: sessions, catalog: catalog, now: now, errorOutput: io.Discard}
}

// Config contains durable workflow control dependencies.
type Config struct {
	// Repository retains workflow state across reconciliation attempts.
	Repository Repository
	// ErrorOutput receives transient reconciliation errors.
	ErrorOutput io.Writer
}

// New creates durable workflow control with explicit session and storage dependencies.
func New(config Config, sessions *agent.Control, catalog Catalog) (*Control, error) {
	if sessions == nil {
		return nil, errors.New("agent control is required")
	}

	if config.Repository == nil {
		return nil, errors.New("workflow repository is required")
	}

	if config.ErrorOutput == nil {
		config.ErrorOutput = io.Discard
	}

	control := newControl(config.Repository, sessions, catalog, nil)
	control.errorOutput = config.ErrorOutput

	return control, nil
}

// Workflows returns safe configured workflow metadata.
func (c *Control) Workflows() []Summary {
	if c.catalog == nil {
		return []Summary{}
	}

	return c.catalog.List()
}

// Start snapshots one workflow and creates its durable run intent.
func (c *Control) Start(ctx context.Context, request StartRequest) (Run, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	definition, err := c.validateStart(request)
	if err != nil {
		return Run{}, err
	}

	if request.Ref == "" {
		request.Ref = "main"
	}

	intent, err := workflowIntent(request, definition)
	if err != nil {
		return Run{}, err
	}

	if existing, getErr := c.repository.Run(ctx, request.Name); getErr == nil {
		if existing.Intent != intent {
			return Run{}, newOperationError(
				ErrorConflict, fmt.Sprintf("workflow run %s already exists with different inputs", request.Name), nil,
			)
		}

		return existing, nil
	} else if !errors.Is(getErr, ErrRunNotFound) {
		return Run{}, newOperationError(ErrorUnavailable, "read existing workflow run failed", getErr)
	}

	now := c.now().UTC()
	run := Run{
		Version: "v1", Name: request.Name, Workflow: definition.Name, Description: definition.Description,
		Repository: request.Repository, Ref: request.Ref, Prompt: request.Prompt, Intent: intent,
		MaxParallelism: definition.MaxParallelism, State: StatePending,
		Steps: make(map[string]Step, len(definition.Steps)), CreatedAt: now, UpdatedAt: now,
	}
	for name, definitionStep := range definition.Steps {
		run.Steps[name] = Step{
			Name: name, Agent: definitionStep.Agent, Prompt: definitionStep.Prompt,
			After: slices.Clone(definitionStep.After), Publishable: definitionStep.Publishable,
			Session: workflowSessionName(request.Name, name), State: StepPending,
		}
	}

	created, err := c.repository.Create(ctx, run, resolvedAgents(definition))
	if err != nil {
		if existing, getErr := c.repository.Run(ctx, request.Name); getErr == nil {
			if existing.Intent == intent {
				return existing, nil
			}

			return Run{}, newOperationError(
				ErrorConflict, fmt.Sprintf("workflow run %s already exists with different inputs", request.Name), nil,
			)
		}

		return Run{}, newOperationError(ErrorUnavailable, "create workflow run failed", err)
	}

	return created, nil
}

// Get returns one durable workflow run.
func (c *Control) Get(ctx context.Context, name string) (Run, error) {
	run, err := c.repository.Run(ctx, name)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			return Run{}, newOperationError(ErrorNotFound, fmt.Sprintf("workflow run %s does not exist", name), err)
		}

		return Run{}, newOperationError(ErrorUnavailable, "read workflow run failed", err)
	}

	return run, nil
}

// Artifacts returns copied workflow outputs and the optional publishable session.
func (c *Control) Artifacts(ctx context.Context, name string) (Artifacts, error) {
	run, err := c.Get(ctx, name)
	if err != nil {
		return Artifacts{}, err
	}

	result := Artifacts{Name: name, Outputs: map[string]json.RawMessage{}}
	for stepName, step := range run.Steps {
		if len(step.Output) > 0 {
			result.Outputs[stepName] = slices.Clone(step.Output)
		}

		if step.Publishable {
			result.PublishableSession = step.Session
		}
	}

	return result, nil
}

// Cancel records durable cancellation intent.
func (c *Control) Cancel(ctx context.Context, name string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	run, err := c.load(ctx, name)
	if err != nil {
		return err
	}

	if terminal(run.State) {
		return nil
	}

	run.CancelRequested = true
	run.UpdatedAt = c.now().UTC()
	_, err = c.save(ctx, run)

	return err
}

// Delete destroys every owned session and then removes durable workflow state.
func (c *Control) Delete(ctx context.Context, name string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	run, err := c.load(ctx, name)
	if err != nil {
		return err
	}

	if !terminal(run.State) && run.State != StateDeleting {
		return newOperationError(
			ErrorConflict, fmt.Sprintf("workflow run %s must be terminal before deletion", name), nil,
		)
	}

	if run.State != StateDeleting {
		run.State = StateDeleting
		run.UpdatedAt = c.now().UTC()
		if run, err = c.save(ctx, run); err != nil {
			return err
		}
	}

	for _, step := range sortedSteps(run.Steps) {
		if err := c.sessions.Destroy(ctx, step.Session); err != nil {
			return fmt.Errorf("destroy workflow run %s session %s: %w", name, step.Session, err)
		}
	}

	if err := c.repository.Delete(ctx, name); err != nil {
		return fmt.Errorf("delete workflow run %s state: %w", name, err)
	}

	return nil
}

// Reconcile advances one workflow run from current durable and session state.
func (c *Control) Reconcile(ctx context.Context, name string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	run, err := c.load(ctx, name)
	if err != nil {
		return err
	}

	if terminal(run.State) {
		return c.cleanupCompletedSteps(ctx, run)
	}

	if run.State == StateDeleting {
		return nil
	}

	if run.CancelRequested {
		return c.reconcileCancellation(ctx, run)
	}

	started, err := c.startPersistedSteps(ctx, &run)
	if err != nil {
		return err
	}

	changed := len(started) > 0

	observed, err := c.observeRunningSteps(ctx, &run)
	if err != nil {
		return err
	}

	scheduled := scheduleReadySteps(&run, c.now().UTC())
	deriveRunState(&run)
	if !changed && !observed && !scheduled {
		return nil
	}

	run.UpdatedAt = c.now().UTC()
	saved, err := c.save(ctx, run)
	if err != nil {
		if errors.Is(err, ErrConcurrentUpdate) {
			return errors.Join(err, c.cleanupCanceledStarts(ctx, run.Name, started))
		}

		return err
	}

	return c.cleanupCompletedSteps(ctx, saved)
}

// ReconcileAll advances every non-terminal run once.
func (c *Control) ReconcileAll(ctx context.Context) error {
	records, err := c.repository.Runs(ctx)
	if err != nil {
		return fmt.Errorf("list workflow runs: %w", err)
	}

	var reconcileErrors []error
	for _, run := range records {
		if (terminal(run.State) && !needsCleanup(run)) || run.State == StateDeleting {
			continue
		}

		if reconcileErr := c.Reconcile(ctx, run.Name); reconcileErr != nil {
			reconcileErrors = append(reconcileErrors, reconcileErr)
		}
	}

	return errors.Join(reconcileErrors...)
}

// Run reconciles durable workflows until the context ends.
func (c *Control) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("workflow reconcile interval must be greater than zero")
	}

	if err := c.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(c.errorOutput, "reconcile workflows: %v\n", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
				_, _ = fmt.Fprintf(c.errorOutput, "reconcile workflows: %v\n", err)
			}
		}
	}
}

func (c *Control) validateStart(request StartRequest) (Definition, error) {
	if c.catalog == nil {
		return Definition{}, newOperationError(ErrorNotFound, "no workflows are configured", nil)
	}

	definition, found := c.catalog.Find(request.Workflow)
	if !found {
		return Definition{}, newOperationError(
			ErrorNotFound, fmt.Sprintf("workflow %s is not configured", request.Workflow), nil,
		)
	}

	if messages := validation.IsDNS1123Label(request.Name); len(messages) > 0 || len(request.Name) > 31 {
		return Definition{}, newOperationError(
			ErrorInvalid, "workflow run name must be a lowercase DNS label no longer than 31 characters", nil,
		)
	}

	if strings.TrimSpace(request.Repository) == "" {
		return Definition{}, newOperationError(ErrorInvalid, "repository is required", nil)
	}

	if strings.TrimSpace(request.Prompt) == "" {
		return Definition{}, newOperationError(ErrorInvalid, "prompt is required", nil)
	}

	return definition, nil
}

func resolvedAgents(definition Definition) map[string]agent.AgentDefinition {
	agents := make(map[string]agent.AgentDefinition)
	for _, step := range definition.Steps {
		agents[step.Agent] = step.ResolvedAgent
	}

	return agents
}

func (c *Control) startPersistedSteps(ctx context.Context, run *Run) ([]string, error) {
	var started []string
	for _, name := range sortedStepNames(run.Steps) {
		step := run.Steps[name]
		if step.State != StepStarting {
			continue
		}

		definition, err := c.repository.Agent(ctx, run.Name, step.Agent)
		if err != nil {
			return nil, fmt.Errorf("read workflow run %s agent %s snapshot: %w", run.Name, step.Agent, err)
		}

		resultKind := agent.ResultKindWorkflowOutput
		if step.Publishable {
			resultKind = agent.ResultKindPullRequest
		}

		_, err = c.sessions.StartDefinition(ctx, definition, agent.StartRequest{
			Agent: step.Agent, Name: step.Session, Repository: run.Repository, InitialRef: run.Ref,
			Prompt: workflowStepPrompt(*run, step), ResultKind: resultKind,
			WorkflowRun: run.Name, WorkflowStep: step.Name,
		})
		if err != nil {
			return nil, fmt.Errorf("start workflow run %s step %s: %w", run.Name, step.Name, err)
		}

		step.State = StepRunning
		run.Steps[name] = step
		started = append(started, step.Session)
	}

	return started, nil
}

func (c *Control) cleanupCanceledStarts(ctx context.Context, runName string, sessions []string) error {
	if len(sessions) == 0 {
		return nil
	}

	value, err := c.repository.Run(ctx, runName)
	if err != nil {
		return fmt.Errorf("read workflow run after conflicting start: %w", err)
	}

	if !value.CancelRequested {
		return nil
	}

	var cleanupErrors []error
	for _, session := range sessions {
		if err := c.sessions.Delete(ctx, session); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete canceled session %s: %w", session, err))
		}
	}

	return errors.Join(cleanupErrors...)
}

func (c *Control) observeRunningSteps(ctx context.Context, run *Run) (bool, error) {
	changed := false
	for _, name := range sortedStepNames(run.Steps) {
		step := run.Steps[name]
		if step.State != StepRunning {
			continue
		}

		status, err := c.sessions.Status(ctx, step.Session)
		if err != nil {
			return false, fmt.Errorf("read workflow run %s step %s status: %w", run.Name, step.Name, err)
		}

		jobState := taskJobState(status)
		if jobState == "" || jobState == "Pending" || jobState == "Running" {
			continue
		}

		now := c.now().UTC()
		step.FinishedAt = &now
		if jobState == "Failed" {
			step.State = StepFailed
			step.Summary = "agent session failed"
			run.Steps[name] = step
			changed = true

			continue
		}

		artifacts, err := c.sessions.Artifacts(ctx, step.Session)
		if err != nil {
			return false, fmt.Errorf("read workflow run %s step %s artifacts: %w", run.Name, step.Name, err)
		}

		if err := applyStepArtifacts(&step, artifacts); err != nil {
			step.State = StepFailed
			step.Summary = err.Error()
		}

		run.Steps[name] = step
		changed = true
	}

	return changed, nil
}

func (c *Control) reconcileCancellation(ctx context.Context, run Run) error {
	now := c.now().UTC()
	for name, step := range run.Steps {
		switch step.State {
		case StepStarting, StepRunning:
			if err := c.sessions.Delete(ctx, step.Session); err != nil {
				return fmt.Errorf("cancel workflow run %s step %s: %w", run.Name, step.Name, err)
			}

			step.State = StepCanceled
			step.FinishedAt = &now
		case StepPending:
			step.State = StepCanceled
			step.FinishedAt = &now
		}

		run.Steps[name] = step
	}

	run.State = StateCanceled
	run.UpdatedAt = now
	_, err := c.save(ctx, run)

	return err
}

func (c *Control) cleanupCompletedSteps(ctx context.Context, run Run) error {
	changed := false
	for name, step := range run.Steps {
		if step.Publishable || step.State != StepCompleted || step.Cleaned {
			continue
		}

		if err := c.sessions.Delete(ctx, step.Session); err != nil {
			return fmt.Errorf("clean up workflow run %s step %s: %w", run.Name, step.Name, err)
		}

		step.Cleaned = true
		run.Steps[name] = step
		changed = true
	}

	if !changed {
		return nil
	}

	run.UpdatedAt = c.now().UTC()
	_, err := c.save(ctx, run)

	return err
}

func needsCleanup(run Run) bool {
	for _, step := range run.Steps {
		if !step.Publishable && step.State == StepCompleted && !step.Cleaned {
			return true
		}
	}

	return false
}

func scheduleReadySteps(run *Run, now time.Time) bool {
	changed := false
	active := 0
	for _, step := range run.Steps {
		if step.State == StepStarting || step.State == StepRunning {
			active++
		}
	}

	for _, name := range sortedStepNames(run.Steps) {
		step := run.Steps[name]
		if step.State != StepPending {
			continue
		}

		ready := true
		failedDependency := false
		for _, dependency := range step.After {
			switch run.Steps[dependency].State {
			case StepCompleted:
			case StepBlocked, StepFailed, StepCanceled, StepSkipped:
				failedDependency = true
			default:
				ready = false
			}
		}

		if failedDependency {
			step.State = StepSkipped
			step.FinishedAt = &now
			run.Steps[name] = step
			changed = true

			continue
		}

		if !ready || active >= run.MaxParallelism {
			continue
		}

		step.State = StepStarting
		step.StartedAt = &now
		run.Steps[name] = step
		active++
		changed = true
	}

	return changed
}

func deriveRunState(run *Run) {
	state := StateCompleted
	for _, step := range run.Steps {
		switch step.State {
		case StepPending, StepStarting, StepRunning:
			run.State = StateRunning

			return
		case StepFailed:
			state = StateFailed
		case StepBlocked:
			if state != StateFailed {
				state = StateBlocked
			}
		case StepCanceled:
			if state != StateFailed && state != StateBlocked {
				state = StateCanceled
			}
		}
	}

	run.State = state
}

func applyStepArtifacts(step *Step, artifacts agent.Artifacts) error {
	var outcome struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
		Blocker string `json:"blocker"`
	}
	if err := json.Unmarshal(artifacts.Outcome, &outcome); err != nil {
		return fmt.Errorf("decode outcome: %w", err)
	}

	step.Summary = outcome.Summary
	step.Blocker = outcome.Blocker
	switch outcome.Status {
	case "blocked":
		step.State = StepBlocked

		return nil
	case "failed":
		step.State = StepFailed

		return nil
	case "completed":
	default:
		return fmt.Errorf("unsupported outcome status %q", outcome.Status)
	}

	if step.Publishable {
		if len(artifacts.PullRequest) == 0 {
			return errors.New("completed publishable step did not report pull request metadata")
		}
	} else {
		if len(artifacts.WorkflowOutput) == 0 {
			return errors.New("completed workflow step did not report workflow output")
		}

		step.Output = slices.Clone(artifacts.WorkflowOutput)
	}

	step.State = StepCompleted

	return nil
}

func workflowStepPrompt(run Run, step Step) string {
	outputs := make(map[string]json.RawMessage, len(step.After))
	for _, dependency := range step.After {
		outputs[dependency] = run.Steps[dependency].Output
	}

	encoded, _ := json.Marshal(outputs)

	return "Workflow goal:\n" + run.Prompt + "\n\nStep task:\n" + step.Prompt + "\n\nDirect dependency outputs (JSON):\n" + string(encoded)
}

func workflowIntent(request StartRequest, definition Definition) (string, error) {
	type intentStep struct {
		Name        string   `json:"name"`
		Agent       string   `json:"agent"`
		Prompt      string   `json:"prompt"`
		After       []string `json:"after,omitempty"`
		Publishable bool     `json:"publishable,omitempty"`
	}
	steps := make([]intentStep, 0, len(definition.Steps))
	for _, name := range sortedDefinitionStepNames(definition.Steps) {
		step := definition.Steps[name]
		steps = append(steps, intentStep{
			Name: name, Agent: step.Agent, Prompt: step.Prompt, After: step.After, Publishable: step.Publishable,
		})
	}

	contents, err := json.Marshal(struct {
		Request        StartRequest `json:"request"`
		Description    string       `json:"description"`
		MaxParallelism int          `json:"max_parallelism"`
		Steps          []intentStep `json:"steps"`
	}{Request: request, Description: definition.Description, MaxParallelism: definition.MaxParallelism, Steps: steps})
	if err != nil {
		return "", fmt.Errorf("encode workflow intent: %w", err)
	}

	digest := sha256.Sum256(contents)

	return hex.EncodeToString(digest[:]), nil
}

func workflowSessionName(run, step string) string {
	digest := sha256.Sum256([]byte(step))

	return run + "-" + hex.EncodeToString(digest[:4])
}

func taskJobState(status agent.Status) string {
	for _, resource := range status.Resources {
		if resource.Kind == "Job" {
			return resource.State
		}
	}

	return ""
}

func terminal(state State) bool {
	return state == StateCompleted || state == StateBlocked || state == StateFailed || state == StateCanceled
}

func (c *Control) load(ctx context.Context, name string) (Run, error) {
	run, err := c.repository.Run(ctx, name)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			return Run{}, newOperationError(ErrorNotFound, fmt.Sprintf("workflow run %s does not exist", name), err)
		}

		return Run{}, newOperationError(ErrorUnavailable, "read workflow run failed", err)
	}

	return run, nil
}

func (c *Control) save(ctx context.Context, run Run) (Run, error) {
	updated, err := c.repository.Update(ctx, run)
	if err != nil {
		return Run{}, newOperationError(ErrorUnavailable, "update workflow run failed", err)
	}

	return updated, nil
}

func newOperationError(kind ErrorKind, message string, cause error) error {
	return &operationError{kind: kind, message: message, cause: cause}
}

func sortedStepNames(steps map[string]Step) []string {
	names := make([]string, 0, len(steps))
	for name := range steps {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

func sortedDefinitionStepNames(steps map[string]StepDefinition) []string {
	names := make([]string, 0, len(steps))
	for name := range steps {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

func sortedSteps(steps map[string]Step) []Step {
	names := sortedStepNames(steps)
	result := make([]Step, len(names))
	for i, name := range names {
		result[i] = steps[name]
	}

	return result
}
