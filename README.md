# dyne

dyne is a small HTTP control plane for coding-agent Jobs and durable multi-agent workflows. One server owns one cluster connection and one existing namespace. The CLI talks only to that server; it never loads kubeconfig, AWS credentials, a GitHub App key, or a GitHub token.

## Runtime model

Every task is a bounded Kubernetes Job. An ephemeral session uses one `emptyDir`. A persistent session uses one PVC with separate directories for the workspace, tool home, agent state, logs, and artifacts. A continuation is another Job mounted to the same PVC. SQL is the only source of application state for sessions, workflows, and publishing. Kubernetes Jobs, ConfigMaps, Secrets, and annotations are disposable execution projections. The workload package owns low-level execution requests and observations; the product packages translate them and persist every state transition in SQL.

A workflow is an immutable directed acyclic graph of isolated sessions. Steps never share a workspace or PVC. Dependency steps return bounded JSON outputs that dyne stores in SQL and includes in the direct dependent step's prompt. Independent ready steps can run concurrently up to the workflow's configured limit. At most one persistent leaf is publishable; all other steps are ephemeral findings.

This keeps recovery simple:

- Kubernetes replaces a failed task Pod. Persistent work written before the failure remains on the PVC.
- A new `dyne task` continues the retained Codex thread and workspace after a task completes or fails.
- SQL stores the immutable session definition, tasks, validated results, and deletion progress, so continuation still works after old Kubernetes resources are deleted or the dyne server restarts.
- `dyne delete` removes Kubernetes runtime resources and keeps a persistent session's SQL record and PVC. `dyne delete --storage` also deletes the SQL record and retained files.
- SQLite stores product state for a local server. PostgreSQL stores the same state in production. SQL transactions retain session, workflow, and publish intent and progress across server restarts.

The namespace must already exist and enforce the security policy appropriate for the cluster. Codex credentials must already exist in a Secret named `coding-agent-auth`. The Secret can contain `auth.json` or `CODEX_API_KEY`. Repository credentials are not stored there.

## Security boundary

The agent Pod has no service-account token and never receives GitHub credentials. The server places each short-lived GitHub App installation token in a task- or publisher-scoped Secret and mounts it only into the clone init container or publisher Job. The server refreshes the token before clone and publish operations and removes the Secret with its runtime resources. Agent definitions, instructions, skills, setup commands, and task prompts must not contain secrets. Principals allowed to read the server files, application database, ConfigMaps, or Pod specifications can read those values.

Agent Pods run as UID/GID 1000 with a read-only root filesystem, `RuntimeDefault` seccomp, no Linux capabilities, no privilege escalation, bounded resources, and denied ingress. The agent can still access the network and its Codex credential. dyne is intended for trusted repositories in a private cluster, not hostile multi-tenant execution.

The HTTP API has no application authentication. It listens on `127.0.0.1:8080` by default. If it runs in Kubernetes, expose it only as a private `ClusterIP` service and rely on the cluster or private-network access boundary.

## Start the server

Build without overwriting a local file named `dyne` by choosing an explicit output path when needed:

```bash
go build -o ./bin/dyne ./cmd/dyne
```

The server uses the following Kubernetes authentication order:

1. `--eks-cluster`, using the AWS SDK default configuration and optional `--aws-role-arn`;
2. explicit `--kubeconfig` and optional `--context`;
3. in-cluster service-account credentials;
4. standard kubeconfig loading when it is not running in a cluster.

Example with kubeconfig:

```bash
./bin/dyne server \
  --context colima-codex-proof \
  --namespace coding-agents \
  --agents-file ./agents.yaml \
  --workflows-file ./workflows.yaml \
  --database-url sqlite:dyne.db \
  --github-app-id 123 \
  --github-installation-id 456 \
  --github-private-key-file /secure/dyne-app.pem
```

Example with EKS and an assumed role:

```bash
./bin/dyne server \
  --eks-cluster coding-agents \
  --aws-region eu-west-1 \
  --aws-role-arn arn:aws:iam::123456789012:role/dyne-control-plane \
  --namespace coding-agents \
  --agents-file ./agents.yaml \
  --workflows-file ./workflows.yaml \
  --database-url "$DYNE_DATABASE_URL" \
  --github-app-id 123 \
  --github-installation-id 456 \
  --github-private-key-file /secure/dyne-app.pem
```

`--workflows-file` is optional. Without it, the server does not expose workflow routes. The application database is still required for session and publish state. `--database-url` accepts `sqlite:path`, `postgres://...`, or `postgresql://...`; `DYNE_DATABASE_URL` supplies the default flag value. The server prepares and versions the SQL schema at startup. Use a local SQLite file for one local process and an externally managed PostgreSQL database for a production server.

## Define agents

An agent is a reusable session template loaded when the server starts. Changing the file requires a server restart and affects only new sessions. Existing persistent sessions continue with the immutable configuration they started with.

```yaml
version: v1

agents:
  reviewer:
    description: Reviews repository changes.
    storage: ephemeral
    instructions: |
      Review correctness, security, tests, naming, and maintainability.
      Do not modify files unless the task requests changes.
    skills:
      - skills/code-review/SKILL.md
    setup: mise install
    clone_depth: 1
    timeout: 2h

  implementer:
    description: Implements focused changes.
    storage: persistent
    instructions: Implement the smallest safe change and run focused tests.
    setup: mise install
    clone_depth: 1
    storage_size: 10Gi
    timeout: 4h
```

Each definition requires a description, `ephemeral` or `persistent` storage, and non-empty instructions. Clone depth defaults to 1. Storage size and timeout inherit the server defaults; storage size is valid only for persistent agents. The server rejects the complete file at startup if any definition is invalid.

Skill paths are relative to the agent file and must identify a regular, non-symlink `SKILL.md` inside that directory. Each skill needs YAML frontmatter with a lowercase DNS-label name and a non-empty description. dyne packages only `SKILL.md`; scripts, references, assets, plugins, and hooks are not supported. Configured skills are additive to repository and Codex system skills.

List the configured agents and start a session from one:

```bash
dyne agents

dyne start \
  --agent reviewer \
  --name review-example \
  --repo https://github.com/example/project.git \
  --prompt 'Review the current changes.'
```

The repository, ref, prompt, session name, and optional timeout belong to the session instance. Storage, setup, clone depth, storage size, instructions, and skills belong to the agent definition and cannot be overridden by the client.

## Define and run workflows

Workflow definitions are loaded separately and resolve every named agent when the server starts. Non-publishable steps require ephemeral agents. The optional publishable step must use a persistent agent and must be a leaf.

```yaml
version: v1

workflows:
  delivery:
    description: Review two concerns, then implement the change.
    max_parallelism: 2
    steps:
      security:
        agent: reviewer
        prompt: Review the trust boundary and return structured findings.
      tests:
        agent: reviewer
        prompt: Review test gaps and return structured findings.
      implement:
        agent: implementer
        prompt: Implement the requested change using the review outputs.
        after: [security, tests]
        publishable: true
```

Start and inspect a durable run:

```bash
dyne workflows
dyne workflow-start \
  --workflow delivery \
  --name change-123 \
  --repo https://github.com/example/project.git \
  --prompt 'Fix the parser without changing its public contract.'

dyne workflow-status --name change-123
dyne workflow-artifacts --name change-123
```

The run snapshots the workflow and resolved agent definitions in the database. Later edits to either YAML file affect only new runs. A failed or blocked step skips its descendants while independent branches continue. `dyne workflow-cancel` records cancellation before deleting active compute. `dyne workflow-delete` is accepted only for a terminal run, destroys every step session and retained workspace, then deletes SQL state.

To publish a completed workflow, read `publishable_session` from `dyne workflow-artifacts` and pass that session name to the existing `dyne publish` command. Publishing remains explicit and uses the existing idempotent session publishing contract.

## Run and continue sessions

Client commands use `--server` or `DYNE_SERVER`; the default is `http://127.0.0.1:8080`.

```bash
dyne start \
  --agent implementer \
  --name update-example \
  --repo https://github.com/example/project.git \
  --prompt 'Implement the requested change and run focused tests.'

dyne status --name update-example
dyne logs --name update-example --follow
dyne artifacts --name update-example

dyne task --name update-example 'Address the remaining failed test.'
```

The selected agent definition controls storage and setup. Sessions created from ephemeral agents cannot continue or publish.

## Outcomes, artifacts, and publishing

A task can finish as completed, blocked, or failed. A blocked result is valid and must identify the blocker; dyne does not turn missing information or a sandbox limitation into false success.

The agent writes `outcome.json` for every successful invocation. A completed standalone or publishable task also writes `pull-request.json` with a title and description. A completed non-publishable workflow step writes a bounded `workflow-output.json` object instead. The harness validates the selected result contract, enforces the Kubernetes termination-message limit, and asks the same Codex thread to recreate invalid files once. dyne copies every validated result into SQL before it reports the task as complete. Persistent session files also remain on the PVC.

Publishing is explicit:

```bash
dyne publish \
  --name update-example \
  --branch yar/KARGO-123-description \
  --commit-message 'KARGO-123: implement description'
```

The publisher mounts the retained workspace and artifacts read-only, makes a clean clone, copies changed files without `.git`, creates and verifies a new non-force-pushed branch, then opens a draft pull request using the agent-authored title and description. Add `--ready` only when the pull request should not be a draft. Retrying the same publish intent recovers the existing branch or pull request instead of duplicating it.

## HTTP API

The CLI uses the agent creation endpoint and the session lifecycle endpoints:

- `GET /v1/agents`
- `POST /v1/agents/{agent}/sessions`
- `GET /v1/sessions/{name}`
- `POST /v1/sessions/{name}/tasks`
- `GET /v1/sessions/{name}/logs`
- `GET /v1/sessions/{name}/artifacts`
- `POST /v1/sessions/{name}/publish`
- `DELETE /v1/sessions/{name}?storage=delete`
- `GET /v1/workflows`
- `POST /v1/workflows/{workflow}/runs`
- `GET /v1/workflow-runs/{name}`
- `GET /v1/workflow-runs/{name}/artifacts`
- `POST /v1/workflow-runs/{name}/cancel`
- `DELETE /v1/workflow-runs/{name}`

Session creation and continuation return `202 Accepted` after Kubernetes accepts the resources. Coding continues asynchronously.

## Development

```bash
make doctor
make generate
make check BINARY=./bin/dyne
make image
make integration-test KUBERNETES_INTEGRATION_CONTEXT=colima-codex-proof
```

Ordinary tests do not contact Kubernetes, Docker, GitHub, AWS, or Codex. Live tests require explicit contexts and clean up their resources.

The [live coding-session E2E runbook](test/e2e/README.md) explains the real Codex and private GitHub repository journey, required GitHub App permissions, credential discovery, the Colima command, and cleanup behavior.
