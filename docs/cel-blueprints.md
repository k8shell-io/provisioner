# CEL expressions in blueprints

Reference for building frontend help / autocomplete for CEL expressions used in
blueprint YAML. Derived from `internal/blueprint` (provisioner) and
`github.com/k8shell-io/yaml-cel` `v0.2.8`.

---

## 1. What it is

Blueprint YAML values can be **dynamic**. Any scalar value may be replaced by a
[CEL](https://github.com/google/cel-spec) expression that is evaluated at
provision time against the current user / workspace / repository context.

CEL is a small, safe, side-effect-free expression language (like a spreadsheet
formula). It is **not** a scripting language: no loops, no variable assignment,
no statements — a single expression that returns a single value.

## 2. Syntax: the `!cel` tag

An expression is written as a YAML scalar tagged `!cel`:

```yaml
hostname: !cel "user.username + '-' + metadata.name"
subdomain: !cel "user.organization"
storages:
  home:
    path: !cel "'/home/' + user.username"
    claimSpec:
      storageClassName: !cel env("WORKSPACE_STORAGE_CLASS")
    claimSpecAnnotations:
      zfs-csi.k8shell.io/squash-uid: !cel "user.uid"
```

Rules:

| Rule | Detail |
|---|---|
| Tag only scalars | `!cel` may only be placed on a scalar node. Maps and sequences cannot be tagged — tag each leaf value individually. |
| Value is the expression string | `!cel "user.username"` — the quoted string is compiled as CEL. |
| Multi-line expressions | Use a YAML block scalar: `!cel \|` then an indented expression on following lines. |
| Untagged scalars are literals | A plain value (`image: docker.io/alpine:latest`) is passed through unchanged. There is **no** `${...}` or `{{...}}` interpolation on the CEL path. |
| Independent evaluation | Each `!cel` leaf is compiled and evaluated on its own. Expressions **cannot** reference other blueprint fields — only the scope objects in §4. |
| Result type must fit the schema | The returned value is written back into the YAML and then decoded into the blueprint struct. If the target field is a string, return a string (wrap with `string(...)` if needed). |
| Errors abort the blueprint | A compile or evaluation error fails the whole provision/validation with the offending field path. |

## 3. Evaluation model

1. The custom blueprint is merged with its `template` parent.
2. Every `!cel` leaf is compiled against a CEL environment that declares the
   scope variables (§4) and the custom functions (§5).
3. Each expression is evaluated; the result replaces the node.
4. The resulting document is decoded into the blueprint model and validated.

During **blueprint validation / preview** (the editor "validate" endpoint) the
same evaluation runs but with a **fixed test scope** (`user.username` =
`testuser`, `uid`/`gid` = `1000`, `roles` = `["role1","role2"]`,
`metadata.repoOwner` = `testowner`, etc.), so expressions are checked for
compile and type errors without a real user.

## 4. Scope: variables available to expressions

Four top-level identifiers are in scope.

### `user` — the authenticated user (object)

| Field | Type | Notes |
|---|---|---|
| `user.username` | string | login name |
| `user.organization` | string | org the user belongs to |
| `user.isValid` | bool | |
| `user.expiresAt` | string | RFC 3339 timestamp; wrap with `timestamp(user.expiresAt)` for date math |
| `user.uid` | int | numeric POSIX uid |
| `user.gid` | int | numeric POSIX gid |
| `user.fullname` | string | |
| `user.email` | string | |
| `user.locked` | bool | |
| `user.roles` | list&lt;string&gt; | e.g. `["org-admin","workspace-user"]` |
| `user.blueprints` | list&lt;string&gt; | blueprint names the user may use; may be `["*"]` |
| `user.source` | string | identity source |
| `user.shell` | string | login shell, may be empty |
| `user.sudo` | bool | |
| `user.password` | string | present only when set — guard with `has(user.password)` |
| `user.manageRepos` | string | present only when the IdP supplied it — guard with `has(...)` |
| `user.gitAddress` | string | e.g. `https://github.com`; present only when set — guard with `has(...)` |

### `metadata` — blueprint / repository coordinates (object)

| Field | Type | Notes |
|---|---|---|
| `metadata.name` | string | resolved blueprint instance name (normalized DNS label) |
| `metadata.repoName` | string | repository name |
| `metadata.repoOwner` | string | repository owner / org |
| `metadata.repoRef` | string | git ref (branch / tag / sha) |
| `metadata.repoAddress` | string | repository host address |

### `workspaceName` — string

The target workspace name for this provision request.

### `blueprint` — string

The blueprint name as requested (before DNS normalization).

> Accessing a field that is absent (an `omitempty` field with no value) raises a
> "no such key" evaluation error. Use `has(user.gitAddress)` before reading
> optional fields.

## 5. Custom functions

These are registered by `yaml-cel` in addition to the CEL standard library.

### `env(name: string) -> string`

Returns the value of an environment variable **of the provisioner process**
(not a workspace env var). Fails the expression if the variable is unset.

```yaml
storageClassName: !cel env("WORKSPACE_STORAGE_CLASS")
```

### `distinct(items: list<string>) -> list<string>`

Returns the list with duplicate strings removed, preserving first-seen order.
Non-string elements are skipped.

```yaml
roles: !cel "distinct(user.roles + ['workspace-user'])"
```

### `normalizeDNS(s: string) -> string`

Normalizes a string to an RFC 1123 DNS label: lowercased, every run of
characters outside `[a-z0-9-]` replaced with `-`, repeated `-` collapsed,
leading/trailing `-` trimmed, truncated to 63 characters.

```yaml
hostname: !cel "normalizeDNS(user.username + '-' + metadata.repoName)"
```

## 6. CEL standard library (available everywhere)

### Literals
`42`, `3.14`, `'single'` or `"double"` quoted strings, `true` / `false`, `null`,
lists `["a", "b"]`, maps `{"k": "v"}`.

### Operators
- Arithmetic: `+` `-` `*` `/` `%` (`+` also concatenates strings and lists)
- Comparison: `==` `!=` `<` `<=` `>` `>=`
- Logical: `&&` `||` `!`
- Membership: `x in list`, `key in map`
- Conditional: `cond ? whenTrue : whenFalse`
- Access: `obj.field`, `list[0]`, `map["key"]`

### Macros
| Macro | Meaning |
|---|---|
| `has(obj.field)` | field is present (use for optional fields) |
| `list.all(x, pred)` | all elements satisfy `pred` |
| `list.exists(x, pred)` | at least one element satisfies `pred` |
| `list.exists_one(x, pred)` | exactly one element satisfies `pred` |
| `list.filter(x, pred)` | sublist of elements satisfying `pred` |
| `list.map(x, expr)` | list with `expr` applied to each element |

### Functions
`size(x)` (list/map/string length), `int(x)`, `uint(x)`, `double(x)`,
`string(x)`, `bool(x)`, `type(x)`, `timestamp(str)`, `duration(str)`.

### String member functions
`s.contains(sub)`, `s.startsWith(prefix)`, `s.endsWith(suffix)`,
`s.matches(regex)` (RE2 syntax).

> Only these string helpers are available — there is no `split`, `replace`,
> `trim`, `toLower`, etc. For DNS-safe names use `normalizeDNS()`.

## 7. Examples

| Goal | Expression |
|---|---|
| Per-user home path | `!cel "'/home/' + user.username"` |
| Hostname from user + blueprint | `!cel "user.username + '-' + metadata.name"` |
| DNS-safe hostname | `!cel "normalizeDNS(user.username + '-' + metadata.repoName)"` |
| Storage class from provisioner env | `!cel env("WORKSPACE_STORAGE_CLASS")` |
| Numeric uid into an annotation | `!cel "user.uid"` |
| uid as a string | `!cel "string(user.uid)"` |
| Shared volume name | `!cel "metadata.repoOwner + '/shared'"` |
| Admins only: disable a feature | `!cel "!(user.roles.exists(r, r in ['org-admin','admin','workspace-admin']))"` |
| Default value when field is empty | `!cel "has(user.shell) && user.shell != '' ? user.shell : '/bin/bash'"` |
| Pick primary email with fallback | `!cel \|`<br>`  user.email != '' ? user.email : 'unknown@nowhere.com'` |
| Dedup a computed role list | `!cel "distinct(user.roles + ['workspace-user'])"` |
| Org as subdomain | `!cel "user.organization"` |
| Days until expiry (int) | `!cel "int((timestamp(user.expiresAt) - timestamp('1970-01-01T00:00:00Z')) / duration('24h'))"` |

## 8. Where `!cel` can be used

Any scalar leaf in the blueprint document. Fields where it is commonly used:

- `hostname`, `subdomain`, `splash`
- `image`, `imagePullPolicy`
- `env.<KEY>` values
- `storages.<name>.id`, `storages.<name>.path`, `storages.<name>.readonly`
- `storages.<name>.claimSpec.storageClassName`,
  `...claimSpec.resources.requests.storage`
- `storages.<name>.claimSpecAnnotations.<key>` values
- `resources.cpu`, `resources.memory`
- `securityContext.*` scalar values
- `apps.<name>.*` scalar values

Structural keys (map keys, the `storages` map itself, sequences) cannot be
expressions — only the scalar values under them.
