# opensearch-api

`osapi` is a small, general-purpose CLI for talking to any OpenSearch REST endpoint with robust
 retry behaviour.

It behaves like a resilient `curl` for OpenSearch: bounded or unlimited retry with configurable
backoff, optional insecure-TLS, and status-code-based success/terminal classification — in a single
binary that can reach any endpoint. The response body is written to **stdout** (pipe it to `jq`);
diagnostics and per-attempt retry detail go to **stderr**.

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

`osapi` sends one request per invocation — the command itself is the request
(there is no `request` subcommand). `osapi --version` prints the version.

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
  retry; `--retry -1` retries until the request succeeds or hits an `--abort-on` status.
- Any `2xx` response is a success: the body is printed to stdout and the exit code is `0`.
- A status listed in `--abort-on` stops retrying immediately (terminal failure).
- Any other non-`2xx` response, or a transport error, is retried until the retry budget is
  exhausted.
- On any non-success outcome the exit code is `1`; a Ctrl-C (interrupt) exits with `130`.
- The response body is **always** printed to stdout, including for failing responses, so you can
  inspect `4xx`/`5xx` payloads.

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
```

## License

[Apache License 2.0](LICENSE).
