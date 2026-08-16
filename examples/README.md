# Runbook examples

Runnable runbooks for `osapi run`. Each file's header comment names the
feature it shows.

| File | Shows |
| ---- | ----- |
| [`index-lifecycle.yaml`](index-lifecycle.yaml) | the everyday shape: idempotent delete via `success-when`, body from [`index-settings.json`](index-settings.json), capture, `continue-on-failure` |
| [`reindex-and-wait.yaml`](reindex-and-wait.yaml) | capture a task id, then poll it — capture into a request path, `retry-when` on a 200 |
| [`optimistic-update.yaml`](optimistic-update.yaml) | capture `_seq_no`/`_primary_term`, wait for them with `success-when` + retry, use them as query values |
| [`wait-for-green.yaml`](wait-for-green.yaml) | gate an action on cluster health — the runbook replacement for a `sleep` |
| [`ism-policy.yaml`](ism-policy.yaml) | `defaults:` layering, including a `retry: 0` override of an inherited `retry: 3` |
| [`with-credentials.yaml`](with-credentials.yaml) | `defaults: credentials:` — authenticate with a runbook-defined username and password |

Validate a file without sending anything (needs no endpoint or credentials):

```sh
osapi run --dry-run index-lifecycle.yaml
```

Run it for real:

```sh
osapi --endpoint https://localhost:9200 -u admin run index-lifecycle.yaml
```

**Warning:** these runbooks create and delete indices (`my-index`,
`old-index`, `new-index`). Do not point them at a cluster you care about.

See [docs/runbooks.md](../docs/runbooks.md) for the full run-file reference.
