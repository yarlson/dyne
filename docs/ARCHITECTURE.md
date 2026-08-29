# Architecture

## Product boundary

The repository controls coding sessions on Kubernetes. A coding session can be a bounded exploration, a persistent update, or a resumable long session. Pull-request publishing is explicit and separate from agent execution.

`internal/codingsession` is the entrypoint-neutral product boundary. The CLI and any future in-repository entrypoint use its request and result types rather than Kubernetes, GitHub, or manifest types.

## Modules

### `cmd/agentctl`

Owns flags, file and environment input, terminal streams, output formatting, signal handling, and process exit. Its only internal dependency is `internal/codingsession`.

### `internal/codingsession`

Owns complete session operations: bootstrap, start, status, logs, task execution, shell access, stop, resume, delete, destroy, and publish. It hides sequencing and translates lower-level results into coding-session contracts.

### `internal/sessionmanifest`

Owns session validation and Kubernetes resource rendering. It determines the workload and storage shape for explore, update, and long modes without communicating with a cluster.

### `internal/kubernetes`

Owns kubeconfig loading, Kubernetes API operations, execution streams, resource status, lifecycle changes, and publisher Jobs. It does not own CLI parsing or pull-request policy.

### `internal/publish`

Owns publish eligibility, intent identity, branch ownership, retry recovery, pull-request sequencing, and publisher cleanup. It checks authoritative Kubernetes and GitHub state before recovering an ambiguous outcome.

### `internal/github`

Owns supported GitHub repository URLs, authenticated commit identity, branch visibility, and pull-request API operations.

### `container/`

Owns the runtime image and entrypoint. It prepares the workspace, runs setup commands and Codex tasks, keeps long sessions idle, and publishes a workspace through a clean clone.

## Lifecycle invariants

- Explore sessions use temporary storage and cannot be published.
- Update sessions retain workspace, tool-home, and Codex-state storage after their bounded task.
- Long sessions retain the same state across stop and resume.
- `delete` removes compute and retains persistent storage.
- `destroy` removes compute and persistent storage.
- A completed update session or stopped long session can be published.
- Publishing never force-pushes and a repeated matching request recovers its branch or pull request.

## Trust boundaries

The Pod is the command-execution boundary. The current Colima setup is intended for trusted repositories in a private cluster, not hostile multi-tenant workloads.

GitHub credentials are available to clone and publisher containers, not the coding agent. Codex credentials are unavailable during repository setup. Prompts and setup commands are visible in Kubernetes workload specifications and must not contain secrets.

## Runtime dependencies

Kubernetes and GitHub are external integration boundaries. Docker and Colima build and host the local runtime image. These tools are not imported into the product API and ordinary Go tests do not require them.
