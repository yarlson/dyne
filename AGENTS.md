# Repository instructions

This repository contains a Go CLI and control plane for running coding sessions on Kubernetes. It supports bounded exploration, persistent code updates, resumable long sessions, setup commands, retained tool state, and explicit pull-request publishing.

## Commands

- Build: `make build`
- Test: `make test`
- Race test: `make test-race`
- Test one package: `go test -race -run TestName ./path/to/package`
- Coverage: `make coverage`
- Format: `make fmt`
- Lint: `make lint`
- Vet: `make vet`
- Run all required checks: `make check`
- Build the local image: `make image`
- Run the live Kubernetes test: `make integration-test KUBERNETES_INTEGRATION_CONTEXT=colima-codex-proof`
- Run the real coding-session E2E suite with the Kubernetes context, Docker context, Codex auth file, and GitHub App values documented in `README.md`.

Run `make doctor` when the local toolchain is uncertain. Install the pinned linter with `make tools`. Ordinary checks must not install tools implicitly.

## Working approach

- Inspect the closest existing code, tests, configuration, and naming before changing anything. Extend a healthy local pattern instead of introducing a second one.
- Define the smallest safe design before implementation. State the behavior, files, data flow, error paths, tests, and behavior that will remain unchanged.
- Keep the change limited to the request. Do not add abstractions, dependencies, configuration, automation, or documentation without a current consumer or a concrete risk.
- Prefer short units, shallow control flow, explicit ownership, and direct data flow. Avoid speculative interfaces, generic helpers, hidden global state, and concurrency without clear cancellation and cleanup.
- Treat public APIs, CLI behavior, serialized manifests, configuration, stored state, and external protocols as contracts. Preserve established behavior unless the task explicitly changes it.
- For multi-step state changes, define the source of truth and success state. Make retries safe, check authoritative state after ambiguous outcomes, and preserve cancellation.
- Require explicit authorization before commits, pushes, pull requests, installations, or live-system mutations. Read-only inspection and repository-native checks do not require extra approval.

Before implementation, report the existing pattern, how the change will follow it, and any necessary deviation. Before closeout, reread the request, review every changed file and the final diff, and rerun checks affected by the final edit.

## Architecture

`internal/session`, `internal/publish`, `internal/workflow`, and `internal/agent` are entrypoint-neutral product boundaries. The HTTP server uses their requests and results instead of Kubernetes or external SDK types.

- `cmd/dyne` owns server and API-client flags, environment and file input, terminal streams, signal handling, and process exit.
- `internal/controlplane` owns the private HTTP contract for sessions, tasks, logs, artifacts, publishing, and deletion.
- `internal/agent` owns configured agent definitions and resolves them into immutable session definitions.
- `internal/session` owns session lifecycle policy, validation, concurrency, repository credential refresh, and publish eligibility.
- `internal/workflow` owns durable multi-session orchestration, dependency scheduling, cancellation, and run artifacts.
- `internal/publish` owns publish intent identity, branch ownership, retry recovery, pull-request sequencing, and publisher cleanup.
- `internal/storage` owns the SQLite and PostgreSQL connection, schema, and repositories for session, workflow, and publish state.
- `internal/kubernetes` owns kubeconfig, in-cluster, and EKS connection authentication.
- `internal/workload` owns low-level task and publisher execution contracts, Kubernetes resource validation and rendering, API operations, retained filesystem projections, and Job execution. `internal/session` and `internal/publish` translate their durable domain state into workload requests and own every state transition.
- `internal/github` owns GitHub App installation authentication, supported repository URLs, commit identity, branch visibility, and pull-request operations.
- `container/` owns the runtime image and entrypoint. It prepares workspaces, runs bounded Codex tasks, validates result artifacts, and publishes through a clean clone.

Keep dependencies directional:

```text
cmd/dyne -> internal/controlplane
cmd/dyne -> internal/github
cmd/dyne -> internal/agent
cmd/dyne -> internal/session
cmd/dyne -> internal/publish
cmd/dyne -> internal/workflow
cmd/dyne -> internal/kubernetes
cmd/dyne -> internal/storage
cmd/dyne -> internal/workload
internal/controlplane -> internal/agent
internal/controlplane -> internal/session
internal/controlplane -> internal/publish
internal/controlplane -> internal/workflow
internal/agentconfig -> internal/agent
internal/agent -> internal/session
internal/workflow -> internal/session
internal/publish -> internal/session
internal/publish -> internal/github
internal/session -> internal/workload
internal/publish -> internal/workload
internal/storage -> internal/session
internal/storage -> internal/publish
internal/storage -> internal/workflow
```

Do not bypass an owning module. Keep external SDK types inside their integration package and translate them at the boundary.

## Product and trust invariants

- Ephemeral sessions use temporary storage and cannot continue or publish.
- Persistent sessions retain workspace, tool-home, Codex state, and artifacts on one PVC. SQL retains their immutable definition, task history, and validated results.
- Every initial or continued task is a bounded Job. Only one task may be active per persistent session.
- `delete` removes compute resources and retains persistent storage.
- `delete --storage` removes compute resources and persistent storage.
- Only a successfully completed persistent session can be published.
- Publishing is explicit, idempotent, non-force-pushing, and recoverable after an ambiguous outcome.

The Pod is the command-execution boundary. The setup is for trusted repositories in a private cluster, not hostile multi-tenant workloads. The HTTP API has no application authentication and must remain behind a private network boundary.

Short-lived GitHub App installation credentials are available to clone and publisher containers, not the coding agent. Codex credentials are unavailable during repository setup. Prompts and setup commands are visible in Kubernetes workload specifications and must not contain secrets.

Kubernetes and GitHub are external integration boundaries. Docker and Colima build and host the local runtime image. These tools stay outside the product API, and ordinary Go tests do not require them.

## Go conventions

- Use package names that describe owned concepts. Do not create generic packages such as `util`, `common`, `service`, or `api`.
- Define an interface beside its consumer. Add one only when current behavior needs another implementation or a focused substitute at an external boundary.
- Choose names that state the result or action. Use the same domain term and verb for the same concept; avoid vague names such as `process`, `handle`, or `manage`.
- Pass `context.Context` through Kubernetes, GitHub, streaming, waiting, and execution calls.
- Return errors outside `main`. Wrap underlying causes with `%w`, use lowercase messages without trailing punctuation, and include the failed operation and a safe identifier.
- Do not retry a non-idempotent operation until authoritative state establishes the previous outcome.
- Keep streams explicit. The command entrypoint owns process-global arguments, environment variables, application input files, and standard streams. `internal/kubernetes` owns kubeconfig loading.
- Validate untrusted names, URLs, branches, and timeouts before changing state.
- Never put credentials in errors, logs, prompts, setup commands, repository URLs, serialized results, or test fixtures.
- Close streams and response bodies. Stop timers, signal subscriptions, and goroutines. Bound waits and background work with contexts and timeouts. Pair terminal restoration with terminal setup.
- Prefer the standard library and existing dependencies. Add a module only when it materially reduces current complexity or risk.
- Never invoke `kubectl` from product code. Connection loading uses `client-go` in `internal/kubernetes`; workload operations use `client-go` in `internal/workload`.

The repository uses gofumpt, goimports, and gci through golangci-lint. Imports are grouped as standard library, external packages, then `github.com/yarlson/dyne` packages. Separate a return or branch from preceding work, but keep a function containing only that return compact. Add a blank line after a block before the next statement. `make fmt-check` verifies formatting without changing files; `make check` runs the same check in CI.

## Tests

Tests are product contracts. Each test must make the condition, action, observable result, and preserved state clear.

Use red-green-refactor for behavior changes: add the smallest failing test, make the minimum production change that passes, then simplify while focused and surrounding tests remain green. Run `make test` while iterating and `make check` before review.

- Use Testify function APIs. Use `require` for prerequisites that make later assertions unsafe and `assert` for independent outcomes. Do not introduce suites only for consistency.
- Test normal behavior, important boundaries, meaningful failures, cleanup, retained state, and externally visible side effects.
- Derive expected values from a fixed example or the behavior contract. Never calculate them with the same constants, mappings, templates, schemas, or dependency used by production code.
- A mock returning an arranged value does not prove behavior unless that wiring is the contract and another independent assertion proves the outcome.
- Prefer direct tests. Use a table only when every case shares setup, action, and assertions. Split cases that require branching setup or different lifecycle expectations.
- Keep scenario inputs and expected results visible. Extract only cohesive repeated setup.
- Use Kubernetes fake clients for cluster state and errors, `httptest.Server` for GitHub HTTP behavior, and small fixed examples for manifest output.
- Keep CLI parsing tests independent of kubeconfig and external processes where possible.
- Avoid arbitrary sleeps, timing assumptions, shared mutable state, real credentials, and ordering dependencies. Use bounded polling only when waiting is part of the contract.
- Do not call `t.Parallel` from a test that changes process-global or live-cluster state.

Ordinary tests must not contact Docker, Kubernetes, Codex, or GitHub. Live tests require the `integration` build tag, explicit opt-in, isolated resources, bounded execution, and verified cleanup. Run the Kubernetes integration test with:

```bash
make integration-test KUBERNETES_INTEGRATION_CONTEXT=colima-codex-proof
```

The integration test creates a unique namespace and verifies its deletion during cleanup.

The E2E suite builds the current image and runs a real Codex coding session against `lokalise/ratchet-test-service`. It clones through a GitHub App installation, publishes a real draft pull request, verifies the changed repository content, then closes the pull request, deletes its branch, and deletes its unique namespace. It requires the explicit live credentials documented in `README.md`; never run it as an ordinary check.

## Comments and technical writing

Make code clear through names and structure before adding comments. Document exported symbols with current behavior. Add other comments only for required language directives, established annotations, or a contract the code cannot express. Do not narrate implementation history, restate code, or add ownerless TODOs. Correct or remove stale comments when behavior changes.

Write technical prose in plain language. State the concrete subject, action, and consequence. Preserve exact commands, paths, identifiers, protocol terms, and required wording.

## CLI and operational safety

- Keep help and errors self-contained for humans and automation.
- Include the relevant flag name in input errors.
- Keep stdout stable and machine-capturable. Send each final error to stderr once.
- Do not run state-changing Colima, Kubernetes, or GitHub commands during ordinary checks.

## Completion report

Report what changed, why it is the smallest safe approach, quality risks considered, tests and checks run, intentionally deferred cleanup, and remaining risks. List every new code comment separately.
