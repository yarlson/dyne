# AGENTS.md

Go CLI and library for running coding sessions on Kubernetes. It supports bounded exploration, persistent code updates, resumable long sessions, setup commands, retained tool state, and explicit pull-request publishing.

## Commands

- Build: `make build`
- Test: `make test`
- Race test: `make test-race`
- Test one package: `go test -race -run TestName ./path/to/package`
- Format: `make fmt`
- Lint: `make lint`
- Vet: `make vet`
- All checks: `make check`
- Build the local image: `make image`

Run `make doctor` when the local toolchain is uncertain. Install the pinned linter with `make tools`; ordinary checks must not install tools implicitly.

## Architecture

- `cmd/agentctl` owns flags, environment and file input, terminal wiring, result formatting, and process exit behavior. It imports only `internal/codingsession`.
- `internal/codingsession` owns complete entrypoint-neutral session operations and translates lower-level results into its own contract.
- `internal/sessionmanifest` validates session specifications and renders Kubernetes resources without contacting a cluster.
- `internal/kubernetes` owns Kubernetes configuration, resource lifecycle, logs, execution, status, and publisher Jobs.
- `internal/publish` owns publish eligibility, idempotency, ambiguous-outcome recovery, and pull-request sequencing.
- `internal/github` owns GitHub URL validation and API operations.
- `container/` owns the runtime image and entrypoint contract.

Do not bypass an owning module. In particular, entrypoints must not import Kubernetes, publishing, GitHub, or manifest packages directly. See `docs/ARCHITECTURE.md`.

## Go conventions

- Keep packages focused and dependencies directional.
- Define interfaces beside the consumer and only when a real substitute or implementation exists.
- Pass `context.Context` through blocking or external operations.
- Return errors to the owning boundary. Wrap causes with `%w`; use lowercase messages without trailing punctuation.
- Keep streams explicit. Library packages must not assume process-global standard input or output.
- Document exported symbols with current behavior. Prefer clear names and structure over explanatory comments.
- Use the standard library and existing dependencies before adding another module.
- Never invoke `kubectl`; Kubernetes access goes through `client-go` in `internal/kubernetes`.

Formatting and import rules are in `docs/GO.md` and `.golangci.yml`.

## Tests

- Test observable behavior, meaningful failures, cleanup, and retained state.
- Derive expected values independently; do not mirror production mappings or templates in assertions.
- Prefer direct tests. Use table-driven tests only when cases share setup, action, and assertions.
- Use Kubernetes fake clients at the cluster boundary and `httptest` at the GitHub boundary.
- Ordinary tests must not contact Docker, Kubernetes, Codex, or GitHub.
- Do not add a live test unless it owns isolated resources, has explicit opt-in, and guarantees cleanup.

See `docs/TESTING.md`.

## CLI and safety

- Keep help and errors self-contained for both humans and automation.
- Include the relevant flag name in input errors.
- Keep stdout stable and machine-capturable; send final errors to stderr once.
- Never print credentials or put tokens in repository URLs, prompts, setup commands, logs, or test fixtures.
- Preserve the distinction between `stop`, `delete`, and `destroy`: stop retains compute state, delete retains storage, and destroy removes storage.
- Publishing must remain explicit, idempotent, non-force-pushing, and recoverable after an ambiguous outcome.
- Do not run state-changing Colima, Kubernetes, or GitHub commands during ordinary checks.
