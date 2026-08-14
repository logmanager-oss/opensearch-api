# opensearch-api

`osapi` is a small, general-purpose CLI for talking to any OpenSearch REST
endpoint with robust retry behaviour. It behaves like a resilient `curl` for
OpenSearch. The response body is written to **stdout** (pipe it to `jq`);
diagnostics and per-attempt retry detail go to **stderr**.

Why `osapi` over `curl` in a retry loop:

- One binary, no runtime dependency, reaches any OpenSearch REST endpoint.
- Retry that judges the response **body**, not only the status — `jq`
  predicates decide success or retry ([docs/retry.md](docs/retry.md)).
- Declarative multi-call YAML runbooks with value capture between calls
  ([docs/runbooks.md](docs/runbooks.md)).
- Shell completion and request-body scaffolds from a pinned OpenSearch
  OpenAPI spec, compiled into the binary.

## Install

```sh
# Latest release, straight from GitHub (installs onto $(go env GOPATH)/bin):
go install github.com/logmanager-oss/opensearch-api/cmd/osapi@latest

# Or from a local checkout:
go install ./cmd/osapi          # onto your PATH
go build -o osapi ./cmd/osapi   # local binary in the repo dir (or: make build)
```

Shell completion needs `osapi` on your `PATH`, so prefer `go install`. Load it
with `source <(osapi completion bash)` (or `zsh`, `fish`).

## Quickstart

```sh
# Cluster health, pretty-printed
osapi --endpoint https://localhost:9200 -k -u admin --path _cluster/health | jq .

# Retry until the cluster reports green, judging success from the body
osapi --path _cluster/health --retry -1 --success-when '.status == "green"'

# PUT a policy from a file, retrying up to 5 times but stopping immediately on 400
osapi -X PUT --path _plugins/_ism/policies/my-policy \
  --body @policy.json --retry 5 --abort-on 400

# Run a declarative, multi-call runbook
osapi run examples/index-lifecycle.yaml
```

## Documentation

| Read this | When you want |
| --------- | ------------- |
| [docs/cli.md](docs/cli.md) | the flag reference, configuration, credentials, and copy-paste recipes |
| [docs/retry.md](docs/retry.md) | how each attempt is classified: retry, success, terminal |
| [docs/runbooks.md](docs/runbooks.md) | the run-file reference: schema, defaults, capture, `--dry-run` |
| [examples/](examples/) | runnable runbooks to start from |
| [docs/architecture.md](docs/architecture.md) | how the code is structured (contributors) |

## Development

```sh
make test   # go test ./...
make lint   # golangci-lint run
make build  # build the osapi binary
make e2e    # end-to-end suite against a real OpenSearch (needs Docker); see e2e/README.md
```

See [docs/architecture.md](docs/architecture.md) for the package map and
maintenance workflows.

## License

[Apache License 2.0](LICENSE).
