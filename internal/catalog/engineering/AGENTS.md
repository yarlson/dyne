## Engineering Quality Gate

Solve the requested task while preserving or improving maintainability, correctness, simplicity, testability, and long-term engineering health.

### Core rules

- **Keep the change small.** Modify only what the task requires. Do not refactor unrelated code, change public interfaces, data models, configuration, or operational behavior, or add speculative abstractions.
- **Justify every addition.** Add code, files, abstractions, dependencies, configuration, automation, documentation, process, or policy only when required by current behavior or to mitigate a concrete risk. Every addition must have a clear consumer and purpose. Common practice elsewhere, appearance of completeness, hypothetical future use, and satisfying a tool without correcting a real defect are not sufficient reasons.
- **Preserve correctness.** Handle normal, boundary, and failure paths. Watch for partial state, ordering and concurrency bugs, nil or null values, stale reads, unsafe assumptions, and off-by-one errors. Make invalid states hard to represent where practical.
- **Keep responsibilities clear.** Avoid cleverness, hidden coupling, unnecessary indirection, and mixed concerns. Keep code boring and easy to modify.
- **Fit the codebase.** Follow existing conventions for structure, naming, errors, logging, testing, configuration, dependency wiring, and API shape. Do not introduce a second pattern without a clear reason.
- **Control complexity.** Prefer short units, shallow control flow, guard clauses, and explicit data flow. Add concurrency or complex runtime patterns only when a simpler design is insufficient.
- **Test the contract.** Add or update deterministic tests for changed product behavior, important boundaries, failure paths, and cleanup. Treat changed test code as maintained product code. For engineering tooling and infrastructure, first use the applicable native checks. Add dedicated tests only when those checks cannot credibly prove important behavior, concrete complexity or failure risk warrants regression coverage, and a focused deterministic test boundary fits the task. Do not weaken existing tests without a clear justification.
- **Surface failures.** Return, wrap, log, or expose errors according to project conventions. Add logs, metrics, traces, or health signals only when useful. Keep failures diagnosable without leaking secrets or sensitive data.
- **Protect trust boundaries.** Validate untrusted input and preserve authentication, authorization, tenant isolation, and permissions. Avoid injection, unsafe paths or deserialization, SSRF, secret exposure, insecure defaults, and unnecessary privilege or access.
- **Own resources.** Avoid inefficient work on hot paths and unbounded memory growth. Close, cancel, clean up, or release files, connections, timers, tasks, threads, and goroutines in the correct order. Bound concurrent or background work.
- **Avoid needless dependencies.** Prefer the standard library and existing dependencies. Add a dependency only when it materially reduces complexity or risk, and consider its maintenance, license, size, security, and transitive cost.

### Authorization and workflow boundaries

- Treat reviews as read-only unless the user or an authorized implementation or validation workflow explicitly permits changes.
- Delegating to another workflow does not expand the current scope, mutation authority, network access, or permission to spawn agents.
- Require explicit authority before external writes, commits, pushes, installations, generated-state changes, or live-system mutations.
- Follow repository-native commands, templates, and conventions before fallback guidance.
- Report inconclusive evidence as inconclusive. Do not turn missing context or unavailable tooling into an unsupported conclusion.

### Before finalizing

Review every changed file and confirm:

1. Every change is required for the task.
2. Complexity did not grow unnecessarily.
3. Changed units remain small, readable, and focused.
4. Errors, edge cases, cleanup, and cancellation are handled.
5. Applicable tests and native checks cover the meaningful behavior, and changed test code meets the same engineering standard as product code.
6. The implementation follows existing conventions.

Fix any failed gate before claiming the work is complete.

Re-read the request and review the final diff before closeout. Confirm that each requirement is satisfied by current evidence, required checks match the final content, directly affected documentation is current, and blockers or unrelated follow-ups are separated from completed work. If closeout requires an edit, rerun the affected checks before reporting completion.

### Report the result

Include:

- what was implemented
- why this is the smallest safe approach
- quality risks considered
- tests and checks run
- intentionally deferred cleanup
- remaining risks or follow-up work

## Existing Codebase First

Before writing code, inspect the closest existing patterns for structure, naming, errors, logging, testing, configuration, dependency wiring, and API shape.

- Extend healthy helpers, packages, conventions, and test styles instead of creating a second way to solve the same problem.
- Before adding an abstraction, package, interface, service, helper, middleware, configuration object, or dependency, check whether an equivalent already exists.
- If the existing pattern is unhealthy, explain the problem and make the smallest localized improvement. Do not silently introduce a competing pattern.

Gather only the repository context needed for the current task:

- read applicable repository instructions and linked context documents;
- identify the relevant source, tests, commands, documentation, and configuration;
- resolve cheaply discoverable facts from local evidence before asking the user;
- use current authoritative external sources only when correctness depends on unfamiliar or version-sensitive behavior;
- stop discovery when the edit surface, verification surface, constraints, and unresolved risks are clear.

Do not turn discovery into broad documentation work or inspect generated output, vendored dependencies, build artifacts, or unrelated modules without a concrete need.

Before implementation, report:

- the existing pattern and where it is used
- how the new code will follow it
- any intentional deviation and why it is necessary

## Small Design Before Code

Define the smallest safe design before implementing. The sketch must cover:

- the exact behavior being changed
- the minimal files or units involved
- the data flow and error paths
- the tests needed
- what will remain unchanged

Reject unrelated cleanup, broad refactoring, speculative abstractions, future-proofing, new frameworks, service boundaries, workers, queues, state machines, or configuration unless the task requires them.

Choose the simplest design that safely satisfies the requirement. Before finalizing, confirm the implementation still matches the sketch; explain necessary growth or reduce the change.

For behavior-preserving structural work, identify the behavior that must remain unchanged, keep the edit sequence mechanical and reversible, name rollback points when partial application is risky, and specify focused regression checks before editing.

## Module Boundaries

Draw module boundaries around coherent knowledge and changes likely to occur together. A module should own a stable domain concept, policy, representation, protocol, or other design decision and keep its changes from spreading to unrelated code.

Do not split modules by execution order, workflow step, technical layer, CRUD resource, or arbitrary size. Put code together when the same likely change affects it; separate code when it uses a different model, policy, vocabulary, or source of change.

Expose the smallest useful interface from the client's point of view. Hide storage, representation, algorithms, and integration details that clients do not need. Make errors, consistency, latency, resource ownership, and other behavior clients must rely on explicit in the contract.

Keep dependencies explicit and directional. Stable domain policy should not depend directly on volatile frameworks, storage, transport, or global registries. Translate external types and terms at the boundary. Avoid cycles, shared mutable state, duplicate knowledge, and paths that bypass the owning module.

Do not create generic buckets such as `common`, `util`, `types`, `interfaces`, or `api`. Avoid shallow wrappers that only forward calls, interfaces added only for symmetry or mocking, and modules that do not hide a real decision or contain change.

Use observed changes to test the design. If routine work crosses a boundary or requires unrelated domain knowledge, reconsider ownership or the contract. Before finalizing a boundary, name what it owns, what it hides, which callers need it, and one expected change that should remain inside it.

## Contract Evolution

Treat public APIs, schemas, serialized data, configuration, protocols, and other published behavior as contracts. Before changing one, identify its consumers, stored data, deployed versions, and compatibility requirements.

Preserve established inputs, outputs, defaults, errors, and semantics unless the task explicitly changes them. Do not assume a change is safe because known source code still compiles; unknown clients and persisted data may depend on observable behavior.

For an incompatible change, use expand, migrate, then contract:

1. Add support for the new contract without removing the old one.
2. Move consumers and data, and verify the new path in current evidence.
3. Remove the old contract only after its consumers and data are gone.

When versions may overlap, deploy readers that accept both forms before writers produce the new form. Keep migrations restartable and safe after partial completion. Preserve a working rollback path until old and new versions no longer need to coexist.

Define how invalid, missing, old, and new values behave throughout the transition. Do not silently reinterpret existing data or reuse an established name for incompatible behavior.

Before finalizing, confirm the change order, mixed-version behavior, migration evidence, removal condition, and rollback point.

## New Code Complexity Budget

Keep every new or heavily changed function, class, module, package, component, or service within a strict simplicity budget.

Prefer:

- one clear responsibility
- short units and shallow control flow
- guard clauses over nested conditions
- explicit data flow and lifecycle ownership
- clear names over explanatory comments
- composition over multipurpose objects
- deterministic behavior over hidden side effects

Avoid:

- modes, flags, branches, or nesting that obscure behavior
- catch-all services and premature interfaces
- generic helpers used only once
- hidden global state or implicit ownership
- mixing business logic, I/O, parsing, validation, and presentation
- concurrency without clear ownership, cancellation, bounds, and cleanup
- comments that explain complexity the code should remove

Before finalizing, confirm each changed unit is understandable without unrelated files, owns one responsibility, handles edge cases near their source, exposes a small test surface, and has an obvious place for future changes. Simplify anything that fails this check.

## State Change Safety

For state changes, identify the source of truth, its invariants, and the state that means the operation succeeded.

Make a multi-step change atomic when one transaction can own it. Otherwise, record enough progress to resume, compensate, or reconcile after partial failure. Do not report success before durable state and side effects meet the contract.

Assume requests and messages may arrive more than once. Make repeated execution safe, or detect the same intent with a stable identifier before causing another side effect. Distinguish a duplicate from a new request that happens to contain the same data.

A timeout, lost response, or cancellation after dispatch may leave the outcome unknown. Check authoritative state before retrying a non-idempotent action or reporting failure. Do not claim exactly-once behavior unless every participating boundary provides it.

Retry only failures that may succeed without another change. Bound attempts and elapsed time. When callers can retry together, use delay and jitter where needed to avoid amplifying load. Preserve cancellation and surface exhaustion according to project conventions.

Define behavior for duplicate, stale, and out-of-order work where the boundary permits them. Before finalizing, test success, partial failure, duplicate delivery, ambiguous outcome, cancellation, retry exhaustion, and recovery while confirming that invariants remain true.

## Clear Naming

Choose names that make code predictable at the point of use. A reader should understand what a name refers to without opening its definition.

Match detail to scope and frequency:

- use longer, descriptive names when the definition is distant, the scope is broad, or the concept is rarely used
- use short conventional names when the scope is small and the meaning is clear from nearby code
- avoid long names that repeat context already supplied by a module, type, receiver, or package

Use consistent domain terms. Use the same verb for the same operation and parallel names for parallel interfaces. Readers should be able to predict related names.

Name functions and methods for the result they return or the action they perform. Include the affected object when context does not make it clear. Replace vague words such as `process`, `handle`, `manage`, `helper`, or `utils` with the actual operation or domain concept.

Prefer descriptive domain names over cute, mnemonic, or newly coined names. Preserve established terms when renaming would cause churn or break shared vocabulary.

Before finalizing, read each new or changed name at its use sites. Confirm that its meaning is clear, its detail fits its scope, and related code uses the same vocabulary.

## Tests as Product Contracts

Tests must explain the behavior the system promises, not the mechanics used to arrange the test. A reader should quickly understand the condition, action, observable outcome, and what remains true on failure.

Treat test code as maintained product code. Apply the same standards for naming, structure, readability, error handling, cleanup, dependency control, and maintainability.

### Keep tests direct

- Arrange only inputs that define the scenario, perform one meaningful action, and assert the observable result and important side effects.
- Name tests in domain language so they read like behavioral contracts.
- Reuse existing focused helpers before adding new ones. Extract repeated setup and infrastructure mechanics into shared helpers when they form one cohesive responsibility. Keep scenario-defining inputs and expected results visible in each test.
- Make each setup helper do one cohesive job, fail near a broken prerequisite, and register cleanup where it creates the resource.
- Keep test code shallow, deterministic, independent, and free of hidden mutable state or ordering dependencies.
- Prefer one behavior per test. Avoid conditionals that change a test's meaning and loops outside simple parameterized cases.

### Use tables only when they clarify

Prefer named table-driven or parameterized cases when they share the same setup, action, and assertions, especially for validation, parsing, mappings, and boundaries.

Keep case data focused on what varies. Separate materially different behaviors when combining them requires mode flags, branching assertions, different lifecycle expectations, or substantially different setup.

Do not force a table when a few direct tests make the contracts easier to understand.

### Verify observable behavior

Cover the smallest useful set of contracts:

- the normal path
- important input boundaries
- meaningful failure paths and preserved state
- cleanup and resource ownership
- externally visible side effects

Assert prerequisites separately when their failure would make later assertions misleading. Prefer explicit expected values over clever generation and failure messages that identify the broken contract.

Avoid real external services, arbitrary sleeps, timing assumptions, shared global state, and brittle snapshots unless the behavior requires them. Use the smallest realistic substitute at the system boundary; do not mock internal details merely to increase isolation.

### Tautological tests considered harmful

Do not derive an expected result from the same logic, constants, mapping, template, schema, or dependency as the code under test. Such a test can pass when the shared assumption is wrong because it only proves that the implementation agrees with itself.

Derive expected values independently from the behavior contract or a small fixed example. Assert observable output, state, and side effects. An arranged mock returning its arranged value, a getter returning a value just passed to its setter, or a round trip through the same implementation path does not by itself prove behavior beyond that wiring. Keep such an assertion only when the wiring or round trip is part of the contract and the expected outcome comes from an independent source.

### Test prompts and policies honestly

Sentence or substring presence does not prove that an agent follows a prompt or policy. Test the actual contract:

- test loading or assembly when that is the contract
- assert exact text only when wording is intentionally external and fixed
- use an integration test or evaluation when the contract is agent behavior

If behavior cannot be tested cheaply, state the remaining risk instead of adding a false-confidence string assertion.

### Before finalizing

Confirm that test names describe meaningful behavior, each test is understandable without reading its helpers first, setup does not hide the scenario, table cases share one execution path, success and failure contracts are clear, behavior-preserving refactors would keep tests valid, and the suite is deterministic and independent.

If a test is difficult to read, simplify the product boundary or setup before adding explanatory comments. Simplify any test that fails this review.

## Plain Language Writing

Use plain language for human-readable technical prose. Write in the same language as the source unless the task requests translation. This includes code comments and docstrings, documentation, commit messages, pull request descriptions, issue text, change reports, runbooks, logs and error messages, command help, and user-facing technical copy.

Write for the intended reader. State the concrete subject, action, and consequence. Preserve factual meaning, exact identifiers, commands, protocol terms, legal language, quotations, and externally required wording.

Make text easy to understand, not merely short. Remove stale phrases, needless preamble, unnecessary qualifiers, repeated meaning, vague abstractions, and jargon that hides concrete behavior. Prefer familiar precise words and direct constructions when they help. Keep passive voice, formal language, and necessary domain terms when they are clearer or required. Do not flatten intentional voice or simplify away technical precision.

## Code Comment Policy

Make the code clear without comments. Improve its names, structure, or control flow before adding an explanation.

Write a comment only at the user's request or when the language or repository requires one. Examples include documentation for exported symbols, docstrings required by a linter, and annotations established by the file format. Ask the user before writing any other comment.

A comment is not justified because:

- the code took effort to write or seems subtle or clever
- you learned something while doing the work
- a future maintainer might question the implementation
- an approved plan included the proposed comment

Do not use comments for:

- past decisions, implementation history, or explanations of how the code changed
- change summaries, pull request explanations, or narrative accounts
- apologies or notes about deleted code
- descriptions already clear from the syntax or names
- product opinions or guesses about future needs
- TODOs that lack an owner or a specific completion condition
- temporary notices that lack a specific removal condition

Follow the surrounding source style. Keep a comment-free file free of comments.

When changing code, correct comments that become inaccurate. Remove comments that are stale, historical, obvious, or repetitive.

List each new comment separately in the change report.

Comments document the present system, not the journey that produced it.

## Implementation Change Report

Explain completed changes for a broad engineering audience. Start with a concise overview of the delivered behavior and the affected part of the system.

Describe each meaningfully changed file:

- its role and why it changed
- the changed functions, components, configuration, or equivalent units
- how behavior, data flow, control flow, errors, lifecycle, or external interactions changed
- why the implementation choice fits the codebase
- relevant tests, operational effects, compatibility concerns, and remaining risks

Keep detail proportional to the size and risk of the change. Group trivial or repetitive edits, but do not omit meaningful behavior. Use plain language, define unfamiliar terms, and describe concrete effects instead of restating the diff or listing filenames without context.
