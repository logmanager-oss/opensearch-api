# opensearch-api

`osapi` is a small, general-purpose CLI for talking to any OpenSearch REST endpoint with robust,
fully configurable retry behaviour and named connection profiles.

It reproduces the network behaviour of the hand-rolled `curl` bootstrap helper used to configure
OpenSearch clusters (infinite/bounded retry with backoff, insecure-TLS support, status-code-based
success/terminal classification), but as a single reusable binary that can hit any endpoint.

> **Status:** early development. Phase 1 delivers the core CLI (`request` + `profile`); shell
> completion driven by the bundled OpenSearch OpenAPI spec is planned for a later phase.

## Install

```sh
go build -o osapi ./cmd/osapi
# or
make build
```

## Usage

```sh
# Cluster health, pretty-printed
osapi request --endpoint https://localhost:9200 -k -u admin --method GET --path _cluster/health | jq .

# PUT a body from a file, treating 404 as terminal (exit 1)
osapi request -X PUT --path _plugins/_ism/policies/my-policy --body @policy.json --expect-empty
```

The response body is written to **stdout** (pipeable to `jq`); human-readable messages and per-attempt
retry detail (`--verbose`) go to **stderr**. Exit code is `0` on success, `1` on terminal failure.

### Configuration precedence

Each connection setting is resolved as: **explicit flag > environment > profile > default**.

Environment resolution is on by default (disable with `--env=false`). Recognised variables:

| Setting  | Variables (first match wins)                              |
| -------- | --------------------------------------------------------- |
| endpoint | `OS_HOST`, `OPENSEARCH_URL`                               |
| username | `OSAPI_USERNAME`, `OPENSEARCH_USERNAME`                   |
| password | `OSAPI_PASSWORD`, `OPENSEARCH_PASSWORD`, `OS_<USER>_PASS` |

`OS_<USER>_PASS` derives from the username, uppercased with `-` → `_` (e.g. user `svc-loader` →
`OS_SVC_LOADER_PASS`).

### Profiles

Named profiles live in `~/.osapi/config.yaml` (override with `--config` or `OSAPI_CONFIG`).

```sh
osapi profile create --name prod --endpoint https://os:9200 --username svc-loader
osapi profile list
osapi request --profile prod --path _cluster/health
```

**Passwords are never stored on disk.** A profile holds only non-secret fields; the password is
supplied per request via `--password`, the environment, or an interactive masked prompt.

## Development

```sh
make test      # go test ./...
make lint      # golangci-lint run
make generate  # go generate ./... (mocks)
make build     # build the osapi binary
```

## License

[Apache License 2.0](LICENSE).
