# provisioner

[![Build](https://github.com/k8shell-io/provisioner/actions/workflows/build.yaml/badge.svg)](https://github.com/k8shell-io/provisioner/actions/workflows/build.yaml)

The k8shell workspace provisioner. Manages the full lifecycle of developer workspaces on Kubernetes: creating and tearing down Helm releases, injecting workspace containers into existing workloads, discovering running workspaces, and streaming real-time provisioning events to callers over gRPC.

## Concepts

### Blueprints

A blueprint is a YAML document that describes how a k8shell workspace pod should be configured — image, resources, storage volumes, network policy, sidecar containers, and more. Blueprint files are loaded from a directory on startup and watched for changes; edits take effect without restarting the server.

Blueprints support two key features:

**CEL template expressions** — field values prefixed with `!cel` are evaluated as [Common Expression Language](https://cel.dev) expressions at provision time. The evaluation context includes the authenticated user record, workspace metadata, and the resolved parent blueprint chain.

```yaml
subdomain: !cel "user.organization"
hostname:  !cel "user.username + '-' + metadata.name"
```

**Inheritance** — a blueprint can declare a `template: <name>` parent. Child fields deep-merge over parent fields, with configurable per-key merge strategies (replace vs. append) registered at server startup. Marking a blueprint `isTemplate: true` makes it non-provisionable — it can only be used as a parent.

```yaml
blueprints:
  - name: base
    isTemplate: true
    image: docker.io/alpine:latest
    resources:
      cpu: 4
      memory: 4Gi

  - name: development
    template: base
    resources:
      cpu: 8
      memory: 8Gi
```

The provisioner resolves the full inheritance chain at request time, evaluates CEL expressions, and hands the final Helm values map to the Helm client.

### Workspace modes

The provisioner supports two modes of operation:

| Mode | Description |
|---|---|
| **Standalone** | A dedicated workspace pod is created in the target namespace as a Helm release. |
| **Injection** | Workspace containers and volumes are patched into an existing workload (Deployment, StatefulSet, or DaemonSet) in a separate namespace. No Helm release is created. |

The mode is determined by the userstr passed in the provision request. A userstr that includes a namespace and workload name selects injection mode; all other userstr forms select standalone mode.

### Workspace lifecycle

Each workspace transitions through a set of states managed by the provisioner:

- **Standalone**: `Provision` installs the Helm release and waits for the pod to reach `Running`. `Stop` scales the release to zero. `Delete` uninstalls the release and deletes all associated resources.
- **Injection**: `Inject` patches the target workload and waits for the updated pods to reach `Running`. `Eject` reverses the patch and deletes workspace-labeled resources from the target namespace.

Both paths acquire a distributed lock before mutating any Kubernetes resources, ensuring that concurrent requests for the same workspace are serialised across provisioner replicas.

### Distributed locking

Concurrent install, upgrade, or delete operations on the same workspace are prevented by a per-workspace mutex backed by Kubernetes Lease objects (`coordination.k8s.io/v1`). Each provisioner replica uses a unique holder identity so lock ownership is unambiguous. Leases carry a 30-second TTL; a lock held by a crashed replica is automatically available to the next caller once the TTL elapses.

### Provisioning jobs

When NATS is enabled the provisioner tracks each provisioning operation as a job record in a JetStream key-value bucket (`WORKSPACE_PROVISION_JOBS`). Job records are updated in real time as events arrive from the workspace, and remain readable for 48 hours (configurable). Callers that reconnect after a stream interruption can retrieve the current job state without re-provisioning.

### Streaming events

`ProvisionWorkspaceStream` delivers a two-phase stream to the caller:

1. **Handshake** — confirms the workspace name and job ID before any Kubernetes mutation begins.
2. **Events** — pod state transitions, progress percentage steps, and a final status are emitted as `ProvisionEvent` messages until the workspace reaches `Running` or an error occurs.

### gRPC API

The provisioner exposes a single gRPC interface defined in `github.com/k8shell-io/common`:

- `ProvisionerService` (`provisioner.proto`) — workspace provision, stop, delete, inject, eject, template rendering, blueprint listing, and job status queries. Consumed by the k8shell API server.

JWT authentication and per-request authorization checks (via the optional authz service) are applied at the gRPC middleware layer before any workspace operation begins.

## Repository layout

```
internal/
  blueprint/   # Blueprint loading, CEL evaluation, inheritance resolution, filesystem watcher
  config/      # Server configuration types and YAML loader
  helm/        # Helm SDK wrapper, Kubernetes client, workload injection adapters
  server/      # gRPC ProvisionerService, request handlers, provisioning job tracking
  workspace/   # Workspace lifecycle (provision, stop, delete, inject, eject), pod state analysis
config/
  blueprints/  # Example blueprint YAML files
  config.yaml  # Reference configuration
docker/
  provisioner/ # Dockerfile (alpine + distroless stages)
test/          # Integration and smoke tests
```

## Prerequisites

- Go 1.24+
- Docker
- A running Kubernetes cluster accessible via `~/.kube/config` (or in-cluster)
- NATS JetStream (optional — required for provisioning job tracking)

## Local development

**Build and run:**
```bash
make build
./bin/provisioner --config config/config.yaml --logtext
```

**CLI flags:**

| Flag | Default | Description |
|---|---|---|
| `--config <file>` | `config/config.yaml` | Path to the configuration file |
| `--logtext` | `false` | Emit logs as human-readable text instead of JSON |
| `-v` | — | Print version and commit, then exit |

**Configuration** is a YAML file. Secrets and paths can be injected via environment variables (`${VAR}`) or `!file` references. See `config/config.yaml` for the full reference.

## Makefile targets

| Target | Description |
|---|---|
| `make build` | Compile binary to `bin/provisioner` |
| `make test` | Run unit tests with coverage |
| `make test-static` | Run `golangci-lint` and `gosec` |
| `make test-self` | Static analysis + build + binary smoke tests (used in CI) |
| `make image` | Build Docker image (Alpine by default) |
| `make image-debug` | Build Alpine debug image (with debug symbols, while-loop entrypoint) |
| `make image-release` | Build production distroless image |
| `PUSH=1 make image` | Build and push image to the registry |
| `make vendor` | Vendor Go dependencies |
| `make coverage` | Print total test coverage percentage |
| `make clean` | Remove build artefacts and vendor directory |

## Docker images

Two runtime stages are available in `docker/provisioner/Dockerfile`:

| Stage | Base | Use case |
|---|---|---|
| `alpine` | `alpine:3.21.3` | Development and debugging (has a shell; binary includes debug symbols; entrypoint restarts on exit) |
| `distroless` | `distroless/static-debian12:nonroot` | Production (no shell, runs as non-root) |

The Alpine stage uses a debug build (`-gcflags="all=-N -l"`). The distroless stage uses a stripped binary (`CGO_ENABLED=0`, `-ldflags="-s -w"`). Both stages embed `VERSION` and `COMMIT_ID` as linker variables.

CI builds PR images with the `alpine` stage and tags releases with `distroless`.

## Running in Kubernetes

Deployment configuration and Helm charts for the provisioner and the other k8shell services are maintained in the [k8shell-io/charts](https://github.com/k8shell-io/charts) repository.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
