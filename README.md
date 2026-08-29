# Kubernetes coding-agent sessions

This repository runs coding agents with native Kubernetes resources and a small Go client. Bounded work uses Jobs. Resumable work uses a singleton StatefulSet. Persistent volumes own durable state; Pods own only compute.

## Storage and lifecycle contract

| Use case                    | Workload    | Workspace                                    | Tool home and cache | Codex state | `/tmp`            |
| --------------------------- | ----------- | -------------------------------------------- | ------------------- | ----------- | ----------------- |
| Read-only exploration       | Job         | `emptyDir`, read-only in the agent container | `emptyDir`          | `emptyDir`  | Memory `emptyDir` |
| Bounded code update         | Job         | PVC                                          | PVC                 | PVC         | Memory `emptyDir` |
| Long or interactive session | StatefulSet | PVC                                          | PVC                 | PVC         | Memory `emptyDir` |

The setup init container writes the initial checkout and can run commands such as `mise install` and `npm ci`. It cannot mount the Codex credential or session volume. A separate init container seeds credentials after repository setup finishes. Setup can run again after Pod replacement, so it must be idempotent. Stable tools are built into the image. Uncommitted changes, `node_modules`, Codex session files, mise installs, and npm cache survive only for modes backed by PVCs.

Stopping a long session scales its StatefulSet to zero and retains its three claims. Resuming recreates the Pod against the same claims. Deleting a workload keeps storage. Destroying it also deletes the claims.

## Build for Colima

The tested local target is Colima 0.10.1 with k3s v1.35.0. Build into that profile's Docker daemon so k3s can use the local image:

```bash
go build -o agentctl ./cmd/agentctl
env -u DOCKER_HOST docker --context colima-codex-k8s build -f container/Dockerfile -t coding-agent:local .
```

`agentctl` uses `client-go` with the standard kubeconfig loading rules. `--context` selects a kubeconfig context; the `kubectl` executable is not required.

The image pins Node 24.14.1, Codex 0.150.1, and mise 2026.8.14. It also includes Git, OpenSSH, ripgrep, and build tools.

## Authentication

For an API key used only by this private local cluster:

```bash
export CODEX_API_KEY='...'
./agentctl bootstrap --context colima-codex-k8s --api-key-env CODEX_API_KEY
unset CODEX_API_KEY
```

To seed an existing Codex CLI login instead:

```bash
./agentctl bootstrap --context colima-codex-k8s --auth-file "$HOME/.codex/auth.json"
```

For private GitHub repositories, pass the current `gh` token directly to bootstrap without printing it or putting it in a repository URL:

```bash
GITHUB_TOKEN="$(gh auth token)" \
./agentctl bootstrap \
  --context colima-codex-k8s \
  --auth-file "$HOME/.codex/auth.json" \
  --github-token-env GITHUB_TOKEN
```

The GitHub Secret is mounted only by the clone init container. Repository setup and Codex cannot mount it. Clones fetch one branch at depth `1` by default; use `--clone-depth 0` only when the task genuinely needs full history.

The Codex Secret is mounted only by the credential init container and the agent container. Repository setup cannot access it. `auth.json` is copied into the separate Codex-state volume only when that volume has no credentials, which lets Codex persist token rotation for a retained session. Treat both the Kubernetes Secret and the Codex-state PVC as credentials.

Codex and the commands it launches run as the same user. Those commands can read the active credential and use outbound network access. Run only repositories and prompts you trust. Do not place secrets in `--prompt` or `--setup`; both values are stored in the Pod specification and are visible to principals that can read Pods.

API keys are the general automation default in the [official Codex non-interactive guidance](https://learn.chatgpt.com/docs/non-interactive-mode). For an eligible managed workspace, prefer [workload identity federation](https://learn.chatgpt.com/docs/enterprise/workload-identity) so the Pod receives a rotating identity token instead of a stored key. This baseline intentionally does not automate federation because it depends on organization-side configuration.

## Short exploration

```bash
./agentctl start \
  --context colima-codex-k8s \
  --name inspect-example \
  --mode explore \
  --repo https://github.com/example/project.git \
  --setup 'mise install && npm ci' \
  --prompt 'Explain the request path and identify correctness risks.'

./agentctl logs --context colima-codex-k8s --name inspect-example --follow
```

The init container can install dependencies, but the agent receives the checked-out workspace read-only. Codex runs with `--ephemeral`, so its rollout files are not retained.

## Bounded code update

```bash
./agentctl start \
  --context colima-codex-k8s \
  --name update-example \
  --mode update \
  --repo https://github.com/example/project.git \
  --setup 'mise install && npm ci' \
  --prompt 'Implement the requested change and run focused tests.'

./agentctl logs --context colima-codex-k8s --name update-example --follow
./agentctl status --context colima-codex-k8s --name update-example
```

The completed Job leaves its workspace, tool-home, and Codex-state PVCs in place. To inspect or continue that exact workspace, delete only the completed workload and start a long session with the same name:

```bash
./agentctl delete --context colima-codex-k8s --name update-example
./agentctl start \
  --context colima-codex-k8s \
  --name update-example \
  --mode long \
  --repo https://github.com/example/project.git \
  --setup 'mise install && npm ci'
./agentctl shell --context colima-codex-k8s --name update-example
```

The init container sees the existing Git checkout and does not clone over it. Each distinct session name owns separate claims.

## Publish a pull request

A completed update session or stopped long session can be published through an explicit delivery command:

```bash
./agentctl publish \
  --context colima-codex-k8s \
  --name update-example \
  --branch yar/KARGO-123-description \
  --commit-message 'KARGO-123: implement description' \
  --title 'KARGO-123: implement description' \
  --body-file pr.md
```

The pull request is a draft by default. Add `--ready` only when it should be ready for review immediately. `--base` defaults to the ref used to start the session.

Publishing refuses exploration sessions, active or unsuccessful sessions, existing branches not owned by the same publish attempt, empty changes, and concurrent publish intents. It never force-pushes. Retrying the same command reuses an existing matching branch or pull request instead of creating duplicates.

The coding agent never receives the GitHub token. A bounded publisher Job mounts the source workspace read-only and clones the trusted base into a separate temporary volume. It copies the workspace snapshot into that clean clone, disables Git hooks and external Git configuration, creates the commit using the authenticated GitHub user's noreply identity, pushes the new branch, and verifies the remote commit. `agentctl` then creates the pull request through the GitHub API and removes the publisher Job.

## Long session

```bash
./agentctl start \
  --context colima-codex-k8s \
  --name long-example \
  --mode long \
  --repo https://github.com/example/project.git \
  --setup 'mise install && npm ci'

./agentctl task --context colima-codex-k8s --name long-example 'Investigate the failing tests.'
./agentctl task --context colima-codex-k8s --name long-example --resume-last 'Apply the smallest safe fix.'
./agentctl shell --context colima-codex-k8s --name long-example
./agentctl stop --context colima-codex-k8s --name long-example
./agentctl resume --context colima-codex-k8s --name long-example
```

`stop` and `resume` preserve storage. `delete` removes compute and keeps storage. `destroy` removes compute and all three PVCs:

```bash
./agentctl destroy --context colima-codex-k8s --name long-example
```

Only one `task` command can run in a long session at a time. A second call fails immediately instead of writing the same workspace concurrently.

## Security and failure boundaries

The namespace enforces Kubernetes Restricted Pod Security. Agent Pods run as UID/GID 1000, drop all capabilities, use `RuntimeDefault` seccomp, disable privilege escalation and service-account token mounting, and use a read-only root filesystem. The namespace denies ingress. CPU, memory, ephemeral storage, duration, and tmpfs size are bounded.

Codex's nested OS sandbox is bypassed because the tested Colima security stack blocks the required nested namespace operations. The Kubernetes Pod is therefore the command boundary. This is appropriate for a private, isolated cluster that runs trusted repositories. It is not enough for hostile multi-tenant workloads. Production use should add a stronger runtime such as gVisor or Kata, domain-aware egress controls, workload identity, per-tenant namespaces and quotas, and external result export.

Agent Jobs use `backoffLimit: 0` and do not push code automatically. Only the explicit `publish` command creates a publisher Job, and that Job pushes one new branch without force. Kubernetes can still create a replacement Pod after infrastructure loss, so setup and task effects outside the workspace must be idempotent. A timeout or lost response can leave outcome unknown; inspect the PVC and Git state before retrying. `ReadWriteOnce` is a node-level constraint, not an exclusive-Pod lock. The session-name workload prevents normal concurrent starts, but a production control plane should enforce exclusive ownership with `ReadWriteOncePod` on a supporting CSI driver or an explicit lease.

Colima's local-path storage survives Pod replacement and VM restart, but it is tied to the single VM. Deleting the PVC, its PV, or the Colima profile can delete the data. Back up or push valuable work before destroying storage.

## Development

The repository adopts the same developer entrypoint locally and in CI:

```bash
make doctor
make check
make build
make image
```

`make tools` installs the pinned golangci-lint version. Ordinary Go tests do not contact Docker, Kubernetes, Codex, or GitHub. There is currently no live conformance suite; state-changing runtime verification must use an explicitly isolated Colima profile and own its cleanup.

Repository conventions are documented in [AGENTS.md](AGENTS.md), [docs/GO.md](docs/GO.md), [docs/TESTING.md](docs/TESTING.md), and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
