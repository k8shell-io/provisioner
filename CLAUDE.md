# CLAUDE.md

Guidelines for Claude Code when working in this repository.

## Build and test commands

```bash
make build          # compile binary to bin/provisioner
make test           # unit tests with coverage (writes reports/)
make test-static    # golangci-lint + gosec (HIGH severity)
make test-self      # static analysis + build + binary smoke test (what CI runs)
make coverage       # print total coverage % from reports/coverage.out
```

Run a single test package:
```bash
go test ./internal/blueprint/... -v
```

Run a single test by name:
```bash
go test ./internal/... -run TestBlueprintCompose -v
```

## Private modules

`github.com/k8shell-io/common` and `github.com/k8shell-io/yaml-cel` are private. Access requires a GitHub PAT configured as a Git credential rewrite:

```bash
git config --global url."https://<PAT>:x-oauth-basic@github.com/".insteadOf "https://github.com/"
```

The repo uses vendoring (`vendor/`). After changing `go.mod`, run `make vendor` to update.

## Code conventions

- **Logging**: use `zerolog` via `github.com/k8shell-io/common/pkg/logger`. Loggers are created with `log.NewLogger("component")` and threaded through structs — never use the global logger directly.
- **Errors**: wrap with `fmt.Errorf("context: %w", err)`. Sentinel errors live in the package that owns the concept (e.g. `models.ErrWorkspaceNotFound`).
- **Context**: always propagate `context.Context` as the first parameter. Never store a context in a struct.
- **Comments**: Go doc comment style — exported symbols only, first word is the symbol name.
- **License header**: every `.go` file must start with:
  ```go
  // Use of this source code is governed by a AGPLv3
  // license that can be found in the LICENSE file.
  ```

## Architecture overview

The provisioner is a single gRPC server process. Requests arrive at `ProvisionerService` in `internal/server/`, which delegates to the `workspace` package for all Kubernetes mutations.

**Request path for a provision call:**
1. `server/provision.go` — validates the userstr, resolves authz, sends the gRPC handshake
2. `workspace/workspace.go` — acquires a `WorkspaceLock` (Kubernetes Lease), builds Helm values from the blueprint, calls the Helm client
3. `helm/client.go` — wraps the Helm SDK; for injection calls, `helm/inject.go` patches the target workload
4. `workspace/podstate.go` — watches pod events and streams `WorkspaceStreamEvent` messages back up the call stack
5. `server/provision.go` — translates stream events to `ProvisionEvent` gRPC messages and writes them to NATS KV

**Key packages:**
- `internal/blueprint` — loads and hot-reloads YAML files; `BlueprintManager` is the single source of truth for blueprint state
- `internal/helm` — owns the embedded `k8shell-workspace` Helm chart (`internal/helm/charts/`) and the label/annotation constants shared across the codebase
- `internal/workspace` — stateless functions; a `Workspace` struct is constructed per-request, not pooled
- `internal/server` — gRPC layer only; no business logic beyond request parsing and response marshalling

## Configuration

The config file is YAML with two extension mechanisms:
- `${ENV_VAR}` — expanded from environment at load time
- `!file <path>` — file contents substituted inline (used for secrets)

`injectNamespaces` controls which namespaces allow workload injection. Setting it to `["*"]` enables cluster-wide injection and requires a `ClusterRole` binding. Omitting it disables injection entirely.

## Docker images

| Target | When to use |
|---|---|
| `make image-debug` / `RUNTIME=alpine make image` | PR builds, local debugging |
| `make image-release` / `RUNTIME=distroless make image` | Production / tagged releases |

Set `PUSH=1` to push instead of loading into the local daemon.
