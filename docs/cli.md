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

These flags work on the root command and on `run`, on either side of the
subcommand.

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
