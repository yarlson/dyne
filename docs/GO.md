# Go conventions

## Formatting and imports

Run `make fmt` before review. The repository uses gofumpt, goimports, and gci through golangci-lint. Imports are grouped as standard library, external packages, then `coding-agent-k8s` packages.

Separate a return or branch from preceding work, but keep a function containing only that return compact. Add a blank line after block statements before the next statement.

`make fmt-check` verifies formatting without changing files. CI runs the same command through `make check`.

## Packages and dependencies

Package names describe owned concepts. Avoid generic packages such as `util`, `common`, `service`, or `api`.

Keep dependencies directional:

```text
cmd/agentctl -> internal/codingsession
internal/codingsession -> internal/sessionmanifest
internal/codingsession -> internal/kubernetes
internal/codingsession -> internal/publish
internal/publish -> internal/kubernetes
internal/publish -> internal/github
internal/kubernetes -> internal/sessionmanifest
```

Entrypoints consume `internal/codingsession`; they do not bypass it. External SDK types remain inside their integration package. Translate results at the owning boundary instead of exposing implementation types.

Define an interface in the package that consumes it. Add one only when current behavior needs more than one implementation or a focused substitute at an external boundary.

## Errors and cancellation

- Return errors instead of terminating outside `main`.
- Wrap an underlying cause with `%w`.
- Write lowercase errors without trailing punctuation.
- Include the failed operation and relevant safe identifier.
- Preserve `context.Context` cancellation through Kubernetes, GitHub, streaming, waiting, and execution calls.
- Do not retry non-idempotent work until authoritative state establishes the previous outcome.

## Input, output, and secrets

Library packages receive streams explicitly. Only the command entrypoint reads process-global arguments, environment variables, files, or standard streams.

Validate untrusted names, URLs, branches, and timeouts before changing state. Never include credentials in errors, logs, prompts, setup commands, repository URLs, or serialized results.

## Resources

Close streams and response bodies. Stop timers, signal subscriptions, and goroutines. Bound waits and background work with contexts and timeouts. Keep terminal restoration paired with terminal setup.

## Documentation

Document exported symbols with current behavior. Comments explain contracts that names and types cannot express; they do not narrate implementation history or restate code.
