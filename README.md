# Airlock

Airlock is a small HTTP control plane for coding-agent Jobs on Kubernetes. One server owns one cluster connection and one existing namespace. The CLI talks only to that server; it never loads kubeconfig, AWS credentials, a GitHub App key, or a GitHub token.

## Runtime model

Every task is a bounded Kubernetes Job. An ephemeral session uses one `emptyDir`. A persistent session uses one PVC with separate directories for the workspace, tool home, agent state, logs, artifacts, and session definition. A continuation is another Job mounted to the same PVC. An agent-backed session also owns an immutable ConfigMap containing its developer instructions and selected instruction-only skills.

This keeps recovery simple:

- Kubernetes replaces a failed task Pod. Persistent work written before the failure remains on the PVC.
- A new `airlock task` continues the retained Codex thread and workspace after a task completes or fails.
- The PVC stores the immutable session definition, so continuation still works after the old Jobs are deleted or the Airlock server restarts.
- `airlock delete` removes Jobs and keeps a persistent session's PVC and agent configuration. `airlock delete --storage` also deletes retained state.

The namespace must already exist and enforce the security policy appropriate for the cluster. Codex credentials must already exist in a Secret named `coding-agent-auth`. The Secret can contain `auth.json` or `CODEX_API_KEY`. Repository credentials are not stored there.

## Security boundary

The agent Pod has no service-account token and never receives GitHub credentials. A short-lived GitHub App installation token is mounted only into the clone init container and the publisher Job. The server refreshes that token before clone and publish operations. Agent definitions, instructions, skills, setup commands, and task prompts must not contain secrets. Principals allowed to read the server file, ConfigMaps, or Pod specifications can read those values.

Agent Pods run as UID/GID 1000 with a read-only root filesystem, `RuntimeDefault` seccomp, no Linux capabilities, no privilege escalation, bounded resources, and denied ingress. The agent can still access the network and its Codex credential. Airlock is intended for trusted repositories in a private cluster, not hostile multi-tenant execution.

The HTTP API has no application authentication. It listens on `127.0.0.1:8080` by default. If it runs in Kubernetes, expose it only as a private `ClusterIP` service and rely on the cluster or private-network access boundary.

## Start the server

Build without overwriting a local file named `airlock` by choosing an explicit output path when needed:

```bash
go build -o ./bin/airlock ./cmd/airlock
```

The server uses the following Kubernetes authentication order:

1. `--eks-cluster`, using the AWS SDK default configuration and optional `--aws-role-arn`;
2. explicit `--kubeconfig` and optional `--context`;
3. in-cluster service-account credentials;
4. standard kubeconfig loading when it is not running in a cluster.

Example with kubeconfig:

```bash
./bin/airlock server \
  --context colima-codex-proof \
  --namespace coding-agents \
  --agents-file ./agents.yaml \
  --github-app-id 123 \
  --github-installation-id 456 \
  --github-private-key-file /secure/airlock-app.pem
```

Example with EKS and an assumed role:

```bash
./bin/airlock server \
  --eks-cluster coding-agents \
  --aws-region eu-west-1 \
  --aws-role-arn arn:aws:iam::123456789012:role/airlock-control-plane \
  --namespace coding-agents \
  --agents-file ./agents.yaml \
  --github-app-id 123 \
  --github-installation-id 456 \
  --github-private-key-file /secure/airlock-app.pem
```

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

Skill paths are relative to the agent file and must identify a regular, non-symlink `SKILL.md` inside that directory. Each skill needs YAML frontmatter with a lowercase DNS-label name and a non-empty description. Airlock packages only `SKILL.md`; scripts, references, assets, plugins, and hooks are not supported. Configured skills are additive to repository and Codex system skills.

List the configured agents and start a session from one:

```bash
airlock agents

airlock start \
  --agent reviewer \
  --name review-example \
  --repo https://github.com/example/project.git \
  --prompt 'Review the current changes.'
```

The repository, ref, prompt, session name, and optional timeout belong to the session instance. Storage, setup, clone depth, storage size, instructions, and skills belong to the agent definition and cannot be overridden by the client.

## Run and continue sessions

Client commands use `--server` or `AIRLOCK_SERVER`; the default is `http://127.0.0.1:8080`.

```bash
airlock start \
  --agent implementer \
  --name update-example \
  --repo https://github.com/example/project.git \
  --prompt 'Implement the requested change and run focused tests.'

airlock status --name update-example
airlock logs --name update-example --follow
airlock artifacts --name update-example

airlock task --name update-example 'Address the remaining failed test.'
```

The selected agent definition controls storage and setup. Sessions created from ephemeral agents cannot continue or publish.

## Outcomes, artifacts, and publishing

A task can finish as completed, blocked, or failed. A blocked result is valid and must identify the blocker; Airlock does not turn missing information or a sandbox limitation into false success.

The agent writes `outcome.json` for every successful invocation. Completed work also writes `pull-request.json` with a title and description. The harness validates these files and asks the same Codex thread to recreate invalid files once. The files remain on persistent storage, and their bounded API representation is available through `airlock artifacts`.

Publishing is explicit:

```bash
airlock publish \
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

Session creation and continuation return `202 Accepted` after Kubernetes accepts the resources. Coding continues asynchronously.

## Development

```bash
make doctor
make check BINARY=./bin/airlock
make image
make integration-test KUBERNETES_INTEGRATION_CONTEXT=colima-codex-proof
make e2e-test KUBERNETES_INTEGRATION_CONTEXT=colima-codex-proof DOCKER_CONTEXT=colima-codex-proof
```

Ordinary tests do not contact Kubernetes, Docker, GitHub, AWS, or Codex. Live tests require explicit contexts and clean up their namespaces.
