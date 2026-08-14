[← back to README](../README.md)

# Run files (`osapi run`)

`osapi run <file.yaml>` executes a declarative YAML runbook: a sequence of
OpenSearch calls, run in document order. Connection settings resolve the same
way as for a single request — see [Command reference](cli.md).

Runnable examples live in [`examples/`](../examples/). Start with
[`examples/index-lifecycle.yaml`](../examples/index-lifecycle.yaml):

```yaml
# Create an index, load one document, and update it with optimistic
# concurrency. Shows: an idempotent delete that accepts a 404, a body from
# @file, capture, and a tolerated failure (continue-on-failure).
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

`index-settings.json` sits next to the runbook file. Run it with
`osapi run index-lifecycle.yaml`. Add `--dry-run` to validate the file and
print the plan without sending anything.

## Schema

Every call is a mapping of these keys. `defaults:` is an optional top-level
mapping applied to every call before its own keys are read.

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
| `continue-on-failure`  |          | do not halt the run if this call fails; the failure is reported as tolerated |
| `max-body-buffer`      |          | max body buffered for predicate/capture evaluation (`0` = unlimited) |

Retry classification (`retry-when`, `success-when`, `abort-on`) works exactly
as for a single request — see [Retry and classification](retry.md).

Durations (`backoff-initial`, `backoff-max`) and sizes (`max-body-buffer`) are
YAML strings, parsed the same way as their flag equivalents: `'2s'`, `'1MiB'`.

## `defaults:` layering

`defaults:` accepts every key above except `name`, `path`, and `capture` —
those describe one specific call, not a shared policy. Three rules decide what
a call inherits:

1. **A present key always overrides, even with a zero value.** Only an omitted
   key inherits.

   ```yaml
   defaults:
     retry: 3
   calls:
     - name: no_retry
       path: /_cluster/health
       retry: 0        # runs with zero retries, not 3
   ```

2. **Lists replace wholesale.** A call's own `abort-on` discards the inherited
   list. The two lists never merge.

   ```yaml
   defaults:
     abort-on: [400, 401]
   calls:
     - name: own_list
       path: /my-index
       abort-on: [404]   # effective list: [404] — 400 and 401 are gone
   ```

3. **`query` and `headers` maps merge key by key.** The call's own value wins
   on a collision. Header keys merge canonically, so `content-type` in
   `defaults:` and `Content-Type` on a call are one key, not two.

   ```yaml
   defaults:
     headers:
       content-type: application/json
   calls:
     - name: override_header
       path: /my-index/_doc/1
       headers:
         Content-Type: application/x-ndjson   # wins; only one header is sent
   ```

## Execution model

Calls run strictly in document order. The first failing call halts the run.
Every call after it is reported as not run, and `osapi run` exits non-zero.

A call with `continue-on-failure: true` does not halt the run. Its failure is
reported as tolerated, and the next call still runs. A run that finishes with
only tolerated failures exits `0`.

A SIGTERM or Ctrl-C while a call is in flight interrupts the run. `osapi`
prints a summary naming the in-flight call and the not-run count, then exits
`130` — the same as any other `osapi` invocation.

There is no built-in time bound on a run. `retry: -1` retries a call forever
until it succeeds or the process receives SIGTERM. There is no `--timeout`
equivalent. The summary line still prints on that path, so an operator
watching stderr sees exactly what completed before the interrupt.

## Capture and `${name}`

A call's `capture:` maps a name to a jq expression, evaluated against that
call's successful response body. Only a scalar result (string, number, or
boolean) can be captured. An object, an array, or `null` fails the call.

Every `${name}` reference is validated when the runbook loads:

- It must name a capture declared by an **earlier** call in the document. A
  forward or self-reference is a load error.
- A reference to a capture declared by a `continue-on-failure: true` call is
  a load error too: a tolerated failure could leave it unset.
- `${...}` is **not** supported in query or header *keys* — only in values. A
  key containing one is a load error, not a literal shipped to the server.

A resolved value is inserted **verbatim**. Nothing is auto-quoted or
auto-escaped, so producing valid JSON (or whatever the field expects) is the
runbook author's job. `${name}` resolves against captures **only**: no
environment variables, no CLI variables, nothing else.

The likeliest question this raises: **the captured value is not there yet.**
There is no separate polling mechanism. Instead, put `success-when` on the
call that produces the capture, paired with a retry budget. For example,
`success-when: '._seq_no != null'` with `retry: -1` retries the call until the
field appears. Only then does its capture run.

Two jq idioms answer most escaping and scalar-restriction questions:

- `'.reason | @json'` renders a string that already carries its own quotes and
  escapes. Write the placeholder **unquoted** in the body:
  `body: '{"msg":${reason}}'` is valid JSON whatever `.reason` contains —
  quotes, newlines, anything.
- `'._source | tojson'` turns a captured object or array into a single scalar
  string of raw JSON. Verbatim insertion then splices that JSON in unchanged,
  so `body: '{"doc":${src}}'` embeds the whole object as-is.

Two limits are real and were found in testing:

1. A value substituted into `path` is rejected if it contains `/`, `?`, `#`,
   or `%`. A captured value could otherwise redirect the call to a different
   endpoint or inject query parameters. After substitution, a path that gained
   a `.` or `..` segment is rejected for the same reason. `body`, `query`, and
   header substitution stay verbatim — only `path` is restricted.
2. There is no way to write a literal `$` immediately followed by a reference.
   `$${` escapes a literal `${`, so `$$${x}` renders as `$${x}`: the `$${`
   escape is consumed before the reference is ever seen.

A `${}` placeholder inside an `@file` body works exactly like a literal one.
Left unquoted, it makes the file itself invalid JSON on disk
(`{"n":${count}}` is not valid JSON by itself). A quoted placeholder
(`{"n":"${count}"}`) keeps the file valid JSON at rest, at the cost of forcing
the substituted value to render as a string.

`-v` prints every captured value as `name=value` on stderr. These values come
from the API response at runtime, not from a flag or environment variable, so
CI secret-masking — which typically redacts known variable names — does
**not** cover them. Do not capture and log a value you would not want in a CI
log.

## Accepting a non-2xx status

A call with `success-when` succeeds only when the expression is truthy,
regardless of HTTP status — so a `404` can be an accepted outcome. This is the
idiom for an idempotent delete at the start of a runbook:

```yaml
success-when: '.acknowledged == true or .status == 404'
```

Both "the index existed and was deleted" and "the index was already gone"
count as success, so the runbook can run repeatedly.

## `--dry-run`

`--dry-run` validates the runbook and prints its plan without sending any
request. It needs no endpoint and no credentials. Against
[`examples/index-lifecycle.yaml`](../examples/index-lifecycle.yaml) it prints:

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

Headers are deliberately omitted from the plan. Printing them would put an
`Authorization` value into stderr and CI logs.

## Output

stdout is always empty (reserved for a future `--output` flag). Progress —
one line per call — a failing call's body, and any warnings all go to stderr.
A failing call's body is echoed indented by two spaces, bounded by that call's
own `max-body-buffer`, with control characters stripped so the endpoint's
response can never overwrite the lines above it.

## Precedence

Connection settings (`--endpoint`, `-u`, `--password`, `--ca-cert`,
`--env-file`, `-k`) resolve exactly as for a single request — see
[Configuration precedence](cli.md#configuration-precedence). Retry settings
(`retry`, `backoff`, `backoff-initial`, ...) come from the YAML alone: `run`
has no `--retry`/`--backoff` flags, and passing one is an "unknown flag"
error.

A call's `@file` body resolves relative to the runbook file's own directory
(an absolute path is used as given). A runbook and its payload files move
together as a unit, regardless of the working directory `osapi run` is
invoked from.

Connection flags work on either side of the subcommand:
`osapi --endpoint ... run deploy.yaml` and `osapi run deploy.yaml --endpoint ...`
are equivalent. `--dry-run` is a flag of `run` itself, so it must come *after*
the subcommand.

## Not yet supported

- a per-call `timeout:`
- per-call identity (`credentials:`/`as:`)
- general variable substitution beyond captures
- a stdin body
- `--output` for saving response bodies

## See also

- [Retry and classification](retry.md) — how each attempt is judged
- [Command reference](cli.md) — flags, configuration, credentials
- [`examples/`](../examples/) — runnable runbooks
