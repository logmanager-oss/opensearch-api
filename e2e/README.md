# e2e stack

A disposable, security-enabled, single-node OpenSearch container used to run
this repo's end-to-end tests against a real cluster over HTTPS.

## Usage

```sh
make e2e-up    # tear down any previous stack, regenerate certs, start fresh
make e2e-down  # tear down the stack
```

Overrides:

- `E2E_OPENSEARCH_IMAGE` - OpenSearch image to run (default `opensearchproject/opensearch:2.19.4`)
- `E2E_OPENSEARCH_PORT` - host port the cluster is published on (default `19200`)

## Credentials

All credentials below are synthetic test fixtures for this throwaway
container only - they are not valid anywhere else.

| user        | password                    | access level                                    |
|-------------|-----------------------------|--------------------------------------------------|
| `admin`     | `E2e-Test-Only-Admin-Pw!`   | full access (`all_access`)                        |
| `ci_reader` | `E2e-Test-Only-Reader-Pw!`  | read-only (`ci_read_only`: read + cluster monitor)|
| `ci_locked` | `E2e-Test-Only-Locked-Pw!`  | authenticates, no permissions (403 everywhere)    |

## Regenerating password hashes

`internal_users.yml` stores bcrypt hashes computed with the image's own hash
tool. To regenerate a hash for a given password:

```sh
docker run --rm opensearchproject/opensearch:2.19.4 \
  plugins/opensearch-security/tools/hash.sh -p '<password>'
```

Paste the resulting `$2y$...` hash into `securityconfig/internal_users.yml`.
