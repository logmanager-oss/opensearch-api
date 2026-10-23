[← back to README](../README.md)

# Command reference

`osapi` sends one request per invocation — the command itself is the request.
The response body is written to **stdout** (pipe it to `jq`). Diagnostics and
per-attempt retry detail go to **stderr**. `osapi --version` prints the
version.

The `run` subcommand is the exception: it executes a declarative multi-call
YAML runbook instead — see [Run files](runbooks.md).

## Flags

### Connection

These flags work on the root command, on `run` and on `ism`, on either side of
the subcommand.

| Flag             | Default | Description                                                      |
| ---------------- | ------- | ---------------------------------------------------------------- |
| `--endpoint`     | `https://localhost:9200` | OpenSearch endpoint URL                                 |
| `-u, --username` |         | username for basic authentication                                |
| `--password`     |         | password for basic authentication (see [Credentials and TLS](#credentials-and-tls)) |
| `--ca-cert`      |         | path to a CA certificate bundle (PEM) used to verify TLS         |
| `-k, --insecure` |         | skip TLS certificate verification                                |
| `--env-file`     |         | path to a dotenv file providing the environment variables below  |
| `-v, --verbose`  |         | print per-attempt retry detail to stderr                         |
| `--timeout`      |         | bound the whole invocation, retries included, e.g. `30s` (0 = no bound). The clock starts at the first request |

### Request

| Flag              | Default  | Description                                                  |
| ----------------- | -------- | ------------------------------------------------------------ |
| `-X, --method`    | `GET`    | HTTP method                                                  |
| `--path`          | required | request path, e.g. `_cluster/health`                         |
| `-d, --body`      |          | request body: literal string, `@file`, or `@-` for stdin     |
| `--body-skeleton` |          | print a JSON request-body template for `--path`/`-X` and exit |
| `-q, --query`     |          | query parameter as `key=value` (repeatable)                  |
| `-H, --header`    |          | request header as `"Key: Value"` (repeatable)                |

The request body is read fully into memory, whatever its source, so retries
can replay it.

### Retry

Semantics live in [Retry and classification](retry.md).

| Flag                | Default  | Description                                                  |
| ------------------- | -------- | ------------------------------------------------------------ |
| `--retry`           | `0`      | number of retries (`0` = none; `-1` = unlimited)             |
| `--abort-on`        |          | non-2xx status codes that stop retrying (comma-separated); mutually exclusive with `--retry-on` |
| `--retry-on`        |          | non-2xx status codes that keep retrying; any other non-2xx stops (comma-separated); mutually exclusive with `--abort-on` |
| `--retry-when`      |          | jq expression; truthy forces a retry even on `2xx`           |
| `--success-when`    |          | jq expression; success only when truthy, regardless of status |
| `--max-body-buffer` | `10MiB`  | max body buffered for predicate evaluation (`0` = unlimited) |
| `--backoff`         | `linear` | backoff strategy: `constant`, `linear`, or `exponential`     |
| `--backoff-initial` | `2s`     | initial backoff delay                                        |
| `--backoff-max`     | `30s`    | maximum backoff delay                                        |
| `--backoff-jitter`  | `0`      | backoff jitter as a fraction in `[0,1)`                      |

## ISM policies (`osapi ism`)

`osapi ism` manages Index State Management policies. It has two verbs.

| Verb | Requests | stdout |
| ---- | -------- | ------ |
| `ism set <id>` | `GET _plugins/_ism/policies/<id>`, then a conditional `PUT` unless the policy is unchanged | the final response body: the `PUT`, or the `GET` when unchanged or when the `GET` fails |
| `ism reapply <id>` | `GET _plugins/_ism/explain[/<target>]?size=10000`, then `POST _plugins/_ism/change_policy/<managed indices>` with `{"policy_id":"<id>"}` | the `change_policy` body, or the `explain` body when `explain` fails |

| Flag | Verb | Default | Description |
| ---- | ---- | ------- | ----------- |
| `-d, --body` | `set` | required | policy body: literal string, `@file`, or `@-` for stdin. It needs a top-level `policy` object |
| `--target` | `set` | | index pattern for the policy's `ism_template` (repeatable). It replaces the template in the body |
| `--priority` | `set` | `0` | priority of the `ism_template` entry. It needs `--target` |
| `--force` | `set` | | send the `PUT` even when the stored policy is equal |
| `--target` | `reapply` | all managed indices | index names or patterns, comma-separated, passed through verbatim |

Every flag in the Connection and Retry tables applies to both verbs. The
request flags `--path`, `-X`, `-q`, `-H` and `--body-skeleton` do not. The
flags may sit before `ism`, between `ism` and the verb, or after the verb.
The `<id>` argument and the `reapply --target` value go into the request
path. Neither may be `.` or `..`, or contain `/`, `?`, `#` or `%`. An empty
`<id>` is an error. An empty `--target` means all managed indices.

### `ism set`

- `set` sends `{"policy": <policy>}`. It removes the fields the server adds:
  `policy_id`, `last_updated_time`, `user`, `schema_version`, and
  `last_updated_time` inside each `ism_template` entry. A `GET` body is
  therefore valid input.
- `set` sends an `ism_template` object as a one-element array.
- `--target` sets `ism_template` to `[{"index_patterns": [<targets>], "priority": <n>}]`.
- When the stored policy equals the body after the same removal, `set` sends
  no `PUT`. It prints the `GET` body on stdout and `policy <id> unchanged` on
  stderr, and exits `0`. `--force` sends the `PUT` anyway.
- The comparison can report "changed" when it is not, which costs one extra
  `PUT`. This happens when the file omits fields that ISM stores by default,
  such as per-action `retry` and `timeout`, or `error_notification`. It never
  reports "unchanged" when the policy differs.
- One attempt is the `GET` and the `PUT`. The engine never sees a `GET` `404`:
  `set` sends a plain `PUT` instead. The engine classifies every other `GET`
  status, and the `PUT` status, like a root request. `--abort-on`,
  `--retry-on`, `--success-when` and `--retry-when` therefore judge the
  response that ends the attempt.
- A `PUT` `409` is a plain non-2xx status. See
  [Retry and classification](retry.md#classification).
- On the unchanged path the final response is the `GET`. A `--success-when`
  written for the `PUT` body then judges the `GET` body.

### `ism reapply`

- `reapply` lists the managed indices with `explain`. It sends `change_policy`
  only for the indices whose `policy_id` equals `<id>`. An index that matches
  `--target` but belongs to another policy keeps its policy.
- With no managed index, `reapply` prints `no indices are managed by <id>` on
  stderr, prints nothing on stdout, and exits `0`.
- A 2xx `change_policy` body with `"failures": true` exits `0`. Add
  `--success-when '.failures == false'` to make it fail. `--success-when` and
  `--retry-when` judge only the `change_policy` response, not the `explain` call.
- `reapply` does not page. It reads at most 10000 managed indices.
- `explain` reflects ISM's own record of managed indices, which lags `add`,
  `remove` and template attachment by a few seconds. Deleting a policy does not
  detach its indices: `explain` still reports the `policy_id`, and `reapply`
  then fails with a 404 from `change_policy`. Detach with `_plugins/_ism/remove`
  first.

## Configuration precedence

Each connection setting is resolved as:

**explicit flag > `--env-file` > process environment > default**

Recognised environment variables (also valid inside an `--env-file` dotenv
file):

| Setting  | Variable              |
| -------- | --------------------- |
| endpoint | `OPENSEARCH_URL`      |
| username | `OPENSEARCH_USERNAME` |
| password | `OPENSEARCH_PASSWORD` |
| timeout  | `OSAPI_TIMEOUT`       |

Values in an `--env-file` take precedence over the process environment, so a
file can override whatever is exported in the shell.

## Credentials and TLS

Prefer `OPENSEARCH_PASSWORD`, an `--env-file`, or the interactive masked
prompt over `--password`. A password passed on the command line is visible in
the process list (`ps`) and your shell history.

- When a username is set on an interactive terminal and no password is
  supplied, `osapi` prompts for one. On a non-interactive terminal it fails
  instead of hanging.
- Basic auth is sent **only to the configured endpoint's origin** (scheme,
  host, and port). A redirect to any other origin is followed without
  credentials.
- Userinfo in the endpoint URL (`https://user:pass@host`) is stripped from
  error messages. Prefer `-u`/`--password` anyway: userinfo can surface in
  transport errors from other layers.
- `-k, --insecure` disables TLS certificate verification entirely — use it
  only against hosts you trust. `osapi` prints a warning whenever `-k` is set.
  When both `-k` and `--ca-cert` are set, `-k` wins.
- `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` are honoured with the standard Go
  semantics. Connection setup is bounded by the Go defaults (30s dial, 10s TLS
  handshake). The response itself is bounded only by `--timeout`.
- `--query` and `--header` values are sent as given and are **not** redacted
  in verbose output, so avoid placing secrets in them.

## Shell completion

`osapi` ships completion driven by a pinned OpenSearch OpenAPI spec, compiled
into the binary — no runtime spec parsing. Load it for your shell:

```sh
source <(osapi completion bash)   # or: zsh, fish
```

- `--path` completes the documented REST surface, one path segment at a time,
  narrowed by the method when `-X` is set. Path parameters surface as literal
  hints (e.g. `{index}`) — real index or policy names are not looked up.
- `--method` completes the verbs valid for the typed `--path`.

`--body-skeleton` uses the same spec to print a typed, top-level JSON body
template for the chosen `--path`/`-X` (object bodies only; nested fields are
left empty). See [architecture.md](architecture.md#updating-the-api-spec) for
how to refresh the vendored spec.

## Exit codes

| Code  | Meaning                                          |
| ----- | ------------------------------------------------ |
| `0`   | the response was classified a success            |
| `1`   | any non-success outcome                          |
| `124` | `--timeout` elapsed                              |
| `130` | interrupted (Ctrl-C or SIGTERM)                  |

The response body always prints to stdout, including for `4xx`/`5xx`
responses and for bodies a predicate rejected.

## Recipes

Cluster health, pretty-printed:

```sh
osapi --endpoint https://localhost:9200 -k -u admin --path _cluster/health | jq .
```

Wait until the cluster reports green, judging success from the body instead of
the status:

```sh
osapi --path _cluster/health --retry -1 --success-when '.status == "green"'
```

Poll a long-running task — a `200` with `"completed": false` is a failure
indicator, so keep retrying:

```sh
osapi --path _tasks/oTUltX4IQMOUUVeiohTt8A:12345 --retry -1 --retry-when '.completed == false'
```

PUT a policy from a file, retrying up to 5 times but stopping immediately on
a `400`:

```sh
osapi -X PUT --path _plugins/_ism/policies/my-policy \
  --body @policy.json --retry 5 --abort-on 400
```

Create or update an ISM policy for `logs-*`. A `409` retries, any other
non-2xx status stops:

```sh
osapi ism set my-policy -d @policy.json --target 'logs-*' --retry 5 --retry-on 409
```

Copy a policy to another name. `--target` gives the copy its own template, so
it does not collide with the source's:

```sh
osapi --path _plugins/_ism/policies/my-policy | osapi ism set my-policy-copy -d @- --target 'other-*'
```

Re-run the current policy on its indices, and fail if any index fails:

```sh
osapi ism reapply my-policy --success-when '.failures == false'
```

Read the body from stdin:

```sh
echo '{"query":{"match_all":{}}}' | osapi -X POST --path my-index/_search -d @-
```

Scaffold a request body for an endpoint, then fill it in:

```sh
osapi -X POST --path _search --body-skeleton
```

Inspect a failing response — the `4xx` body still reaches stdout, and the exit
code is `1`:

```sh
osapi --path no-such-index/_search | jq .error
```

Verify TLS against a private CA, instead of turning verification off with
`-k`:

```sh
osapi --endpoint https://opensearch.internal:9200 --ca-cert ./ca.pem --path _cluster/health
```

Keep credentials out of the process list in CI with a dotenv file:

```sh
osapi --env-file ./opensearch.env --path _cluster/health
```

Spread out retries against a busy cluster — exponential backoff with jitter so
concurrent jobs do not retry in lockstep:

```sh
osapi --path _cluster/health --retry 8 --backoff exponential --backoff-jitter 0.3
```

Raise the predicate buffer for a large response judged by `--success-when`:

```sh
osapi -X POST --path my-index/_search -d @query.json \
  --retry 3 --success-when '.hits.total.value > 0' --max-body-buffer 64MiB
```

Watch each attempt while debugging a retry loop:

```sh
osapi -v --path _cluster/health --retry 5 --success-when '.status == "green"'
```

Bound a one-shot cron call to 30 seconds, with up to 3 retries inside that
bound. The deadline wins over any remaining retries:

```sh
osapi --path _cluster/health --timeout 30s --retry 3
```

## See also

- [Retry and classification](retry.md) — how each attempt is judged
- [Run files](runbooks.md) — multi-call runbooks
- [`examples/`](../examples/) — runnable runbooks
