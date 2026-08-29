# Testing conventions

## Required workflow

Use red-green-refactor for behavior changes:

1. Add the smallest test that fails for the missing or incorrect behavior.
2. Make the minimum production change that passes.
3. Simplify while keeping focused and surrounding tests green.

Run `make test` during development and `make check` before review. `make check` includes formatting, module verification, vet, lint, race tests, and a build.

## Test contracts

A test should make its condition, action, result, and preserved state obvious. Prefer direct tests with fixed independent expectations.

Do not calculate expected values with the same constants, templates, mappings, or dependencies used by production code. A mock returning its arranged value proves only wiring unless wiring is the contract being tested.

Use table-driven tests when cases share setup, action, and assertions. Keep distinct lifecycle, cleanup, concurrency, or failure behavior in separate tests.

## Boundaries

- Use Kubernetes fake clients to test resource state and errors without a cluster.
- Use `httptest.Server` to test GitHub request and response behavior.
- Test manifest output from small fixed examples, not duplicated render logic.
- Keep CLI parsing tests independent of kubeconfig and external processes where possible.
- Test cleanup and retained state for lifecycle operations that remove or stop resources.

Ordinary tests must not contact Docker, Kubernetes, Codex, or GitHub. A live test requires explicit opt-in, isolated resources, bounded execution, and cleanup that is verified even after failure.

Run the Kubernetes integration test against an isolated cluster:

```bash
make integration-test KUBERNETES_INTEGRATION_CONTEXT=colima-codex-proof
```

The `integration` build tag excludes live tests from ordinary checks. The test creates a unique namespace and verifies its deletion during cleanup.

## Concurrency and determinism

Avoid arbitrary sleeps, shared mutable state, real credentials, and ordering dependencies. Use bounded polling only when waiting is the product contract. Do not use `t.Parallel` when a test changes process-global state.
