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

`osapi` sends one request per invocation — the command itself is the request. The `run`
subcommand is the exception: it executes a declarative, multi-call YAML runbook instead (see
[Run files](#run-files-osapi-run) below). `osapi --version` prints the version.

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

# Run a declarative, multi-call runbook (see Run files below)
osapi run deploy.yaml
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

`osapi run <file.yaml>` executes a declarative YAML "runbook": a sequence of OpenSearch calls,
run in document order, against the same connection settings as every other subcommand.

```yaml
defaults:
  retry: 3
  backoff: exponential

calls:
  - name: drop_stale_index
    method: DELETE
    path: /my-index
    success-when: '.acknowledged == true or .status == 404'

  - name: create_index
    method: PUT
    path: /my-index
    body: '@index-settings.json'
    success-when: '.acknowledged'

  - name: index_doc
    method: PUT
    path: /my-index/_doc/1
    body: '{"field":"initial"}'

  - name: get_doc
    method: GET
    path: /my-index/_doc/1
    capture:
      seq: '._seq_no'
      term: '._primary_term'

  - name: update_doc
    method: PUT
    path: /my-index/_doc/1
    query:
      if_seq_no: '${seq}'
      if_primary_term: '${term}'
    body: '{"field":"updated"}'

  - name: warm_caches
    method: POST
    path: /my-index/_forcemerge
    continue-on-failure: true
```

`index-settings.json` sits next to the runbook file; run it with `osapi run deploy.yaml` (add
`--dry-run` to validate it and print the plan without sending anything).

### Schema

Every call is a mapping of these keys; `defaults:` is an optional top-level mapping applied to
every call before its own keys are read.

| Key                   | Required | Description                                                          |
| ---------------------- | -------- | --------------------------------------------------------------------- |
| `name`                 | yes      | unique call identifier, used in progress lines, errors, and the dry-run plan |
| `path`                 | yes      | request path, e.g. `_cluster/health` — same as `--path`              |
| `method`               |          | HTTP method (default `GET`)                                          |
| `body`                 |          | request body: literal string or `@file` (resolved next to the runbook) |
| `query`                |          | mapping of query parameters                                          |
| `headers`              |          | mapping of request headers                                           |
| `retry`                |          | number of retries (`0` = none, `-1` = unlimited) — same as `--retry` |
| `backoff`              |          | backoff strategy: `constant`, `linear`, or `exponential`             |
| `backoff-initial`      |          | initial backoff delay, e.g. `'2s'`                                   |
| `backoff-max`          |          | maximum backoff delay                                                |
| `backoff-jitter`       |          | backoff jitter as a fraction in `[0,1)`                              |
| `abort-on`             |          | status codes that stop retrying (list)                               |
| `retry-when`           |          | jq expression; truthy forces a retry even on a `2xx`                 |
| `success-when`         |          | jq expression; success only when truthy, regardless of status        |
| `capture`              |          | mapping of `name: jq-expression`, extracted from a successful response |
| `continue-on-failure`  |          | don't halt the run if this call fails; its failure is reported as tolerated |
| `max-body-buffer`      |          | max body buffered for predicate/capture evaluation (`0` = unlimited) |

`defaults:` accepts every key above except `name`, `path`, and `capture` — those describe one
specific call, not a policy to share. A call's own key always wins over the inherited one. List
values such as `abort-on` are **replaced** wholesale by a call's own list, never merged with the
inherited one; `query` and `headers` maps **merge** key-by-key instead, with the call's own value
winning per key on a collision. A key that is present on a call always overrides the inherited
value, even when it is zero: `retry: 0` under a `defaults` of `retry: 3` runs with zero retries.
Only an omitted key inherits.

### Execution model

Calls run strictly in document order. The first call to fail halts the run: every call after it is
reported as not run, and `osapi run` exits non-zero. A call with `continue-on-failure: true` does
not halt the run on failure — its failure is reported as tolerated, and the next call still runs.
The run's exit code is non-zero exactly when it halted; a run that finishes with only tolerated
failures still exits `0`. A SIGTERM or Ctrl-C while a call is in flight interrupts the run, prints a
summary naming the in-flight call and the not-run count, and exits `130` — the same as any other
`osapi` invocation.

A call's `method:` defaults to `GET`. Durations (`backoff-initial`, `backoff-max`) and sizes
(`max-body-buffer`) are YAML strings, parsed the same way as their flag equivalents — e.g. `'2s'`,
`'1MiB'`.

### Capture and `${name}`

A call's `capture:` maps a name to a jq expression evaluated against that call's successful
response body. Only a scalar result (string, number, or boolean) can be captured — an object, an
array, or `null` fails the call.

Every `${name}` reference is validated when the runbook loads: it must name a capture declared by
an earlier call in the document. A forward or self-reference is a load error, and so is a
reference to a capture declared by a `continue-on-failure: true` call, since a tolerated failure
could leave it unset. `$${` escapes a literal `${` without triggering substitution. A resolved
value is inserted **verbatim** — nothing is auto-quoted or auto-escaped, so producing valid
JSON (or whatever the field expects) is the runbook author's job. `${name}` resolves against
captures **only**: no environment variables, no CLI variables, nothing else.

The likeliest question this raises: **the captured value isn't there yet.** Rather than a separate
polling mechanism, put `success-when` on the same call that produces the capture, paired with a
retry budget (`retry: -1` to wait as long as it takes, or a bounded count), e.g.
`success-when: '._seq_no != null'` with `retry: -1` — the call retries until the field appears, and
only then does its capture run.

Two jq idioms answer most escaping and scalar-restriction questions in one shot:

- `'.reason | @json'` renders a string that already carries its own quotes and escapes. Write the
  placeholder **unquoted** in the body: `body: '{"msg":${reason}}'` is valid JSON whatever
  `.reason` contains — quotes, newlines, anything.
- `'._source | tojson'` turns a captured object or array into a single scalar string of raw JSON.
  Verbatim insertion then splices that JSON in unchanged, so `body: '{"doc":${src}}'` embeds the
  whole object as-is.

Two limits are worth knowing because they are real and were found in testing:

1. A value substituted into `path` is rejected if it contains `/`, `?`, `#`, or `%` — a captured
   value could otherwise redirect the call to a different endpoint or inject query parameters.
   `body`, `query`, and header substitution stay verbatim; only `path` is restricted.
2. There is no way to write a literal `$` immediately followed by a reference: `$$${x}` renders as
   `$${x}`, because `$$` (the escape) and the `${` that follows it are consumed together as one
   `$${` escape sequence before the reference is ever seen.

`${...}` is **not** supported in query or header *keys* — only in values. A key containing one is a
load error, not a literal shipped to the server.

A `${}` placeholder inside an `@file` body works exactly like a literal one, but if left unquoted
it makes the file itself invalid JSON on disk (`{"n":${count}}` is not valid JSON by itself). A
quoted placeholder (`{"n":"${count}"}`) keeps the file valid JSON at rest, at the cost of forcing
the substituted value to render as a string.

`-v` prints every captured value as `name=value` on stderr. Because these values come from the API
response at runtime rather than from a flag or environment variable, CI secret-masking — which
typically redacts known variable names — does **not** cover them; don't capture and log a value you
wouldn't want sitting in a CI log.

### Accepting a non-2xx status

A call succeeds only when its `success-when` is truthy, regardless of HTTP status — so a `404` can
be an accepted outcome. This is the idiom for an idempotent delete at the start of a runbook:
`success-when: '.acknowledged == true or .status == 404'` treats both "the index existed and was
deleted" and "the index was already gone" as success, so the runbook can be run repeatedly.

### `--dry-run`

`--dry-run` validates the runbook and prints its plan without sending any request. Running it
against the example above prints:

```
dry-run: 6 call(s), no requests sent
  1. drop_stale_index: DELETE /my-index
     retry: 3 (exponential)
  2. create_index: PUT /my-index
     body: 38 bytes
     retry: 3 (exponential)
  3. index_doc: PUT /my-index/_doc/1
     body: 19 bytes
     retry: 3 (exponential)
  4. get_doc: GET /my-index/_doc/1
     retry: 3 (exponential)
     produces: seq, term
  5. update_doc: PUT /my-index/_doc/1
     body: 19 bytes
     retry: 3 (exponential)
     consumes: ${seq}, ${term}
  6. warm_caches: POST /my-index/_forcemerge (continue-on-failure)
     retry: 3 (exponential)
```

Headers are deliberately omitted from the plan — printing them would put an `Authorization` value
into stderr and CI logs.

### Output

stdout is always empty (reserved for a future `--output` flag). Progress — one line per call — a
failing call's body, and any warnings all go to stderr. A failing call's body is echoed indented by
two spaces, bounded by that call's own `max-body-buffer`, with control characters stripped so the
endpoint's response can never overwrite the lines above it.

### Precedence

Connection settings (`--endpoint`, `-u`, `--password`, `--ca-cert`, `--env-file`, `-k`) resolve
exactly as for every other subcommand — see [Configuration precedence](#configuration-precedence)
above. Retry semantics (`retry`, `backoff`, `backoff-initial`, ...), however, come from the YAML
alone: `run` has no `--retry`/`--backoff`/etc. flags, and passing one is an "unknown flag" error.

A call's `@file` body resolves relative to the runbook file's own directory (an absolute path is
used as given), so a runbook and its payload files can be moved together as a unit regardless of
the working directory `osapi run` is invoked from.

Connection flags work on either side of the subcommand: `osapi --endpoint ... run deploy.yaml` and
`osapi run deploy.yaml --endpoint ...` are equivalent. `--dry-run` is a flag of `run` itself, so it
must come *after* the subcommand.

There is no built-in time bound on a run. `retry: -1` retries a call forever until it succeeds or
the process receives SIGTERM — there is no `--timeout` equivalent. The summary line still prints on
that path, so an operator watching stderr sees exactly what completed before the interrupt.

Not yet supported: a per-call `timeout:`, per-call identity (`credentials:`/`as:`), general
variable substitution/templating beyond captures, a stdin body, and `--output` for saving response
bodies.

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
