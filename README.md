# opensearch-api

`osapi` is a small, general-purpose CLI for talking to any OpenSearch REST endpoint with robust
 retry behaviour.

It behaves like a resilient `curl` for OpenSearch: bounded or unlimited retry with configurable
backoff, optional insecure-TLS, and success/retry classification that defaults to status codes but
can be driven by `jq` predicates against the response body — in a single binary that can reach any
endpoint. The response body is written to **stdout** (pipe it to `jq`); diagnostics and per-attempt
retry detail go to **stderr**.

## Install

```sh
# Latest release, straight from GitHub (installs onto $(go env GOPATH)/bin):
go install github.com/logmanager-oss/opensearch-api/cmd/osapi@latest

# Or from a local checkout:
go install ./cmd/osapi          # onto your PATH
go build -o osapi ./cmd/osapi   # local binary in the repo dir (or: make build)
```

Shell completion (below) needs `osapi` on your `PATH`, so prefer `go install`.

## Usage

By default `osapi` sends one request per invocation — the command itself is the request (there is
no `request` subcommand). The `run` subcommand instead executes an ordered sequence of calls
defined in a YAML file (see [Run files](#run-files-osapi-run) below). `osapi --version` prints the
version.

```sh
# Cluster health, pretty-printed
osapi --endpoint https://localhost:9200 -k -u admin --path _cluster/health | jq .

# PUT a policy from a file, retrying up to 5 times but stopping immediately on 400
osapi -X PUT --path _plugins/_ism/policies/my-policy \
  --body @policy.json --retry 5 --abort-on 400

# Read the body from stdin
echo '{"query":{"match_all":{}}}' | osapi -X POST --path my-index/_search -d @-

# Scaffold a request body for an endpoint, then fill it in
osapi -X POST --path _search --body-skeleton

# Retry until the cluster reports green, judging success from the body instead of the status
osapi --path _cluster/health --retry -1 --success-when '.status == "green"'

# Poll a long-running task: a 200 with "completed": false is a failure indicator, so keep retrying
osapi --path _tasks/oTUltX4IQMOUUVeiohTt8A:12345 --retry -1 --retry-when '.completed == false'

# Run an ordered sequence of calls defined in a YAML runbook
osapi --endpoint https://localhost:9200 -k -u admin run runbook.yaml
```

### Flags

| Flag                 | Default   | Description                                                        |
| -------------------- | --------- | ----------------------------------------------------------------- |
| `--endpoint`         |           | OpenSearch endpoint URL, e.g. `https://localhost:9200`            |
| `-u, --username`     |           | username for basic authentication                                 |
| `--password`         |           | password for basic authentication (see the caveat below)          |
| `--ca-cert`          |           | path to a CA certificate bundle (PEM) used to verify TLS          |
| `-k, --insecure`     |           | skip TLS certificate verification                                 |
| `--env-file`         |           | path to a dotenv file providing the environment variables below   |
| `-v, --verbose`      |           | print per-attempt retry detail to stderr                          |
| `-X, --method`       | `GET`     | HTTP method                                                       |
| `--path`             | required  | request path, e.g. `_cluster/health`                             |
| `-d, --body`         |           | request body: literal string, `@file`, or `@-` for stdin         |
| `--body-skeleton`    |           | print a JSON request-body template for `--path`/`-X` and exit     |
| `-q, --query`        |           | query parameter as `key=value` (repeatable)                      |
| `-H, --header`       |           | request header as `"Key: Value"` (repeatable)                    |
| `--retry`            | `0`       | number of retries (`0` = none; `-1` = unlimited)                 |
| `--abort-on`         |           | status codes that stop retrying (comma-separated)                |
| `--retry-when`       |           | jq expression against the JSON body; truthy forces a retry even on `2xx` |
| `--success-when`     |           | jq expression; success only when truthy, regardless of status    |
| `--max-body-buffer`  | `10MiB`   | max body buffered for `--retry-when`/`--success-when` (`0` = unlimited) |
| `--backoff`          | `linear`  | backoff strategy: `constant`, `linear`, or `exponential`         |
| `--backoff-initial`  | `2s`      | initial backoff delay                                            |
| `--backoff-max`      | `30s`     | maximum backoff delay                                            |
| `--backoff-jitter`   | `0`       | backoff jitter as a fraction in `[0,1)`                          |

## Shell completion

`osapi` ships completion driven by a pinned OpenSearch OpenAPI spec (compiled into the binary — no
runtime spec parsing). Load it for your shell:

```sh
source <(osapi completion bash)   # or: zsh, fish
```

- `--path` completes the documented REST surface, one path segment at a time, and is narrowed by
  the method when `-X` is set. Path parameters surface as literal hints (e.g. `{index}`) — real
  index/policy names are not looked up.
- `--method` completes the verbs valid for the typed `--path`.

`--body-skeleton` uses the same spec to print a typed, top-level JSON body template for the chosen
`--path`/`-X` (object bodies only; nested fields are left empty). Run `make update-spec` to refresh
the vendored spec.

## Configuration precedence

Each connection setting is resolved as:

**explicit flag > `--env-file` > process environment > default**

Recognised environment variables (also valid inside an `--env-file` dotenv file):

| Setting  | Variable              |
| -------- | --------------------- |
| endpoint | `OPENSEARCH_URL`      |
| username | `OPENSEARCH_USERNAME` |
| password | `OPENSEARCH_PASSWORD` |

Values in an `--env-file` take precedence over the process environment, so a file can override
whatever is exported in the shell.

## Retry model

- `--retry N` performs `1 + N` attempts. `--retry 0` (the default) makes a single attempt with no
  retry; `--retry -1` retries until the request is classified a success or hits an `--abort-on`
  status.
- Each attempt is classified in this order: a transport error always retries; a non-`2xx` status
  listed in `--abort-on` is a terminal failure (this wins over `--retry-when`/`--success-when`); a
  truthy `--retry-when` forces a retry even on a `2xx`; a configured `--success-when` then decides
  success or retry purely on its own truthiness, **regardless of status** — a truthy
  `--success-when` on a `503` is a success, and a falsy one on a `200` is a retry; with neither
  predicate set, any `2xx` is a success and everything else retries.
- `--retry-when`/`--success-when` are `jq` expressions evaluated against the parsed JSON response
  body. Truthiness follows `jq`: every value is truthy except `null` and `false` (so `0`, `""`,
  `[]`, and `{}` all count as truthy).
- Evaluating either predicate requires buffering the response body, up to `--max-body-buffer`
  (default `10MiB`; `0` means unlimited). A body over that cap, or one that is empty or not valid
  JSON, skips predicate evaluation for that attempt (with a warning on stderr, printed even without
  `-v`) and is treated as `--retry-when` not matched / `--success-when` not satisfied; buffering
  never truncates what reaches stdout — the final attempt's body prints in full even when over-cap.
- On any non-success outcome the exit code is `1`; a Ctrl-C (interrupt) exits with `130`. Once
  retries are exhausted, the stderr error names the deciding reason when one applies, e.g.
  `retries exhausted: --success-when not satisfied`.
- The response body is **always** printed to stdout, including for failing responses, so you can
  inspect `4xx`/`5xx` payloads (or a body a predicate rejected).

## Run files (osapi run)

`osapi run <file.yaml>` executes an ordered sequence of calls against a single OpenSearch endpoint,
defined in a YAML file. Connection flags/env work exactly as in single-request mode; the top-level
`--retry`/`--backoff-*`/etc. flags do not apply here — each call gets its retry behaviour from its
own YAML keys (defaulted the same way the flags default when a key is omitted).

```sh
osapi --endpoint https://localhost:9200 -k -u admin run runbook.yaml
```

### Schema

Every entry under the top-level `calls:` mapping is a call name mapped to a spec of these keys:

| Key               | Type                     | Default  | Description                                                        |
| ----------------- | ------------------------ | -------- | ------------------------------------------------------------------- |
| `path`            | string                   | required | request path, e.g. `_cluster/health`                               |
| `method`          | string                   | `GET`    | HTTP method                                                         |
| `body`            | string                   | none     | request body: literal string or `@file` (see below; `@-` stdin is not supported) |
| `query`           | map of string to string  | none     | query parameters                                                    |
| `headers`         | map of string to string  | none     | request headers                                                     |
| `retry`           | int                      | `0`      | number of retries (`0` = none, `-1` = unlimited)                    |
| `backoff`         | string                   | `linear` | backoff strategy: `constant`, `linear`, or `exponential`            |
| `backoff-initial` | duration string          | `2s`     | initial backoff delay                                               |
| `backoff-max`     | duration string          | `30s`    | maximum backoff delay                                               |
| `backoff-jitter`  | float                    | `0`      | backoff jitter as a fraction in `[0,1)`                             |
| `abort-on`        | list of int              | none     | status codes that stop retrying                                     |
| `retry-when`      | string (jq expression)   | none     | truthy forces a retry even on a `2xx`                               |
| `success-when`    | string (jq expression)   | none     | success only when truthy, regardless of status; mutually exclusive with `verify-with` |
| `verify-with`     | string                   | none     | name of another call in the same file, used as a nested success check; mutually exclusive with `success-when` |
| `depends-on`      | string or list of string | none     | prerequisite call name(s); must be defined earlier in the file      |
| `stop-on-failure` | bool                     | `false`  | on failure, skip every remaining call instead of continuing         |
| `max-body-buffer` | size string              | `10MiB`  | max body buffered for `retry-when`/`success-when` evaluation (`0` = unlimited)               |

### Execution model

- Calls run in document order. A call that is never anyone's own step — only ever used as another
  call's `verify-with` target — is "check-only": it never runs on its own, only when invoked as a
  nested check, and it is excluded from the run's summary counts.
- Continuing on failure is the default: a failed call is recorded and the run moves on to the next
  one. Set `stop-on-failure: true` on a call to instead skip every remaining call once it fails.
- A call whose `depends-on` prerequisite did not succeed is skipped instead of run, and that skip
  cascades to its own dependents in turn.
- The run prints a final summary line to stderr, e.g. `run: 4 succeeded, 0 failed, 0 skipped`
  (check-only calls are not counted). The process exits non-zero iff any call failed.

### depends-on

`depends-on` names one or more earlier calls that must have already succeeded. Targets must be
defined earlier in the file (forward references are rejected at load time) and must be entry calls,
not check-only `verify-with` targets. When a prerequisite has not succeeded — because it failed or
was itself skipped — the dependent call is skipped (`call "<name>": skipped (needs <prereq>)`), and
that skip cascades to whatever depends on it.

### verify-with

`verify-with: <other-call>` turns `<other-call>` into a nested success check for the call that
references it:

- On every attempt that gets a `2xx` response (and isn't already forced into a retry by a truthy
  `retry-when`), the outer call doesn't succeed immediately — instead the check is run (with its
  own retry policy), and only a successful check makes the outer attempt a success. A non-`2xx`
  outer response is classified without ever running the check.
- The check has its own retry budget, isolated from the outer call's. If the check's own retries are
  exhausted ("not yet done"), the outer attempt just counts as a retry under its own policy. A check
  with `retry: -1` therefore blocks: the outer attempt won't return until the check itself succeeds
  or hits its own `abort-on`, since the check runs synchronously inside the outer attempt.
- Any other check failure — its own `abort-on` firing, a malformed request — makes the outer call
  terminal immediately, without waiting on the outer call's own retry budget: a nested `abort-on` in
  effect terminates the outer call too.
- A check-only call cannot itself have `depends-on` or `stop-on-failure`. Chains of checks (`a`
  verifies via `b`, which verifies via `c`) are allowed; cycles are rejected at load time.

### --dry-run

`osapi run --dry-run <file.yaml>` prints the execution plan — each entry call in document order,
with its method, path, and `depends-on`/`verify-with` wiring — to stderr and exits `0` without
resolving connection settings, prompting for a password, or making any request.

### stdout/stderr

As in single-request mode, progress goes to **stderr**: one outcome line per call/check invocation
(plus per-attempt lines with `-v`) and the final summary. Unlike single-request mode, **stdout is currently always empty** — it's reserved for
a future `--output` flag that would print structured per-call results.

### @file bodies

A relative `@file` in a call's `body` resolves against the **runbook file's own directory**, so a
runbook and the body files it references stay portable together regardless of where you run
`osapi` from. Absolute paths are used as given. (This differs from `-d @file` in single-request
mode, which resolves against the process's current working directory.)

### Example

```yaml
calls:
  create_index:
    method: PUT
    path: my-index
    body: '{"settings":{"number_of_replicas":1}}'
    success-when: '.acknowledged'
    stop-on-failure: true

  wait_for_ism:
    method: GET
    path: _plugins/_ism/explain/my-index
    depends-on: create_index
    retry: -1
    abort-on: [404]
    verify-with: verify_replicas

  verify_replicas:
    method: GET
    path: my-index/_settings
    retry: -1
    success-when: '.["my-index"].settings.index.number_of_replicas == "1"'
```

`create_index` runs first and stops the whole run if it fails. `wait_for_ism` only runs once
`create_index` has succeeded, polls the ISM explain endpoint indefinitely (aborting immediately on
a `404` — the index is gone), and on every `2xx` defers to `verify_replicas` — itself polling
indefinitely — before counting as done. `verify_replicas` is check-only: it never appears as its
own step, only as `wait_for_ism`'s nested check.

### Not in v1

- Templating or variable substitution inside a runbook.
- A file-level `defaults:` block shared across calls (each call defaults independently, from the
  same baseline the flags use).
- A per-call `timeout` key.
- Stdin bodies (`@-`) — a runbook has no interactive stdin to read from.
- Capturing a response from one call for use in another.
- Combining `success-when` and `verify-with` on the same call.

## Passwords

Prefer `OPENSEARCH_PASSWORD`, an `--env-file`, or the interactive masked prompt over `--password`.
A password passed on the command line is visible in the process list (`ps`) and your shell history.
When a username is set on an interactive terminal and no password is supplied, `osapi` prompts for
one; on a non-interactive terminal it fails instead of hanging.

## Caveats

- `-k, --insecure` disables TLS certificate verification entirely — use it only against hosts you
  trust.
- `--query` and `--header` values are sent as given and are **not** redacted in verbose output, so
  avoid placing secrets in them.

## Development

```sh
make test   # go test ./...
make lint   # golangci-lint run
make build  # build the osapi binary
make e2e    # end-to-end suite against a real OpenSearch (needs Docker); see e2e/README.md
```

## License

[Apache License 2.0](LICENSE).
